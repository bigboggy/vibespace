// vibespace — a TUI lobby for devs and vibe coders.
//
// This is the local-mode entrypoint: one user, one hub, one bubbletea program.
// The SSH-server entrypoint lives in cmd/server.
package main

import (
	"fmt"
	"os"
	"os/user"

	"github.com/bigboggy/vibespace/internal/app"
	"github.com/bigboggy/vibespace/internal/auth"
	"github.com/bigboggy/vibespace/internal/hub"
	"github.com/bigboggy/vibespace/internal/identity"
	"github.com/bigboggy/vibespace/internal/radio"
	"github.com/bigboggy/vibespace/internal/reportcli"
	"github.com/bigboggy/vibespace/internal/store"
	"github.com/bigboggy/vibespace/internal/theme"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func main() {
	// Subcommand dispatch. Only `report` is a non-TUI flow today; everything
	// else falls through to the local lobby below. Lives in its own package
	// so `go run main.go` works as well as `go run .`.
	if len(os.Args) > 1 && os.Args[1] == "report" {
		reportcli.Run(os.Args[2:])
		return
	}

	// Fire-and-forget token usage upload. Throttled to once per minute via
	// a marker file in the config dir, so back-to-back TUI launches don't
	// double-fire. Runs detached — no impact on TUI startup.
	reportcli.KickBackground(localConfigDir())

	h := hub.New()
	// Local-mode SQLite lives under the user's config dir so profiles persist
	// across runs without polluting the working directory.
	dbPath := localDBPath()
	data, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vibespace: store: %v\n", err)
		os.Exit(1)
	}
	defer data.Close()

	// Optional /auth in local mode — set VIBESPACE_GH_CLIENT_ID to enable.
	// The identity store sits next to the SQLite DB. With no client id, /auth
	// stays disabled and the lobby is just a local chat surface.
	authSvc, ghLogin := localAuth(data)

	// In local mode the "fingerprint" is the synthesized identity key (see
	// localFingerprint) — only meaningful when authSvc is non-nil. Pass it
	// either way; the lobby just stashes it.
	fingerprint := ""
	if authSvc != nil {
		fingerprint = localFingerprint()
	}

	// Radio manifest + on-disk cache. The client caches under the per-user
	// config dir so the manifest survives across runs and tolerates offline
	// launches. VIBESPACE_RADIO_MANIFEST overrides the manifest URL for dev.
	// The downloader writes finished shows into the same dir.
	radioDir := localRadioDir()
	radioClient := radio.NewClient(radioDir, os.Getenv("VIBESPACE_RADIO_MANIFEST"))
	radioDL := radio.NewDownloader(radioDir)
	// Real audio player on platforms that support it (macOS/Windows via
	// purego, Linux+CGO via ALSA). The stub is used by linux/!cgo builds.
	radioPlayer := radio.NewPlayer()

	styles := theme.New(lipgloss.DefaultRenderer(), theme.Default())
	// No mouse capture — the app doesn't consume mouse events, and capturing
	// them blocks the terminal's native click-and-drag text selection (which
	// users need to copy the install one-liner out of the join dialog).
	p := tea.NewProgram(
		app.New(styles, localUser(), fingerprint, ghLogin, h, authSvc, data, radioClient, radioDL, radioPlayer, app.LocalMode(true)),
		tea.WithAltScreen(),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "vibespace: %v\n", err)
		os.Exit(1)
	}
}

// localConfigDir returns the per-user directory under which all
// vibespace-owned files live (SQLite store, identity map, auto-report
// marker). Creates the dir as a side effect so callers don't have to.
// Falls back to "." when the OS doesn't expose a config dir.
func localConfigDir() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return "."
	}
	root := dir + "/vibespace"
	_ = os.MkdirAll(root, 0o700)
	return root
}

func localDBPath() string {
	return localConfigDir() + "/vibespace.db"
}

// localRadioDir is the per-user directory the radio client uses for the
// manifest cache (and, once Phase 2 lands, downloaded episodes).
func localRadioDir() string {
	root := localConfigDir() + "/radio"
	_ = os.MkdirAll(root, 0o700)
	return root
}

// localAuth wires the /auth flow in local mode when VIBESPACE_GH_CLIENT_ID is
// set. With no SSH fingerprint to key off of, the identity store is keyed by
// the OS username instead — so re-running the binary picks up the same link.
func localAuth(data *store.Store) (*auth.Service, string) {
	clientID := os.Getenv("VIBESPACE_GH_CLIENT_ID")
	if clientID == "" {
		return nil, ""
	}
	idPath := localIdentityPath()
	idStore, err := identity.Open(idPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vibespace: identity: %v\n", err)
		return nil, ""
	}
	svc := auth.New(clientID, idStore, data)
	if svc == nil {
		return nil, ""
	}
	// Pre-resolve any prior link so the session starts already authenticated.
	ghLogin, _ := idStore.Lookup(localFingerprint())
	return svc, ghLogin
}

func localIdentityPath() string {
	return localConfigDir() + "/identities.json"
}

// localFingerprint is the stable key local-mode sessions use in the identity
// store. The real server uses SHA256 SSH pubkey fingerprints; locally we have
// no SSH layer, so we synthesize one from the OS user. The lobby passes this
// through to auth.Service.Link / .Lookup.
func localFingerprint() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "local:" + u.Username
	}
	return "local:unknown"
}

func localUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "@" + u.Username
	}
	return "@local"
}
