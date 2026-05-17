// Package screens defines the Screen interface that every feature screen
// implements, plus the cross-cutting messages screens use to talk to the app
// (navigation requests, flash messages).
//
// Screens never import each other; they communicate by emitting messages that
// the app router catches. This keeps the dependency graph a star with the app
// at the center and screens as leaves.
package screens

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Screen is the contract every feature screen implements. The interface is
// intentionally small: Init/Update/View mirror tea.Model, the rest is metadata
// the app uses to compose chrome (header, footer, focus handling).
type Screen interface {
	// Init returns any commands the screen wants to fire when first wired up.
	Init() tea.Cmd

	// Update advances the screen's state in response to a message and returns
	// the new screen value. Screens are value types; the returned Screen
	// replaces the old one in the app's screen map.
	Update(msg tea.Msg) (Screen, tea.Cmd)

	// View renders the screen body. The header and footer are handled by the
	// app; width/height are the budget for the body region only.
	View(width, height int) string

	// Name is the stable id used by the router (e.g. "lobby", "news").
	Name() string

	// Title is the human-readable label shown in the header chip.
	Title() string

	// HeaderContext is optional metadata shown after the title (e.g. active
	// channel name, scroll position). Empty string means no extra content.
	HeaderContext() string

	// Footer returns the key hints shown in the status bar.
	Footer() []KeyHint

	// InputFocused returns true when the screen has an active text input and
	// the app's global key handlers should defer to the screen. Without this,
	// pressing 'q' in a text field would quit instead of typing q.
	InputFocused() bool
}

// KeyHint is one entry in the footer status bar.
type KeyHint struct {
	Key, Desc string
}

// NavigateMsg requests a screen switch. Emitted by Navigate; handled by the
// app router.
type NavigateMsg struct{ Target string }

// Navigate returns a tea.Cmd that asks the app to switch to the named screen.
func Navigate(target string) tea.Cmd {
	return func() tea.Msg { return NavigateMsg{Target: target} }
}

// OpenProfileMsg asks the app to switch to the profile screen with the given
// target login. Viewer is the currently-authenticated user (may be empty for
// guests). The app router uses this to populate the profile screen's state
// before activating it.
type OpenProfileMsg struct {
	Target string // gh_login of the profile to view
	Viewer string // gh_login of the current user, "" if unauthenticated
}

// OpenProfile returns a tea.Cmd that opens the profile screen for target.
func OpenProfile(target, viewer string) tea.Cmd {
	return func() tea.Msg { return OpenProfileMsg{Target: target, Viewer: viewer} }
}

// OpenLeaderboardJoinMsg asks the app to open the leaderboard screen with
// the "how to join" install dialog already visible. The router toggles the
// dialog on the leaderboard screen before activating it so it appears on
// the first frame instead of after an extra key press.
type OpenLeaderboardJoinMsg struct{}

// OpenLeaderboardJoin returns a tea.Cmd that opens the leaderboard with the
// join modal already up. Emitted by the lobby's `/leaderboard-join` command.
func OpenLeaderboardJoin() tea.Cmd {
	return func() tea.Msg { return OpenLeaderboardJoinMsg{} }
}

// Quit returns a tea.Cmd that quits the app.
func Quit() tea.Cmd {
	return tea.Quit
}

// Screen ids — used as keys in the app's screen map and as Navigate targets.
const (
	IntroID       = "intro"
	LobbyID       = "lobby"
	ProfileID     = "profile"
	LeaderboardID = "leaderboard"
	RadioID       = "radio"
)
