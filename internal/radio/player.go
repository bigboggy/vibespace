package radio

// Player is the radio's audio output. There are two implementations,
// selected at compile time by the `cgo` build tag:
//
//   - player_cgo.go     real player (beep + oto). Default when CGO is on.
//   - player_nocgo.go   no-op stub. Used by CGO_ENABLED=0 builds, including
//                       the SSH server which never plays audio anyway.
//
// The stub exists so callers don't need build tags themselves — they always
// have a Player, the real one or one that politely says it can't play.
type Player interface {
	// Play loads path (must be a local MP3) and starts looping it.
	// If a track is already playing, it's replaced with the new one.
	// Returns ErrAudioUnavailable in the stub build.
	Play(path string) error

	// Stop halts playback. Safe to call when nothing's playing.
	Stop()

	// PauseToggle flips between paused and playing. No-op when nothing is
	// loaded.
	PauseToggle()

	// SetVolume sets output level in [0.0, 1.0]. Values are clamped.
	SetVolume(v float64)

	// AdjustVolume nudges the volume by delta in [-1.0, 1.0]. Clamped.
	AdjustVolume(delta float64)

	// Status returns a snapshot of player state. Safe to call any time.
	Status() PlayerStatus

	// Close releases any held resources. Safe to call more than once.
	Close()
}

// PlayerStatus is a snapshot of Player state used by the UI to render.
type PlayerStatus struct {
	// Available is false in stub builds where audio output isn't possible.
	// The radio screen uses this to swap the "press enter to play" hint
	// for a "this build has no audio" notice.
	Available bool

	// Playing is true between a successful Play() and the next Stop()
	// (or Play() with a different path).
	Playing bool

	// Paused is true when the user has hit pause. Playing stays true.
	Paused bool

	// Path is the file currently loaded (empty when nothing is loaded).
	Path string

	// Volume in [0.0, 1.0].
	Volume float64

	// Err is the last error, if any. The UI shows it inline; it's cleared
	// on the next successful Play().
	Err error
}

// volume clamps v to [0,1].
func clampVolume(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
