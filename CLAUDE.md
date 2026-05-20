# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

**vibespace** — a TUI IRC-style chat lobby for devs, built with bubbletea / lipgloss / wish. Ships from one codebase with two entrypoints that share state over the network:

- **Local binary** (`./main.go`): the primary client. Connects over WebSocket to the backend (defaults to `wss://vibespace.sh/ws`) so chat is shared with every other binary user and every SSH "glimpse" session. Falls back to an in-process `LocalHub` when no backend is configured / reachable — radio playback and `report` still work offline.
- **Server** (`./cmd/server/main.go`): a wish SSH server **and** an HTTP/WebSocket API in one process. Both surfaces front the same in-memory `LocalHub`, so SSH peekers and binary users see each other's messages in real time.

Go 1.24+. No test suite, no linter config — `go vet ./...` and `go build ./...` are the only verifiers in tree.

> Heads-up: `AGENTS.md` is stale (describes a previous project called "gitstatus"). Ignore it; this file supersedes it.

## Common commands

```bash
go run .                   # run local mode
go run ./cmd/server        # run SSH server on :2222 (needs VIBESPACE_GH_CLIENT_ID for /auth)
go build ./...             # verify everything compiles
go vet ./...               # only available lint
scripts/deploy.sh          # cross-compile linux/amd64, scp + systemctl restart on vibespace.sh
scripts/release.sh vX.Y.Z  # tag, build 4 platforms, gh release create
```

The Go module path is `github.com/bigboggy/vibespace`. All internal imports go through this prefix.

## Architecture

### Hub interface, two implementations

`internal/hub` defines the `Hub` interface (the contract every screen depends on) plus two implementations:

- **`*LocalHub`** (`hub.go`): in-process, holds the channels/messages/presence in memory. The server's SSH lobby and the offline-mode local binary use this directly.
- **`*RemoteHub`** (`remote.go`): a network-backed mirror. The local binary calls `hub.Dial(ctx, url, token)` which opens a WebSocket to the server, sends `{t:"hello",token}`, receives a state snapshot, then keeps a local mirror in sync via streamed wire events. Read methods (`Messages`, `Online`, `ChannelNames`) serve from the mirror; write methods (`Post`, `CreateChannel`, `SetViewing`) send JSON envelopes over the WS.

`internal/hubwire` is the JSON-over-WebSocket wire shared by both sides. A single `Envelope` struct carries every message type; receivers dispatch on the `T` field. Types: `hello`, `welcome`, `state`, `post`, `create`, `view`, `msg`, `created`, `online`, `error`.

The server's WS handler (`cmd/server/ws.go`) bridges the wire to the `LocalHub`: client `post` → `LocalHub.Post`, hub `EventMessage` → wire `msg`, etc. Tokens are verified against `api.github.com/user` and cached for 10 minutes in `wsServer.tokenCache`.

### Two entrypoints, one app

`main.go` and `cmd/server/main.go` both build the same `app.App` (`internal/app/app.go`):

- **Local mode** (`main.go`) calls `connectBackend(...)` at startup. If `VIBESPACE_BACKEND` is reachable and `VIBESPACE_GH_CLIENT_ID` is set, it runs the GitHub device flow (or loads a cached token from `$config/vibespace/auth.json`), then `hub.Dial`s the WS endpoint and uses the returned `*RemoteHub`. Falls back to `hub.NewLocal()` on any failure — the rest of the app still runs (radio, profile, report).
- **Server mode** (`cmd/server/main.go`) creates one `*LocalHub` and one `*auth.Service` at startup, hands the LocalHub to both the wish SSH middleware (per-session `app.App`) and the HTTP WebSocket handler (`ws.go`). SSH identity comes from the SSH pubkey fingerprint (resolved via `auth.Service.Lookup`) or a sanitized `sess.User()`. WS identity comes from the GH access token's verified login.

### Hub is the only mutable chat state

The LocalHub is the only place that holds mutable chat state on the server. Sessions (SSH or WS-backed) read state during `View` and on every `hub.Event` they receive via `Subscribe()`. Sessions never hold their own copy of chat — they own only session-local UI state (input, scroll, history, active channel, identity).

`hub.Event` implements `tea.Msg`, so events flow straight through bubbletea. Subscribers re-read the hub on each event rather than trusting the event payload — `broadcast` drops on full channel buffer, which is safe because of this re-read pattern. The RemoteHub's wire envelopes DO carry payload (so clients don't need a round-trip per event), but the lobby still re-reads on each event for consistency with the SSH path.

### Screen interface + Navigate messages

`internal/screens/screen.go` defines the `Screen` interface (`Init/Update/View/Name/Title/HeaderContext/Footer/InputFocused`). Screens **never import each other**. Cross-screen flow happens by emitting `screens.NavigateMsg{Target: ...}` (via `screens.Navigate(target)`), which `internal/app/router.go` catches and dispatches. The dependency graph is a star: `app` at the center, screens as leaves.

Two screens currently exist:
- `screens/intro` — boot animation, emits `Navigate(LobbyID)` when done
- `screens/lobby` — chat, slash commands, autocomplete, `/auth` modal, `/theme` picker

### App router

`internal/app/router.go` handles three concerns:
1. `tea.WindowSizeMsg` is broadcast to **every** screen (not just the active one) so cached layouts in inactive screens stay correct.
2. `hub.Event` is always routed to the lobby (the screen that owns the subscription), regardless of which screen is currently visible — otherwise events arriving during the intro would be swallowed.
3. Key handling: if the active screen's `InputFocused()` is true, the router forwards every key without interception. Otherwise it applies global bindings (`esc`/`q` → lobby, `ctrl+c` → quit).

### Identity and auth gating

`internal/auth` is a thin facade over `internal/github` (device flow) and `internal/identity` (a JSON file mapping SSH fingerprint → GitHub login). The lobby treats `auth.Service` as optional — pass `nil` and `/auth` disappears.

When `auth != nil` and the session is unauthenticated, the lobby is **gated**: only commands in `allowedWhenGated` (in `internal/screens/lobby/commands.go`) work. Everything else returns a "type /auth" hint. After successful auth, `meUser` flips to `@<ghLogin>`; subsequent connections from the same SSH key skip auth via `authSvc.Lookup(fingerprint)` at session start.

The identity store only persists `(fingerprint, login)` — **never access tokens**.

### Theme system

`internal/theme` holds a registry of palettes (`tokyonight` is default — see `theme.DefaultID`) and a `*Styles` value that pairs a theme with a per-session `lipgloss.Renderer`. Server-mode builds a fresh renderer per SSH session (`bm.MakeRenderer(sess)`) so truecolor/256/16-color clients all get appropriate downgrade. `/theme <id>` mutates `*Styles` in place — every subsequent render across all screens picks up the new theme.

### Slash commands

Defined in `internal/screens/lobby/commands.go`:
- `builtins` slice = canonical commands in autocomplete order
- `aliases` map = alternate names (e.g. `/exit` → `/quit`)
- `allowedWhenGated` map = whitelist for unauthenticated server sessions
- `/auth` and `/logout` are mutually exclusive — palette hides whichever doesn't match current auth state

## Server config (env vars)

| Var | Default | Purpose |
|-----|---------|---------|
| `VIBESPACE_ADDR` | `:2222` | SSH listen addr (use non-22 unless OpenSSH moved) |
| `VIBESPACE_HTTP_ADDR` | `:8080` | HTTP/WS listen addr (set empty to disable WS endpoint) |
| `VIBESPACE_HOSTKEY` | `.ssh/id_ed25519` | SSH host key path (auto-generated) |
| `VIBESPACE_GH_CLIENT_ID` | unset | GitHub OAuth client id; enables in-lobby `/auth` AND the WS bearer-token verification (without it, the WS endpoint stays off) |
| `VIBESPACE_IDENTITY_PATH` | `./identities.json` | fingerprint → GH login store |
| `VIBESPACE_DATA_PATH` | `./vibespace.db` | SQLite store for profiles/posts/friends/guestbook |

Without `VIBESPACE_GH_CLIENT_ID`, the SSH server still runs but `/auth` is disabled, no SSH gating is applied, and the WebSocket endpoint stays off.

## Local binary config (env vars)

| Var | Default | Purpose |
|-----|---------|---------|
| `VIBESPACE_BACKEND` | `wss://vibespace.sh/ws` | backend WebSocket URL; set to `off` (or fail to set client id) to run offline-only |
| `VIBESPACE_GH_CLIENT_ID` | unset | required for backend mode — runs the device flow on first launch |

The cached GH token + login live at `$config/vibespace/auth.json` (mode 0600). The local SQLite, identity store, and radio cache sit in the same directory. On macOS that's `~/Library/Application Support/vibespace/`; on Linux `$XDG_CONFIG_HOME/vibespace/`.

## Conventions

- Min terminal: 80×22 (`ui.MinWidth` / `ui.MinHeight`). Below that, the app renders a "too small" message.
- All chat state mutations go through `hub` methods. Don't mutate channel/message slices from outside the hub.
- Per-session resources (hub subscription) are released via `App.Cleanup()` — called from the SSH context-done goroutine in `cmd/server/main.go`.
- No tests exist. If adding them, target `hub` (concurrent subscribe/broadcast), `identity` (file IO), and `lobby` command parsing.
