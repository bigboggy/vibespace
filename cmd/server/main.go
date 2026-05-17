// vibespace-server — wraps the lobby in an SSH server so anyone can connect
// with `ssh -p 2222 you@host` and land in the chat.
//
// One shared hub.Hub backs every session; each SSH connection gets its own
// app.App (intro + lobby) using the SSH user name as identity.
//
// Configuration via env vars:
//
//	VIBESPACE_ADDR           listen address, default ":2222"
//	VIBESPACE_HOSTKEY        host key path, default ".ssh/id_ed25519" (auto-generated on first run)
//	VIBESPACE_GH_CLIENT_ID   GitHub OAuth app client id (enables /auth github)
//	VIBESPACE_IDENTITY_PATH  path to identity store JSON, default "./identities.json"
//	VIBESPACE_DATA_PATH      path to profile/posts/friends SQLite DB, default "./vibespace.db"
//	VIBESPACE_RADIO_CACHE    dir for the radio manifest cache, default "./radio-cache"
//	VIBESPACE_RADIO_MANIFEST radio manifest URL override (default = release-hosted manifest)
//
// Run on a non-22 port unless you've moved system OpenSSH. Front it with a
// tunnel or VPS proxy before pointing public DNS at your home machine.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bigboggy/vibespace/internal/app"
	"github.com/bigboggy/vibespace/internal/auth"
	"github.com/bigboggy/vibespace/internal/hub"
	"github.com/bigboggy/vibespace/internal/identity"
	"github.com/bigboggy/vibespace/internal/radio"
	"github.com/bigboggy/vibespace/internal/store"
	"github.com/bigboggy/vibespace/internal/theme"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	bm "github.com/charmbracelet/wish/bubbletea"
	"github.com/charmbracelet/wish/logging"
	gossh "golang.org/x/crypto/ssh"
)

func main() {
	addr := envOr("VIBESPACE_ADDR", ":2222")
	hostKey := envOr("VIBESPACE_HOSTKEY", ".ssh/id_ed25519")
	ghClientID := os.Getenv("VIBESPACE_GH_CLIENT_ID")
	identityPath := envOr("VIBESPACE_IDENTITY_PATH", "./identities.json")
	dataPath := envOr("VIBESPACE_DATA_PATH", "./vibespace.db")
	radioCacheDir := envOr("VIBESPACE_RADIO_CACHE", "./radio-cache")
	radioManifestURL := os.Getenv("VIBESPACE_RADIO_MANIFEST")
	_ = os.MkdirAll(radioCacheDir, 0o700)
	radioClient := radio.NewClient(radioCacheDir, radioManifestURL)

	world := hub.New()

	// The profile/posts/friends SQLite store is always opened — profiles work
	// without auth, just without cached GitHub data.
	data, err := store.Open(dataPath)
	if err != nil {
		log.Fatalf("data store: %v", err)
	}
	defer data.Close()
	log.Printf("data store at %s", dataPath)

	var authSvc *auth.Service
	if ghClientID != "" {
		idStore, err := identity.Open(identityPath)
		if err != nil {
			log.Fatalf("identity store: %v", err)
		}
		authSvc = auth.New(ghClientID, idStore, data)
		log.Printf("github auth enabled, identity store at %s", identityPath)
	} else {
		log.Printf("github auth disabled (set VIBESPACE_GH_CLIENT_ID to enable)")
	}

	s, err := wish.NewServer(
		wish.WithAddress(addr),
		wish.WithHostKeyPath(hostKey),


		// Accept any pubkey — we don't allowlist, we just want to capture the
		// fingerprint for `/auth github` linking. Without a PublicKeyHandler,
		// charm's ssh defaults to NoClientAuth, which means OpenSSH never
		// offers a key in the first place and sess.PublicKey() is always nil.
		wish.WithPublicKeyAuth(func(_ ssh.Context, _ ssh.PublicKey) bool {
			return true
		}),
		// Once any auth handler is set, NoClientAuth flips off — so keyless
		// clients also need a path. Keyboard-interactive that returns true
		// without challenging the user gets them through silently. They land
		// as @guest and can't use /auth (no fingerprint to bind to).
		wish.WithKeyboardInteractiveAuth(func(_ ssh.Context, _ gossh.KeyboardInteractiveChallenge) bool {
			return true
		}),

		wish.WithMiddleware(
			bm.Middleware(teaHandler(world, authSvc, data, radioClient)),
			activeterm.Middleware(),
			// reportMiddleware sits outside activeterm so `ssh -T host report`
			// (no PTY, command="report") gets handled before activeterm bounces
			// it for lacking a terminal. Logging stays outermost so both the
			// TUI and report sessions show up in the access log.
			reportMiddleware(data, authSvc),
			logging.Middleware(),
		),
	)
	if err != nil {
		log.Fatalf("wish: %v", err)
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	log.Printf("vibespace-server listening on %s", addr)
	go func() {
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-done
	log.Println("shutting down")

	// Persist today's hot-tier token totals to SQLite before we exit so a
	// graceful stop doesn't drop the in-memory portion of the leaderboard.
	// Hot tier doesn't fsync on every write — the trade-off is fast
	// per-minute uploads now, one SQLite write at shutdown / day rollover.
	if err := data.FlushHot(); err != nil {
		log.Printf("flush hot tier: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
		log.Printf("shutdown: %v", err)
	}
}

// teaHandler returns a wish bubbletea handler that builds a per-session app.
func teaHandler(world *hub.Hub, authSvc *auth.Service, data *store.Store, radioClient *radio.Client) bm.Handler {
	return func(sess ssh.Session) (tea.Model, []tea.ProgramOption) {
		_, _, hasPty := sess.Pty()
		if !hasPty {
			wish.Fatalln(sess, "vibespace requires an interactive terminal (try without -T)")
			return nil, nil
		}

		fingerprint := pubkeyFingerprint(sess)
		fallback := sshUser(sess)
		// Pre-existing GitHub link (if any) — the lobby will start the user
		// as @<ghlogin> and unlock chat without requiring /auth again.
		ghLogin := ""
		if authSvc != nil {
			ghLogin = authSvc.Lookup(fingerprint)
		}

		// Per-session renderer reflects this client's terminal capabilities
		// (truecolor / 256 / 16 / no-color). Styles built through it downgrade
		// gracefully instead of dumping raw 24-bit escapes on terminals that
		// can't render them.
		styles := theme.New(bm.MakeRenderer(sess), theme.Default())

		// SSH sessions get LocalMode=false — /radio shows the install nudge
		// instead of offering downloads, since audio output can't traverse SSH.
		// Downloader and player are nil because audio doesn't make sense on
		// the server side; app.New will substitute a stub player so the
		// screen still renders correctly.
		a := app.New(styles, fallback, fingerprint, ghLogin, world, authSvc, data, radioClient, nil, nil, app.LocalMode(false))

		// Cleanup on session end: SSH closes ctx -> unsubscribe + free resources.
		go func() {
			<-sess.Context().Done()
			a.Cleanup()
		}()

		return a, []tea.ProgramOption{tea.WithAltScreen()}
	}
}

// pubkeyFingerprint returns the SHA256 fingerprint of the session's public
// key (e.g. "SHA256:abcdef..."), or "" if the client didn't present one.
func pubkeyFingerprint(sess ssh.Session) string {
	pk := sess.PublicKey()
	if pk == nil {
		return ""
	}
	return gossh.FingerprintSHA256(pk)
}

// sshUser derives a display name from the SSH session. Prefers the SSH user
// (`ssh foo@host` -> "@foo"), falling back to a short prefix of the public key
// fingerprint for unauthenticated sessions.
func sshUser(sess ssh.Session) string {
	if u := strings.TrimSpace(sess.User()); u != "" && u != "anonymous" {
		return "@" + sanitizeNick(u)
	}
	if pk := sess.PublicKey(); pk != nil {
		fp := gossh.FingerprintSHA256(pk)
		// fp looks like "SHA256:abcdef..."; trim prefix and shorten.
		if i := strings.IndexByte(fp, ':'); i >= 0 {
			fp = fp[i+1:]
		}
		if len(fp) > 8 {
			fp = fp[:8]
		}
		return "@guest-" + strings.ToLower(fp)
	}
	return "@guest"
}

// sanitizeNick keeps the displayed user predictable: lowercase, alnum/_/-, max
// 24 chars. Don't trust raw SSH usernames in the chat surface.
func sanitizeNick(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
		if b.Len() >= 24 {
			break
		}
	}
	if b.Len() == 0 {
		return "guest"
	}
	return b.String()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
