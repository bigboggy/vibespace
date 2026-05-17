//go:build !linux || cgo

// Real audio player. Compiled everywhere except Linux without CGO — the
// server cross-build (linux/amd64, CGO_ENABLED=0) gets the stub instead,
// since oto's ALSA driver needs CGO. macOS and Windows ship without CGO
// because oto uses purego there.

package radio

import (
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/effects"
	"github.com/gopxl/beep/v2/mp3"
	"github.com/gopxl/beep/v2/speaker"
)

// realPlayer is the CGO-backed Player using beep + oto (via the speaker
// package). It owns the currently-loaded track and the volume / pause effects
// stacked on top of it.
//
// State machine:
//
//	closed (no file)     —Play(p)→    playing
//	playing              —PauseToggle→ paused
//	paused               —PauseToggle→ playing
//	{playing,paused}     —Play(p2)→   playing (replaces)
//	{playing,paused}     —Stop()→     closed
//
// Concurrency: the public methods all take p.mu before mutating, and acquire
// speaker.Lock around any mutation the audio goroutine could observe.
type realPlayer struct {
	mu sync.Mutex

	// One-time speaker.Init. We re-init lazily if a future track has a
	// different sample rate.
	speakerInit       bool
	speakerSampleRate beep.SampleRate

	// Currently-loaded track. file is the source; loop wraps it for infinite
	// repetition; ctrl wraps loop for pause; volume wraps ctrl for gain.
	file   *os.File
	stream beep.StreamSeekCloser
	ctrl   *beep.Ctrl
	volume *effects.Volume

	// loaded reports whether file/stream/etc are populated.
	loaded     bool
	loadedPath string

	// volumeLevel in [0,1]; mirrored into the effects.Volume base/Volume
	// fields each time it changes. Kept separately so we can render the
	// current level in Status without reaching into the speaker lock.
	volumeLevel float64

	lastErr error
}

// NewPlayer returns a real audio player. The speaker isn't initialized until
// the first successful Play — that way constructing the player on a
// headless or no-audio machine is harmless until the user tries to listen.
func NewPlayer() Player {
	return &realPlayer{volumeLevel: 0.7}
}

// Play loads path, replaces any currently-playing track, and starts an
// infinite loop with the player's current volume.
func (p *realPlayer) Play(path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Open + decode first; if anything fails we leave the previous track
	// running rather than dropping into silence.
	f, err := os.Open(path)
	if err != nil {
		p.lastErr = err
		return err
	}
	streamer, format, err := mp3.Decode(f)
	if err != nil {
		_ = f.Close()
		p.lastErr = fmt.Errorf("decode mp3: %w", err)
		return p.lastErr
	}

	if err := p.ensureSpeaker(format.SampleRate); err != nil {
		_ = streamer.Close()
		_ = f.Close()
		p.lastErr = err
		return err
	}

	loop, err := beep.Loop2(streamer)
	if err != nil {
		_ = streamer.Close()
		_ = f.Close()
		p.lastErr = fmt.Errorf("loop: %w", err)
		return p.lastErr
	}

	ctrl := &beep.Ctrl{Streamer: loop, Paused: false}
	volume := &effects.Volume{
		Streamer: ctrl,
		Base:     2,
		Volume:   volumeFromLinear(p.volumeLevel),
		Silent:   p.volumeLevel == 0,
	}

	// Replace the live track atomically. Clear stops any current stream the
	// speaker is reading from; closing the prior decoder/file releases the
	// fd. We do this AFTER decoding the new file so a corrupt new track
	// can't strand us with no audio.
	speaker.Clear()
	if p.loaded {
		_ = p.stream.Close()
		_ = p.file.Close()
	}

	p.file = f
	p.stream = streamer
	p.ctrl = ctrl
	p.volume = volume
	p.loaded = true
	p.loadedPath = path
	p.lastErr = nil

	speaker.Play(volume)
	return nil
}

// Stop halts playback and releases the decoder + fd.
func (p *realPlayer) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

func (p *realPlayer) stopLocked() {
	if !p.loaded {
		return
	}
	speaker.Clear()
	_ = p.stream.Close()
	_ = p.file.Close()
	p.stream = nil
	p.file = nil
	p.ctrl = nil
	p.volume = nil
	p.loaded = false
	p.loadedPath = ""
}

// PauseToggle flips Ctrl.Paused under the speaker lock — without the lock,
// the audio goroutine could read Paused mid-flip and either miss a frame or
// stutter.
func (p *realPlayer) PauseToggle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded {
		return
	}
	speaker.Lock()
	p.ctrl.Paused = !p.ctrl.Paused
	speaker.Unlock()
}

// SetVolume writes the new level into the effects.Volume node. Same locking
// rule as PauseToggle.
func (p *realPlayer) SetVolume(v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volumeLevel = clampVolume(v)
	if !p.loaded {
		return
	}
	speaker.Lock()
	p.volume.Volume = volumeFromLinear(p.volumeLevel)
	p.volume.Silent = p.volumeLevel == 0
	speaker.Unlock()
}

// AdjustVolume nudges by delta. Convenience for "+ / -" key handlers.
func (p *realPlayer) AdjustVolume(delta float64) {
	p.mu.Lock()
	level := clampVolume(p.volumeLevel + delta)
	p.mu.Unlock()
	p.SetVolume(level)
}

// Status snapshots the current state.
func (p *realPlayer) Status() PlayerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := PlayerStatus{
		Available: true,
		Playing:   p.loaded,
		Path:      p.loadedPath,
		Volume:    p.volumeLevel,
		Err:       p.lastErr,
	}
	if p.loaded && p.ctrl != nil {
		// Reading Paused without the speaker lock is racy in principle but
		// it's a single bool — the worst case is one rendered frame is off
		// from the truth, which corrects on the next render.
		st.Paused = p.ctrl.Paused
	}
	return st
}

// Close releases the speaker and any held file. Safe to call more than once.
func (p *realPlayer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
	if p.speakerInit {
		speaker.Close()
		p.speakerInit = false
	}
}

// ensureSpeaker initializes the audio output device on first use, or
// re-initializes it if a new track has a different sample rate. Speaker is
// a global in beep — there's no per-instance device.
func (p *realPlayer) ensureSpeaker(sr beep.SampleRate) error {
	if p.speakerInit && p.speakerSampleRate == sr {
		return nil
	}
	if p.speakerInit {
		// Close before re-init; speaker.Init returns an error if already
		// active. We accept a brief audio gap between tracks of different
		// sample rates.
		speaker.Close()
		p.speakerInit = false
	}
	// 100ms buffer balances latency against jitter tolerance on slow hosts.
	if err := speaker.Init(sr, sr.N(time.Second/10)); err != nil {
		return fmt.Errorf("speaker init: %w", err)
	}
	p.speakerInit = true
	p.speakerSampleRate = sr
	return nil
}

// volumeFromLinear maps a linear [0,1] slider position to the logarithmic
// scale beep's Volume effect expects. Volume=0 is "no change"; negative is
// quieter; positive is louder. Base=2 means each ±1 doubles/halves the
// gain. A linear 0.7 lands around Volume=-0.5 (~30% quieter than unity).
func volumeFromLinear(v float64) float64 {
	if v <= 0 {
		return -math.MaxFloat64 // Silent flag covers this; value is unused
	}
	// log2(v) gives us a sensible perceptual scale across [0,1] → [-∞, 0].
	// We cap the floor at -6 (~64× quieter) so slider:0.01 isn't silent.
	out := math.Log2(v)
	if out < -6 {
		out = -6
	}
	return out
}

