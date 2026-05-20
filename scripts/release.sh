#!/usr/bin/env bash
# release.sh — cut a new vibespace release
# Usage: scripts/release.sh v0.3.0 [--draft] [--notes "..."]
#
# Requires: go, git, gh (https://cli.github.com), authenticated `gh auth login`.

set -euo pipefail

BINARY="vibespace"
PLATFORMS=("darwin/amd64" "darwin/arm64" "linux/amd64" "linux/arm64")

info()  { printf "\033[32m>> %s\033[0m\n" "$*"; }
warn()  { printf "\033[33m!! %s\033[0m\n" "$*"; }
error() { printf "\033[31m!! %s\033[0m\n" "$*" >&2; }

# ── Args ──────────────────────────────────────────────────────────────────────
if [[ $# -lt 1 ]]; then
  error "Usage: $0 <version> [--draft] [--notes \"...\"]"
  error "Example: $0 v0.3.0"
  exit 1
fi

VERSION="$1"; shift
DRAFT=""
NOTES=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --draft) DRAFT="--draft"; shift ;;
    --notes) NOTES="$2"; shift 2 ;;
    *) error "Unknown flag: $1"; exit 1 ;;
  esac
done

if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?$ ]]; then
  error "Version must look like v1.2.3 or v1.2.3-rc1 (got: $VERSION)"
  exit 1
fi

# ── Preflight ────────────────────────────────────────────────────────────────
REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

command -v gh >/dev/null || { error "gh CLI not found. Install: https://cli.github.com"; exit 1; }
command -v go >/dev/null || { error "go not found"; exit 1; }

if ! git diff-index --quiet HEAD --; then
  error "Working tree is dirty. Commit or stash first."
  exit 1
fi

BRANCH="$(git rev-parse --abbrev-ref HEAD)"
if [[ "$BRANCH" != "main" ]]; then
  warn "Not on main (current: $BRANCH)."
  read -rp "Continue anyway? [y/N] " ans
  [[ "$ans" =~ ^[Yy]$ ]] || exit 1
fi

if git rev-parse "$VERSION" >/dev/null 2>&1; then
  error "Tag $VERSION already exists."
  exit 1
fi

# ── Build ────────────────────────────────────────────────────────────────────
DIST="$REPO_ROOT/dist/$VERSION"
rm -rf "$DIST"
mkdir -p "$DIST"

# Load env file if it exists (e.g. .env.release)
ENV_FILE="$REPO_ROOT/.env.release"
if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

if [[ -z "${VIBESPACE_GH_CLIENT_ID:-}" ]]; then
  error "VIBESPACE_GH_CLIENT_ID not set — required for baking into release"
  error "Create $ENV_FILE with: VIBESPACE_GH_CLIENT_ID=Ov1.xxx..."
  exit 1
fi
LDFLAGS="-s -w -X main.version=$VERSION -X main.bakedClientID=$VIBESPACE_GH_CLIENT_ID"

for platform in "${PLATFORMS[@]}"; do
  OS="${platform%/*}"
  ARCH="${platform#*/}"
  STAGE="$DIST/stage-$OS-$ARCH"
  mkdir -p "$STAGE"

  info "Building $BINARY for $OS/$ARCH ..."
  GOOS="$OS" GOARCH="$ARCH" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$STAGE/$BINARY" .

  ARCHIVE="$BINARY-$OS-$ARCH.tar.gz"
  tar -C "$STAGE" -czf "$DIST/$ARCHIVE" "$BINARY"
  rm -rf "$STAGE"
done

# ── Checksums ────────────────────────────────────────────────────────────────
info "Computing checksums ..."
( cd "$DIST" && shasum -a 256 *.tar.gz > checksums.txt )

# ── Tag & push ───────────────────────────────────────────────────────────────
info "Tagging $VERSION ..."
git tag -a "$VERSION" -m "$VERSION"

info "Pushing tag to origin ..."
git push origin "$VERSION"

# ── GitHub release ───────────────────────────────────────────────────────────
info "Creating GitHub release ..."
RELEASE_ARGS=("$VERSION" "$DIST"/*.tar.gz "$DIST/checksums.txt" --title "$VERSION")
[[ -n "$DRAFT" ]] && RELEASE_ARGS+=("$DRAFT")
if [[ -n "$NOTES" ]]; then
  RELEASE_ARGS+=(--notes "$NOTES")
else
  RELEASE_ARGS+=(--generate-notes)
fi

gh release create "${RELEASE_ARGS[@]}"

info "Released $VERSION"
info "Assets in $DIST"