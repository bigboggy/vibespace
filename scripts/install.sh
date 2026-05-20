#!/usr/bin/env bash
# install.sh — one-shot installer for vibespace + the background token tracker.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/bigboggy/vibespace/main/scripts/install.sh | bash
#
# What it does:
#   1. Detects your OS/arch and downloads the matching release binary from
#      github.com/bigboggy/vibespace/releases (latest).
#   2. Verifies the SHA-256 checksum.
#   3. Installs to $VIBESPACE_INSTALL_DIR (default: ~/.local/bin).
#   4. Sets up a periodic auto-report (every minute) using the best
#      scheduler available on your system: systemd --user timer → launchd
#      LaunchAgent → crontab. Falls back to "no scheduler, run manually"
#      with clear instructions when none of those exist.
#   5. Attempts one immediate upload so you see the result right away.
#
# Re-runnable. Already installed? It refreshes the binary, rewrites the
# scheduler unit, and re-runs the initial upload — nothing destructive.
#
# Environment overrides:
#   VIBESPACE_INSTALL_DIR        where the binary lands (default: ~/.local/bin)
#   VIBESPACE_SERVER             upload target (default: vibespace.sh)
#   VIBESPACE_VERSION            pin a specific release tag (default: latest)
#   VIBESPACE_NO_SCHEDULE        set to "1" to skip scheduler setup
#   VIBESPACE_GH_CLIENT_ID       GitHub OAuth app client ID (baked into binary at release time)

set -euo pipefail

REPO="bigboggy/vibespace"
BINARY="vibespace"
INSTALL_DIR="${VIBESPACE_INSTALL_DIR:-$HOME/.local/bin}"
SERVER="${VIBESPACE_SERVER:-vibespace.sh}"
VERSION="${VIBESPACE_VERSION:-latest}"
CLIENT_ID="${VIBESPACE_GH_CLIENT_ID:-}"

# ── pretty output ──────────────────────────────────────────────────────────
info()  { printf "\033[1;32m==>\033[0m %s\n" "$*"; }
warn()  { printf "\033[1;33m!!\033[0m  %s\n" "$*"; }
err()   { printf "\033[1;31m!!\033[0m  %s\n" "$*" >&2; }
die()   { err "$@"; exit 1; }

# ── prereq checks ──────────────────────────────────────────────────────────
for cmd in curl tar uname; do
  command -v "$cmd" >/dev/null 2>&1 || die "missing required tool: $cmd"
done
if ! command -v ssh >/dev/null 2>&1; then
  warn "ssh not found — vibespace report needs it to upload to the server"
fi

# ── platform detection ────────────────────────────────────────────────────
detect_os() {
  case "$(uname -s)" in
    Linux*)  echo linux ;;
    Darwin*) echo darwin ;;
    *)       die "unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo amd64 ;;
    arm64|aarch64) echo arm64 ;;
    *)             die "unsupported arch: $(uname -m)" ;;
  esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"
ARCHIVE="${BINARY}-${OS}-${ARCH}.tar.gz"

# ── download ──────────────────────────────────────────────────────────────
if [[ "$VERSION" == "latest" ]]; then
  BASE_URL="https://github.com/$REPO/releases/latest/download"
else
  BASE_URL="https://github.com/$REPO/releases/download/$VERSION"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$INSTALL_DIR"

install_from_release() {
  info "trying release download: $BASE_URL/$ARCHIVE"
  curl -fsSL --retry 3 -o "$TMP/$ARCHIVE" "$BASE_URL/$ARCHIVE" || return 1
  curl -fsSL --retry 3 -o "$TMP/checksums.txt" "$BASE_URL/checksums.txt" 2>/dev/null \
    || warn "no checksums.txt — skipping integrity check"

  if [[ -f "$TMP/checksums.txt" ]]; then
    local expected=""
    while IFS= read -r line; do
      [[ "$line" == *"$ARCHIVE" ]] && expected="${line%% *}"
    done < "$TMP/checksums.txt"
    if [[ -n "$expected" ]]; then
      local actual=""
      if command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$TMP/$ARCHIVE" | awk '{print $1}')"
      elif command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$TMP/$ARCHIVE" | awk '{print $1}')"
      fi
      if [[ -n "$actual" && "$actual" != "$expected" ]]; then
        die "checksum mismatch: expected $expected, got $actual"
      fi
      [[ -z "$actual" ]] && warn "no sha256 tool — skipping integrity check"
    fi
  fi

  tar -C "$TMP" -xzf "$TMP/$ARCHIVE"
  install -m 0755 "$TMP/$BINARY" "$INSTALL_DIR/$BINARY"
  return 0
}

install_from_go() {
  command -v go >/dev/null 2>&1 || return 1
  info "no release for $OS/$ARCH — falling back to \`go install\`"
  local ref="@latest"
  [[ "$VERSION" != "latest" ]] && ref="@$VERSION"
  GOBIN="$INSTALL_DIR" go install "github.com/${REPO}${ref}"
}

if ! install_from_release; then
  warn "release download failed — is $VERSION published for $OS/$ARCH?"
  install_from_go || die "no release available and 'go' not installed; install Go from https://go.dev/dl/ then re-run"
fi
info "installed $INSTALL_DIR/$BINARY"

# Warn if it's not on PATH.
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) warn "$INSTALL_DIR is not on your \$PATH — add this to your shell rc:"
     printf "        export PATH=\"%s:\$PATH\"\n" "$INSTALL_DIR" ;;
esac

# ── scheduler setup ───────────────────────────────────────────────────────
schedule_systemd() {
  command -v systemctl >/dev/null 2>&1 || return 1
  # systemd --user only works when there's an active user session bus. A
  # quick probe — `systemctl --user status` exits 0 (or 3 = "no units") on
  # a live manager and 1 / 127 when the user bus isn't running.
  systemctl --user status >/dev/null 2>&1
  rc=$?
  [[ $rc -eq 0 || $rc -eq 3 ]] || return 1

  local unit_dir="$HOME/.config/systemd/user"
  mkdir -p "$unit_dir"

  cat > "$unit_dir/vibespace-report.service" <<EOF
[Unit]
Description=Vibespace token usage reporter
After=network-online.target

[Service]
Type=oneshot
Environment=VIBESPACE_SERVER=$SERVER
Environment=VIBESPACE_GH_CLIENT_ID=$VIBESPACE_GH_CLIENT_ID
ExecStart=$INSTALL_DIR/$BINARY report
EOF

  cat > "$unit_dir/vibespace-report.timer" <<EOF
[Unit]
Description=Run vibespace-report every minute

[Timer]
OnBootSec=30s
OnUnitActiveSec=1min
Persistent=true
Unit=vibespace-report.service

[Install]
WantedBy=timers.target
EOF

  systemctl --user daemon-reload
  systemctl --user enable --now vibespace-report.timer >/dev/null
  info "scheduler: systemd --user timer (every minute) — \`systemctl --user list-timers vibespace*\` to inspect"
  return 0
}

schedule_launchd() {
  [[ "$OS" == "darwin" ]] || return 1
  command -v launchctl >/dev/null 2>&1 || return 1

  local agent_dir="$HOME/Library/LaunchAgents"
  local plist="$agent_dir/sh.vibespace.report.plist"
  mkdir -p "$agent_dir"

  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>            <string>sh.vibespace.report</string>
  <key>ProgramArguments</key>
  <array>
    <string>$INSTALL_DIR/$BINARY</string>
    <string>report</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>VIBESPACE_SERVER</key>     <string>$SERVER</string>
    <key>VIBESPACE_GH_CLIENT_ID</key> <string>$CLIENT_ID</string>
    <key>PATH</key>                 <string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>
  </dict>
  <key>StartInterval</key>    <integer>60</integer>
  <key>RunAtLoad</key>        <true/>
  <key>StandardErrorPath</key> <string>$HOME/Library/Logs/vibespace-report.log</string>
  <key>StandardOutPath</key>   <string>$HOME/Library/Logs/vibespace-report.log</string>
</dict>
</plist>
EOF

  # Reload if already loaded (idempotent re-runs).
  launchctl unload "$plist" >/dev/null 2>&1 || true
  launchctl load   "$plist"
  info "scheduler: launchd LaunchAgent (every minute) — logs at ~/Library/Logs/vibespace-report.log"
  return 0
}

schedule_cron() {
  command -v crontab >/dev/null 2>&1 || return 1
  local line="* * * * * VIBESPACE_SERVER=$SERVER VIBESPACE_GH_CLIENT_ID=$CLIENT_ID $INSTALL_DIR/$BINARY report >/dev/null 2>&1 # vibespace-report"
  # Strip any prior vibespace-report line so re-runs stay clean.
  local existing
  existing="$(crontab -l 2>/dev/null | grep -v '# vibespace-report' || true)"
  printf "%s\n%s\n" "$existing" "$line" | crontab -
  info "scheduler: crontab (every minute) — \`crontab -l\` to inspect"
  return 0
}

if [[ "${VIBESPACE_NO_SCHEDULE:-0}" == "1" ]]; then
  info "scheduler setup skipped (VIBESPACE_NO_SCHEDULE=1)"
elif schedule_systemd; then
  :
elif schedule_launchd; then
  :
elif schedule_cron; then
  :
else
  warn "no scheduler available — invoke \`$BINARY report\` manually or rely on the in-TUI auto-trigger when you run \`$BINARY\`"
fi

# ── initial sync ──────────────────────────────────────────────────────────
info "trying an initial upload to $SERVER ..."
if VIBESPACE_SERVER="$SERVER" "$INSTALL_DIR/$BINARY" report; then
  info "first upload OK — leaderboard widget should populate within a minute"
else
  warn "first upload didn't succeed yet. Most likely your SSH key isn't"
  warn "linked to a GitHub login on the server. Fix:"
  printf "        ssh -p %s %s   # land in the TUI, then run /auth\n" \
    "${SERVER##*:}" "${SERVER%%:*}"
  warn "Once that's done the next scheduled run will succeed automatically."
fi

# ── persist client ID for the user ────────────────────────────────────────
if [[ -n "$CLIENT_ID" ]]; then
  for rc in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.bash_profile"; do
    [[ -f "$rc" ]] || continue
    if grep -q 'VIBESPACE_GH_CLIENT_ID' "$rc" 2>/dev/null; then
      continue
    fi
    printf "\nexport VIBESPACE_GH_CLIENT_ID=%s\n" "$CLIENT_ID" >> "$rc"
    info "added VIBESPACE_GH_CLIENT_ID to $rc"
    break
  done
fi

cat <<'EOF'

Installed. Next:
  • Connect: ssh vibespace.sh
  • Link your GitHub once: type /auth in the lobby
  • See yourself on the board: /leaderboard

The tracker runs every minute from now on. To uninstall the scheduler:
  systemd:  systemctl --user disable --now vibespace-report.timer
            rm ~/.config/systemd/user/vibespace-report.*
  launchd:  launchctl unload ~/Library/LaunchAgents/sh.vibespace.report.plist
            rm ~/Library/LaunchAgents/sh.vibespace.report.plist
  cron:     crontab -e   # delete the line tagged "# vibespace-report"
EOF
