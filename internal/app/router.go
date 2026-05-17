package app

import (
	"github.com/bigboggy/vibespace/internal/hub"
	"github.com/bigboggy/vibespace/internal/screens"
	"github.com/bigboggy/vibespace/internal/screens/lobby"
	radioscreen "github.com/bigboggy/vibespace/internal/screens/radio"
	tea "github.com/charmbracelet/bubbletea"
)

// Update is the top-level message handler. It manages three concerns:
//
//  1. Global side-effects (screen changes, quit) that any screen can emit via
//     screens.Navigate / tea.Quit.
//  2. Global keyboard shortcuts (esc back to lobby, ctrl+c quit) that apply
//     when the current screen isn't holding a text input.
//  3. Forwarding the remaining messages to the active screen.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {

	case tea.WindowSizeMsg:
		a.width = m.Width
		a.height = m.Height
		// Broadcast so screens with cached widths (textareas) can resize.
		for id, s := range a.screens {
			ns, _ := s.Update(msg)
			a.screens[id] = ns
		}
		return a, nil

	case screens.NavigateMsg:
		return a, a.navigate(m.Target)

	case screens.OpenProfileMsg:
		// Populate the profile screen's target/viewer state before switching
		// to it, so the screen renders the right user on its first View call.
		if a.profile != nil {
			a.profile.SetTarget(m.Target, m.Viewer)
		}
		return a, a.navigate(screens.ProfileID)

	case screens.OpenLeaderboardJoinMsg:
		// Flip the join modal on before navigating so it's already up on the
		// first frame the user sees. Lobby uses this for /leaderboard-join.
		if a.board != nil {
			a.board.ShowJoin()
		}
		return a, a.navigate(screens.LeaderboardID)

	case hub.Event:
		// Hub broadcasts always go to the lobby — it's the screen that owns
		// the subscription — regardless of which screen is currently active.
		// Without this, events that arrive while the intro is still showing
		// would be swallowed and the subscription would stall.
		ns, cmd := a.screens[screens.LobbyID].Update(m)
		a.screens[screens.LobbyID] = ns
		return a, cmd

	case radioscreen.DownloadProgressMsg, radioscreen.DownloadFinishedMsg:
		// Download messages must reach the radio screen even when the user
		// has navigated away mid-download — otherwise the goroutine keeps
		// running but the UI never resumes. Mirrors the hub.Event routing.
		if rs, ok := a.screens[screens.RadioID]; ok {
			ns, cmd := rs.Update(m)
			a.screens[screens.RadioID] = ns
			return a, cmd
		}
		return a, nil

	case tea.KeyMsg:
		return a.handleKey(m)
	}

	return a, a.updateScreen(msg)
}

// navigate switches the active screen. Entering the lobby for the first time
// triggers the "@boggy entered" join message. Screens that implement
// OnEnter() return a Cmd that runs immediately after activation — used by
// screens that lazily load remote data (e.g. the radio manifest) so the
// response arrives when the screen is already current.
func (a *App) navigate(target string) tea.Cmd {
	scr, ok := a.screens[target]
	if !ok {
		return nil
	}
	a.current = target
	if target == screens.LobbyID {
		if lob, ok := scr.(*lobby.Screen); ok {
			lob.EnsureJoined()
		}
	}
	if entrant, ok := scr.(interface{ OnEnter() tea.Cmd }); ok {
		return entrant.OnEnter()
	}
	return nil
}

// handleKey runs global key bindings (esc → lobby, ctrl+c → quit) before
// delegating to the active screen. Screens that own a text input
// (InputFocused()==true) get every key without interception, otherwise typing
// "q" in a chat box would quit instead of typing q.
func (a *App) handleKey(km tea.KeyMsg) (tea.Model, tea.Cmd) {
	scr := a.activeScreen()

	// Intro is fullscreen and consumes all keys (any key skips).
	if a.current == screens.IntroID {
		return a, a.updateScreen(km)
	}

	if scr.InputFocused() {
		return a, a.updateScreen(km)
	}

	switch km.String() {
	case "ctrl+c":
		return a, tea.Quit
	case "esc", "q":
		return a, a.navigate(screens.LobbyID)
	}
	return a, a.updateScreen(km)
}
