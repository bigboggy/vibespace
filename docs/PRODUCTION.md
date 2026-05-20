# Production setup

End-to-end checklist for running the WS-backed vibespace in production: GitHub OAuth app → server binary → Caddy → DNS → client install. Assumes you already control `vibespace.sh` (or your own domain) with a public-facing Linux box reachable on ports 80, 443, and `VIBESPACE_ADDR` (default `:2222`).

## 1. GitHub OAuth app

Both the server (token verification) and the client (device flow) point at the same GitHub OAuth app. Create one once:

1. Go to https://github.com/settings/applications/new
2. Fill in:
   - **Application name**: `vibespace`
   - **Homepage URL**: `https://vibespace.sh`
   - **Authorization callback URL**: anything (device flow doesn't use it — `https://vibespace.sh` is fine)
3. After creating it, on the app's settings page check **"Enable Device Flow"**.
4. Copy the **Client ID** (something like `Iv1.abc123...`).

There is no client secret to manage — device flow doesn't use one.

Treat the client ID as semi-public: it's safe to bake into release binaries, and it has to be reachable by every local install. The server uses it only as a label when verifying the user's access token against `api.github.com/user`.

## 2. Server side

### Env vars

On `vibespace.sh`, set in the systemd unit (`/etc/systemd/system/vibespace.service` or wherever your deploy lives):

```ini
[Service]
Environment="VIBESPACE_ADDR=:2222"
Environment="VIBESPACE_HTTP_ADDR=127.0.0.1:8080"
Environment="VIBESPACE_GH_CLIENT_ID=Iv1.abc123..."
Environment="VIBESPACE_IDENTITY_PATH=/var/lib/vibespace/identities.json"
Environment="VIBESPACE_DATA_PATH=/var/lib/vibespace/vibespace.db"
Environment="VIBESPACE_RADIO_CACHE=/var/lib/vibespace/radio-cache"
Environment="VIBESPACE_HOSTKEY=/var/lib/vibespace/hostkey"
ExecStart=/usr/local/bin/vibespace-server
WorkingDirectory=/var/lib/vibespace
Restart=on-failure
User=vibespace
Group=vibespace
```

Key points:

- **`VIBESPACE_HTTP_ADDR=127.0.0.1:8080`** — bind to loopback. Caddy will reverse-proxy to it; the WS port never directly faces the internet, so no firewall rules needed for `:8080`.
- **`VIBESPACE_GH_CLIENT_ID`** is required for the WS endpoint to come up. Without it the server still serves SSH but the WS handler stays off (and `runHTTPServer` logs a clear "disabled" line at boot).
- Run as a dedicated `vibespace` user; the data dir (`/var/lib/vibespace`) should be `0700`-owned.

Reload + start:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now vibespace
sudo systemctl status vibespace
journalctl -u vibespace -f
```

You should see three lines in the log at startup:

```
data store at /var/lib/vibespace/vibespace.db
github auth enabled, identity store at /var/lib/vibespace/identities.json
vibespace-server WS listening on 127.0.0.1:8080
vibespace-server listening on :2222
```

### Caddy

Caddy terminates TLS and reverse-proxies `/ws` to the binary. Put this in `/etc/caddy/Caddyfile`:

```caddy
vibespace.sh {
    reverse_proxy /ws 127.0.0.1:8080
    reverse_proxy /healthz 127.0.0.1:8080

    # Optional: keep something at the root so vibespace.sh isn't blank.
    # Without a root handler Caddy returns 404 — fine for a backend-only host.
    respond / "vibespace — ssh vibespace.sh -p 2222 to peek, or install: curl https://vibespace.sh/install.sh | bash" 200
}
```

Reload:

```sh
sudo systemctl reload caddy
```

The WebSocket upgrade works through `reverse_proxy` without any extra directives — Caddy handles `Connection: Upgrade` automatically.

### Firewall

Only three ports need to be public:

- **`80`** — Caddy uses it for the ACME HTTP-01 challenge.
- **`443`** — Caddy serves the WS endpoint here.
- **`2222`** (or whatever `VIBESPACE_ADDR` is) — the SSH "glimpse" path.

`127.0.0.1:8080` stays internal. If you're using `ufw`:

```sh
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 2222/tcp
```

### Deploy

`scripts/deploy.sh` already cross-compiles linux/amd64 + scp's + restarts the service. No changes needed — the new HTTP listener comes up automatically once you set `VIBESPACE_HTTP_ADDR` in the systemd unit.

```sh
scripts/deploy.sh
```

## 3. Verify the server is up

From any machine:

```sh
curl https://vibespace.sh/healthz
# → ok
```

If you get `ok`, Caddy → systemd → server is wired end-to-end. The WS endpoint is sitting behind `wss://vibespace.sh/ws`.

```sh
ssh vibespace.sh -p 2222
# → drops you into the SSH TUI
```

Both surfaces share the same `LocalHub`, so sending a message from a binary user (next section) will show up in this SSH session too.

## 4. Client release

The locally-installed binary needs the same `VIBESPACE_GH_CLIENT_ID`. Two ways to deliver it:

### Option A — bake it in at build time (recommended for public releases)

Edit `scripts/release.sh` so the build line passes the client id via `-ldflags -X`:

```sh
LDFLAGS="-s -w -X main.version=$VERSION -X main.bakedClientID=Iv1.abc123..."
```

Then in `main.go`, fall back to `bakedClientID` when the env var is empty:

```go
var bakedClientID = "" // overridden by ldflags

func ghClientID() string {
    if v := os.Getenv("VIBESPACE_GH_CLIENT_ID"); v != "" {
        return v
    }
    return bakedClientID
}
```

This is how users get a frictionless `curl install.sh | bash` experience — they don't need to know about env vars.

### Option B — require the env var

Skip the ldflags hack and document that users must `export VIBESPACE_GH_CLIENT_ID=...` before first run. Reasonable for a private/internal install; not what you want for a public release.

### Cut the release

```sh
scripts/release.sh v0.4.0
```

This builds for `darwin/{amd64,arm64}` + `linux/{amd64,arm64}`, computes checksums, tags, pushes, and `gh release create`s with all assets. The same release flow already powers `scripts/install.sh`.

## 5. First-time client install

User runs:

```sh
curl -fsSL https://raw.githubusercontent.com/bigboggy/vibespace/main/scripts/install.sh | bash
vibespace
```

On first launch the binary:
1. Sees `auth.json` is missing.
2. Prints the device-flow code + verification URL to the terminal.
3. Best-effort opens the URL in their browser.
4. Polls `github.com/login/oauth/access_token` until they authorize.
5. Persists `{token, login}` to `~/Library/Application Support/vibespace/auth.json` (mode 0600).
6. Dials `wss://vibespace.sh/ws`, sends `{t:"hello",token}`, receives state, joins.

On every subsequent launch it skips straight to step 6 with the cached token. If the server returns "rejected auth" (token revoked), it drops the cache and re-runs the device flow once.

## 6. Operational notes

### Updating the binary

`scripts/release.sh vX.Y.Z` cuts a new release; existing users re-run `install.sh` (it's idempotent — refreshes the binary and rewires the scheduler).

### Updating the server

`scripts/deploy.sh` swaps the binary on `vibespace.sh` and `systemctl restart`s. **Important**: the LocalHub is in-memory only — restarting the server drops all chat history. Tell users in advance (e.g. `/system going down for an update` via a server-side admin command, if you ever add one).

### Rotating the GH OAuth app

If you ever need to rotate the client id:
1. Create a new GitHub OAuth app + enable device flow.
2. Bake the new client id into a fresh release, push.
3. Update the server's `VIBESPACE_GH_CLIENT_ID` and restart. Old tokens (issued under the previous app) will fail verification and clients will re-run the device flow against the new app on next launch.

### Monitoring

The minimum signal: `journalctl -u vibespace -f`. Things to watch for:
- `ws upgrade: ...` — non-WS clients hitting `/ws`; expected occasionally, but spikes mean something's misrouted.
- `ws serve: ...` — fatal server error. The systemd `Restart=on-failure` will bounce it.
- Token-verification 401s show up as `github: 401` strings in the log; expected when revoked tokens reconnect.

For more, point Prometheus at `/healthz` (it returns 200 + "ok") and add `httptest`-style instrumentation if you outgrow it.

### Backups

The only thing worth backing up is `VIBESPACE_DATA_PATH` (`vibespace.db`) — it holds profile, post, friend, guestbook, and leaderboard data. Identity store (`identities.json`) is regenerable if you re-`/auth`; the hot-tier token tracker resyncs automatically. Run a daily sqlite-aware backup:

```sh
sqlite3 /var/lib/vibespace/vibespace.db ".backup '/var/backups/vibespace/$(date +%F).db'"
```

That's it. End-to-end setup is GH OAuth + systemd unit + Caddyfile + DNS A record. Everything else is the release pipeline you already had.
