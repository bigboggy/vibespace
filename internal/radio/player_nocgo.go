//go:build linux && !cgo

// Stub player for Linux builds without CGO — the server cross-compile path.
// All other platforms (macOS, Windows, Linux+CGO) get the real player.

package radio

import "errors"

// ErrAudioUnavailable is returned by the stub Player when CGO is off and
// real audio output isn't compiled in. The UI surfaces this so users know
// the binary can't play (e.g. the cross-compiled server build).
var ErrAudioUnavailable = errors.New("radio: audio output unavailable in this build (build with CGO_ENABLED=1)")

type stubPlayer struct{}

// NewPlayer returns a no-op Player. The radio screen still works — it just
// renders "no audio in this build" instead of starting playback.
func NewPlayer() Player { return &stubPlayer{} }

func (*stubPlayer) Play(string) error          { return ErrAudioUnavailable }
func (*stubPlayer) Stop()                      {}
func (*stubPlayer) PauseToggle()               {}
func (*stubPlayer) SetVolume(float64)          {}
func (*stubPlayer) AdjustVolume(float64)       {}
func (*stubPlayer) Status() PlayerStatus       { return PlayerStatus{Available: false} }
func (*stubPlayer) Close()                     {}
