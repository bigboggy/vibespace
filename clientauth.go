// clientauth.go — device-flow bootstrap for the local binary's backend
// connection. Lives alongside main.go because it's strictly a startup-time
// concern: get a token, persist it, hand it to hub.Dial.
//
// The token JSON file (mode 0600) sits under the per-user config dir. We
// store the token only — no refresh-token dance because GitHub device-flow
// tokens don't expire on a fixed schedule. If a token is revoked or rotated
// the next /user call returns 401 and we run the flow again.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bigboggy/vibespace/internal/github"
)

// authCache is the on-disk shape of the saved token.
type authCache struct {
	Token   string    `json:"token"`
	Login   string    `json:"login"`
	SavedAt time.Time `json:"saved_at"`
}

// loadToken returns a previously-saved token (and login) if the file exists
// and parses. Empty strings + nil on any failure — callers fall through to
// the device flow.
func loadToken(path string) (token, login string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var c authCache
	if json.Unmarshal(b, &c) != nil {
		return "", ""
	}
	return c.Token, c.Login
}

// saveToken atomically writes the token+login to disk with 0600 perms.
func saveToken(path, token, login string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(authCache{Token: token, Login: login, SavedAt: time.Now()}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// clearToken deletes the cached token. Used when the backend rejects it so
// the next launch re-runs the device flow.
func clearToken(path string) {
	_ = os.Remove(path)
}

// runDeviceFlow walks the user through the GitHub device authorization. It
// blocks until they finish (or hit ctrl+c). On success returns the access
// token + resolved login.
//
// Print-driven UX: we're invoked before bubbletea starts, so plain stdout
// is the cleanest surface. Tries to xdg-open / open the verification URL,
// but the printed code/URL is the source of truth.
func runDeviceFlow(ctx context.Context, clientID string) (token, login string, err error) {
	flow := &github.DeviceFlow{
		ClientID: clientID,
		Scope:    "read:user",
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}

	start, err := flow.Start(ctx)
	if err != nil {
		return "", "", fmt.Errorf("start device flow: %w", err)
	}

	fmt.Println()
	fmt.Println("  Link your GitHub account to vibespace.")
	fmt.Println()
	fmt.Printf("  1. Open %s\n", start.VerificationURI)
	fmt.Printf("  2. Enter code: %s\n", start.UserCode)
	fmt.Println()
	fmt.Println("  Waiting for you to authorize on github.com…")
	fmt.Println()

	// Best-effort browser open. Failures are silent.
	openBrowser(start.VerificationURI)

	token, err = flow.Poll(ctx, start.DeviceCode, start.Interval)
	if err != nil {
		return "", "", fmt.Errorf("poll: %w", err)
	}
	login, err = flow.UserLogin(ctx, token)
	if err != nil {
		return "", "", fmt.Errorf("resolve login: %w", err)
	}
	fmt.Printf("  ✓ linked as @%s\n\n", login)
	return token, login, nil
}

// openBrowser is a fire-and-forget "open the verification URL" — uses the
// platform's standard helper. We don't wait for it or care if it fails; the
// printed URL is the actual instruction to the user.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		return
	}
	_ = cmd.Start()
}

// authPath is where the cached token lives, under the per-user config dir.
func authPath(configDir string) string {
	return filepath.Join(configDir, "auth.json")
}

// ErrAuthRejected reports that the backend rejected the supplied token. The
// caller drops the cached file and re-runs the device flow.
var ErrAuthRejected = errors.New("backend rejected auth token")

// looksLikeAuthError sniffs the WS dial error for the codes the server
// returns when auth fails. We don't have structured error access from the
// coder/websocket close frame, so substring-match the message — it's
// stable enough ("server rejected auth: ...").
func looksLikeAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "rejected auth") || strings.Contains(s, "status = StatusPolicyViolation")
}
