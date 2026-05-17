// Package app is the bubbletea Model that wires everything together.
//
// One App per session. App owns the intro + lobby screens for this session;
// chat state itself lives in the shared *hub.Hub passed to New.
package app

import (
	"github.com/bigboggy/vibespace/internal/auth"
	"github.com/bigboggy/vibespace/internal/hub"
	"github.com/bigboggy/vibespace/internal/radio"
	"github.com/bigboggy/vibespace/internal/screens"
	"github.com/bigboggy/vibespace/internal/screens/intro"
	"github.com/bigboggy/vibespace/internal/screens/leaderboard"
	"github.com/bigboggy/vibespace/internal/screens/lobby"
	"github.com/bigboggy/vibespace/internal/screens/profile"
	radioscreen "github.com/bigboggy/vibespace/internal/screens/radio"
	"github.com/bigboggy/vibespace/internal/store"
	"github.com/bigboggy/vibespace/internal/theme"
	"github.com/bigboggy/vibespace/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
)

// App is the top-level bubbletea Model.
type App struct {
	styles *theme.Styles // shared with all screens — /theme mutates in place

	screens map[string]screens.Screen
	current string
	lobby   *lobby.Screen   // kept for Cleanup + navigation hooks
	profile *profile.Screen // kept for navigation hooks (SetTarget)

	board *leaderboard.Screen // kept for navigation hooks (ShowJoin)

	data        *store.Store // queried by the top-right leaderboard widget
	radioPlayer radio.Player // closed during Cleanup so the audio device is released on session end

	width, height int
}

// LocalMode identifies whether this session is running inside the locally
// installed binary (true) or being served to an SSH client (false). Used to
// gate features like radio playback that only work on the user's own machine.
type LocalMode bool

// New constructs a session app. styles owns the per-session renderer and the
// current theme — pass a freshly built *theme.Styles per session so terminal
// capabilities and theme choice are scoped correctly. fallbackUser is the
// SSH-derived nick used when the user isn't (yet) authenticated; fingerprint
// is the SSH pubkey fingerprint (may be empty); ghLogin is a pre-existing
// GitHub link from the identity store (may be empty); h is the shared chat
// backend; authSvc may be nil to disable /auth; data is the profile/posts
// store (required); radioClient drives the /radio screen (may be nil to
// disable it); local distinguishes the locally installed binary from an SSH
// session so the radio screen knows whether to offer downloads or nudge the
// install one-liner. The intro screen is the initial active screen; it emits
// Navigate(lobby) when its animation ends.
func New(styles *theme.Styles, fallbackUser, fingerprint, ghLogin string, h *hub.Hub, authSvc *auth.Service, data *store.Store, radioClient *radio.Client, radioDL *radio.Downloader, radioPlayer radio.Player, local LocalMode) *App {
	lob := lobby.New(styles, fallbackUser, fingerprint, ghLogin, h, authSvc, data)
	prof := profile.New(styles, data)
	board := leaderboard.New(styles, data)
	scrns := map[string]screens.Screen{
		screens.IntroID:       intro.New(styles),
		screens.LobbyID:       lob,
		screens.ProfileID:     prof,
		screens.LeaderboardID: board,
	}
	if radioClient != nil {
		mode := radioscreen.ModeRemote
		dl := (*radio.Downloader)(nil)
		if local {
			mode = radioscreen.ModeLocal
			dl = radioDL
		}
		// Player is required by the screen; if the caller passed nil we
		// substitute the platform's no-op stub so the screen still works.
		if radioPlayer == nil {
			radioPlayer = radio.NewPlayer()
		}
		scrns[screens.RadioID] = radioscreen.New(styles, radioClient, dl, radioPlayer, mode)
	}
	return &App{
		styles:      styles,
		screens:     scrns,
		current:     screens.IntroID,
		lobby:       lob,
		profile:     prof,
		board:       board,
		data:        data,
		radioPlayer: radioPlayer,
	}
}

func (a *App) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, s := range a.screens {
		if c := s.Init(); c != nil {
			cmds = append(cmds, c)
		}
	}
	return tea.Batch(cmds...)
}

// Cleanup releases per-session resources (hub subscription, audio device).
// Safe to call more than once.
func (a *App) Cleanup() {
	if a.lobby != nil {
		a.lobby.Cleanup()
	}
	if a.radioPlayer != nil {
		a.radioPlayer.Close()
	}
}

// activeScreen returns the screen referenced by a.current, falling back to the
// lobby if the id is somehow stale.
func (a *App) activeScreen() screens.Screen {
	if s, ok := a.screens[a.current]; ok {
		return s
	}
	return a.screens[screens.LobbyID]
}

// updateScreen forwards a message to the active screen and writes the result
// back into the map.
func (a *App) updateScreen(msg tea.Msg) tea.Cmd {
	ns, cmd := a.activeScreen().Update(msg)
	a.screens[a.current] = ns
	return cmd
}

func (a *App) View() string {
	if a.width < ui.MinWidth || a.height < ui.MinHeight {
		return tooSmall(a.styles, a.width, a.height)
	}
	return a.renderFrame()
}
