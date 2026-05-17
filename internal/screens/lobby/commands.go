package lobby

import (
	"fmt"
	"strings"

	"github.com/bigboggy/vibespace/internal/screens"
	"github.com/bigboggy/vibespace/internal/theme"
	"github.com/bigboggy/vibespace/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
)

type command struct {
	name string
	desc string
}

// builtins is the canonical list of slash commands. Order here is the order
// shown in autocomplete. /auth and /logout are mutually exclusive — the
// palette hides whichever doesn't match the current auth state.
var builtins = []command{
	{"/join", "join or switch channel"},
	{"/leave", "return to #lobby"},
	{"/list", "list channels"},
	{"/who", "list users in this channel"},
	{"/me", "third-person action"},
	{"/profile", "view a profile (no arg = your own)"},
	{"/friend", "send a friend request"},
	{"/accept", "accept an incoming friend request"},
	{"/reject", "reject an incoming friend request"},
	{"/unfriend", "remove a friend"},
	{"/friends", "show your friends + pending requests"},
	{"/post", "write a post on your profile"},
	{"/sign", "sign a friend's guestbook"},
	{"/leaderboard", "open the token-usage leaderboard"},
	{"/leaderboard-join", "how to add yourself to the leaderboard"},
	{"/radio", "browse the radio episodes"},
	{"/nick", "set your display name"},
	{"/theme", "switch color theme"},
	{"/auth", "link your GitHub account"},
	{"/logout", "unlink your GitHub account"},
	{"/clear", "clear scrollback"},
	{"/help", "show all commands"},
	{"/quit", "exit vibespace"},
}

var aliases = map[string]string{
	"/exit":     "/quit",
	"/bye":      "/quit",
	"/part":     "/leave",
	"/channels": "/list",
	"/users":    "/who",
	"/?":        "/help",
	"/signout":  "/logout",
	"/p":        "/profile",
	"/lb":       "/leaderboard",
	"/board":    "/leaderboard",
	"/top":      "/leaderboard",
	"/lb-join":  "/leaderboard-join",
	"/join-lb":  "/leaderboard-join",
	"/r":        "/radio",
	"/tune":     "/radio",
	"/listen":   "/radio",
}

// allowedWhenGated lists commands that work even when the session hasn't
// authenticated yet. Everything else returns a "type /auth" hint.
//
// /profile is gated-allowed so unauthenticated users can browse profiles;
// the screen itself nudges them to /auth before they can act on what they see.
var allowedWhenGated = map[string]bool{
	"/auth":             true,
	"/help":             true,
	"/quit":             true,
	"/clear":            true,
	"/theme":            true,
	"/nick":             true,
	"/profile":          true,
	"/leaderboard":      true,
	"/leaderboard-join": true,
	"/radio":            true,
}

func canonicalName(name string) string {
	if a, ok := aliases[name]; ok {
		return a
	}
	return name
}

// commandHidden reports whether a builtin should be omitted from autocomplete
// in the current state. Used to flip between /auth and /logout depending on
// whether the user is linked.
func (s *Screen) commandHidden(name string) bool {
	switch name {
	case "/auth":
		// Hide once the user is linked; /logout takes its slot.
		return s.ghLogin != ""
	case "/logout":
		// Hide when there's nothing to log out of. Also hide when /auth isn't
		// even configured server-side (local mode).
		return s.auth == nil || s.ghLogin == ""
	}
	return false
}

func (s *Screen) handleSlash(text string) (*Screen, tea.Cmd) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return s, nil
	}
	name := canonicalName(parts[0])
	args := parts[1:]

	if s.authRequired() && !allowedWhenGated[name] {
		s.postSystem("type /auth to authenticate first")
		return s, nil
	}

	switch name {
	case "/help":
		return s.cmdHelp()
	case "/clear":
		return s.cmdClear()
	case "/quit":
		s.Cleanup()
		return s, screens.Quit()
	case "/me":
		return s.cmdMe(args)
	case "/join":
		return s.cmdJoin(args)
	case "/leave":
		return s.cmdLeave()
	case "/list":
		return s.cmdList()
	case "/who":
		return s.cmdWho()
	case "/auth":
		return s.cmdAuthGithub()
	case "/logout":
		return s.cmdLogout()
	case "/theme":
		return s.cmdTheme(args)
	case "/nick":
		return s.cmdNick(args)
	case "/profile":
		return s.cmdProfile(args)
	case "/friend":
		return s.cmdFriend(args)
	case "/accept":
		return s.cmdAccept(args)
	case "/reject":
		return s.cmdReject(args)
	case "/unfriend":
		return s.cmdUnfriend(args)
	case "/friends":
		return s.cmdFriends()
	case "/post":
		return s.cmdPost(args)
	case "/sign":
		return s.cmdSign(args)
	case "/leaderboard":
		return s, screens.Navigate(screens.LeaderboardID)
	case "/leaderboard-join":
		return s, screens.OpenLeaderboardJoin()
	case "/radio":
		return s, screens.Navigate(screens.RadioID)
	}
	s.postSystem(fmt.Sprintf("unknown command %q — try /help", parts[0]))
	return s, nil
}

// ---------------------------------------------------------------------------
// Per-command handlers
// ---------------------------------------------------------------------------

func (s *Screen) cmdHelp() (*Screen, tea.Cmd) {
	lines := []string{"available commands:"}
	for _, c := range builtins {
		if s.commandHidden(c.name) {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-13s %s", c.name, c.desc))
	}
	lines = append(lines, "ctrl+c quits")
	s.postSystem(strings.Join(lines, "\n"))
	return s, nil
}

// cmdClear is intentionally local-only: it just hides scrollback for this
// session by jumping scroll past the end. The hub still holds the history.
func (s *Screen) cmdClear() (*Screen, tea.Cmd) {
	msgs, _ := s.hub.Messages(s.activeName)
	s.chatScroll = len(msgs) + len(s.localMessages)
	s.localMessages = nil
	return s, nil
}

func (s *Screen) cmdMe(args []string) (*Screen, tea.Cmd) {
	body := strings.Join(args, " ")
	if body == "" {
		return s, nil
	}
	s.hub.Post(s.activeName, s.meUser, body, ui.ChatAction)
	s.chatScroll = 0
	return s, nil
}

func (s *Screen) cmdJoin(args []string) (*Screen, tea.Cmd) {
	if len(args) == 0 {
		s.postSystem("usage: /join #channel")
		return s, nil
	}
	name := args[0]
	if !strings.HasPrefix(name, "#") {
		name = "#" + name
	}
	if !s.hub.HasChannel(name) {
		s.hub.CreateChannel(name)
	}
	prev := s.activeName
	s.activeName = name
	s.hub.SetViewing(s.subID, name)
	s.chatScroll = 0
	if prev != name {
		s.hub.Post(name, s.meUser, "joined "+name, ui.ChatJoin)
	}
	return s, nil
}

func (s *Screen) cmdLeave() (*Screen, tea.Cmd) {
	if s.activeName == "#lobby" {
		s.postSystem("you're already in #lobby — try /quit to exit")
		return s, nil
	}
	leaving := s.activeName
	s.hub.Post(leaving, s.meUser, "left "+leaving, ui.ChatJoin)
	s.activeName = "#lobby"
	s.hub.SetViewing(s.subID, "#lobby")
	s.chatScroll = 0
	return s, nil
}

func (s *Screen) cmdList() (*Screen, tea.Cmd) {
	names := s.hub.ChannelNames()
	lines := []string{"channels:"}
	for _, n := range names {
		lines = append(lines, fmt.Sprintf("  %-20s  %4d online", n, s.hub.Online(n)))
	}
	s.postSystem(strings.Join(lines, "\n"))
	return s, nil
}

func (s *Screen) cmdWho() (*Screen, tea.Cmd) {
	n := s.hub.Online(s.activeName)
	s.postSystem(fmt.Sprintf("%d connected in %s", n, s.activeName))
	return s, nil
}

// cmdTheme opens an interactive picker (no args) or switches directly by id.
// The picker live-previews each option as the user navigates and commits on
// enter; the direct form is kept so scripted / muscle-memory flows still work.
func (s *Screen) cmdTheme(args []string) (*Screen, tea.Cmd) {
	if len(args) == 0 {
		s.openThemePicker()
		return s, nil
	}
	id := strings.ToLower(args[0])
	t, ok := theme.Get(id)
	if !ok {
		s.postSystem(fmt.Sprintf("unknown theme %q — try /theme to pick", args[0]))
		return s, nil
	}
	s.styles.SetTheme(t)
	s.postSystem(fmt.Sprintf("theme set to %s (%s)", t.ID, t.DisplayName))
	return s, nil
}

func (s *Screen) cmdNick(args []string) (*Screen, tea.Cmd) {
	if len(args) == 0 {
		s.postSystem(fmt.Sprintf("your display name is %s", s.meUser))
		return s, nil
	}
	nick := strings.Join(args, " ")
	nick = strings.TrimPrefix(nick, "@")
	if nick == "" {
		return s, nil
	}
	prev := s.meUser
	s.meUser = nick
	s.hub.Post(s.activeName, "*",
		fmt.Sprintf("%s is now %s", prev, nick),
		ui.ChatSystem)
	return s, nil
}

func (s *Screen) cmdLogout() (*Screen, tea.Cmd) {
	if s.ghLogin == "" {
		s.postSystem("you're not authenticated")
		return s, nil
	}
	if s.auth != nil && s.fingerprint != "" {
		if err := s.auth.Unlink(s.fingerprint); err != nil {
			s.postSystem("failed to unlink: " + err.Error())
			return s, nil
		}
	}
	prev := s.meUser
	s.ghLogin = ""
	s.meUser = s.fallbackUser
	s.hub.Post(s.activeName, "*",
		fmt.Sprintf("%s logged out, now %s", prev, s.meUser),
		ui.ChatSystem)
	return s, nil
}
