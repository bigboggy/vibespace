// vibespace — a TUI lobby for devs and vibe coders.
//
// This is the locally-installed binary. By default it connects to the
// vibespace.sh backend over WebSocket so chat is shared with every other
// binary user (and the SSH "glimpse" sessions). When VIBESPACE_BACKEND is
// empty (or the connection fails), it falls back to an in-process LocalHub
// — you still get the TUI, radio playback, and report subcommand, but
// chat is single-user.
//
// The SSH-server entrypoint lives in cmd/server.
package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"time"

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

// defaultBackend is the production WebSocket endpoint. Override via
// VIBESPACE_BACKEND for dev (e.g. ws://localhost:8080/ws); set to "off" to
// force fully offline mode.
const defaultBackend = "wss://vibespace.sh/ws"

// bakedClientID is overridden at build time via -ldflags -X main.bakedClientID=...
// to bake the GitHub OAuth client ID into the release binary.
var bakedClientID = ""

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

	// Try to attach to the shared backend first. Falls back to an offline
	// LocalHub if no backend is configured or the connection fails — the
	// rest of the app (radio, profile, report) still works either way.
	configDir := localConfigDir()
	var (
		h            hub.Hub
		fallbackUser = localUser()
		ghLogin      string
	)
	if rh := connectBackend(configDir); rh != nil {
		h = rh
		fallbackUser = rh.User()
		ghLogin = rh.GhLogin()
	} else {
		h = hub.NewLocal()
	}

	// SQLite lives under the user's config dir so profiles persist across
	// runs without polluting the working directory.
	dbPath := localDBPath()
	data, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vibespace: store: %v\n", err)
		os.Exit(1)
	}
	defer data.Close()

	// /auth via SSH-key + GitHub device flow (the old in-lobby path) is only
	// wired in offline mode now. In backend mode the device flow runs once at
	// startup (see connectBackend), so the lobby's /auth disappears.
	var authSvc *auth.Service
	if ghLogin == "" {
		authSvc, ghLogin = localAuth(data)
	}

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
		app.New(styles, fallbackUser, fingerprint, ghLogin, h, authSvc, data, radioClient, radioDL, radioPlayer, app.LocalMode(true)),
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

// connectBackend tries to reach the shared chat backend. Returns nil — and
// prints a friendly note — when backend is disabled, misconfigured, or
// unreachable. The TUI still launches in offline (LocalHub) mode.
//
// On the happy path it loads a cached GitHub access token, falls through
// to a fresh device flow if none, then dials the WS endpoint.
func connectBackend(configDir string) *hub.RemoteHub {
	backend := os.Getenv("VIBESPACE_BACKEND")
	if backend == "" {
		backend = defaultBackend
	}
	if backend == "off" {
		fmt.Println("vibespace: VIBESPACE_BACKEND=off → offline mode")
		return nil
	}

	clientID := os.Getenv("VIBESPACE_GH_CLIENT_ID")
	if clientID == "" {
		clientID = bakedClientID
	}
	if clientID == "" {
		fmt.Println("vibespace: VIBESPACE_GH_CLIENT_ID not set — running offline. Set it or rebuild with -ldflags -X main.bakedClientID=...")
		return nil
	}

	tokPath := authPath(configDir)
	token, login := loadToken(tokPath)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if token == "" {
		var err error
		token, login, err = runDeviceFlow(ctx, clientID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vibespace: device flow: %v\n  → offline mode\n", err)
			return nil
		}
		if err := saveToken(tokPath, token, login); err != nil {
			// Non-fatal — we still have the token in memory for this run.
			fmt.Fprintf(os.Stderr, "vibespace: couldn't persist token: %v\n", err)
		}
	}

	rh, err := hub.Dial(ctx, backend, token)
	if err == nil {
		return rh
	}
	if !looksLikeAuthError(err) {
		fmt.Fprintf(os.Stderr, "vibespace: backend %s: %v\n  → offline mode\n", backend, err)
		return nil
	}

	// Token rejected — drop it and try a fresh device flow once.
	fmt.Fprintln(os.Stderr, "vibespace: cached token rejected — re-linking GitHub…")
	clearToken(tokPath)
	token, login, err = runDeviceFlow(ctx, clientID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vibespace: device flow: %v\n  → offline mode\n", err)
		return nil
	}
	if err := saveToken(tokPath, token, login); err != nil {
		fmt.Fprintf(os.Stderr, "vibespace: couldn't persist token: %v\n", err)
	}
	rh, err = hub.Dial(ctx, backend, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vibespace: backend %s: %v\n  → offline mode\n", backend, err)
		return nil
	}
	return rh
}
