// Package radio is the radio client. There's exactly one show queued up at a
// time — the manifest points at a single MP3, the client downloads it, the
// player loops it. Switching shows is just publishing a new manifest.
//
// The manifest lives at a fixed URL (DefaultManifestURL, overridable via the
// VIBESPACE_RADIO_MANIFEST env var). It's cached on disk so cold-start
// without network still shows the last-known show.
package radio

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultManifestURL points at the radio manifest hosted on a sticky GitHub
// Releases tag. Publishing a new show means re-uploading the manifest asset
// on this tag (and the new .mp3 on its own release tag).
const DefaultManifestURL = "https://github.com/bigboggy/vibespace/releases/download/radio-manifest/manifest.json"

// CacheTTL is how long a fetched manifest is treated as fresh. After this
// expires the next Load() refetches; until then it serves the cached copy.
const CacheTTL = 1 * time.Hour

// fetchTimeout caps how long a manifest fetch can hang. Manifest is tiny so
// this is generous on purpose — we'd rather wait than show "no radio" on a
// slow connection.
const fetchTimeout = 15 * time.Second

// Manifest is the entire radio catalog: one show, plus when it was published.
// Title is derived from the URL's filename (see Title()). Updated is optional;
// when present the screen renders "updated 4h ago".
type Manifest struct {
	URL     string    `json:"url"`
	Updated time.Time `json:"updated,omitempty"`
}

// HasShow reports whether the manifest points at a playable URL.
func (m *Manifest) HasShow() bool {
	return m != nil && strings.TrimSpace(m.URL) != ""
}

// Filename returns the basename of the manifest URL's path, e.g.
// "late-night-coding.mp3". Returns "" if the URL is empty or unparseable.
func (m *Manifest) Filename() string {
	if !m.HasShow() {
		return ""
	}
	// Strip query/fragment by cutting at '?' / '#'. Avoids dragging in
	// net/url just for this; the URLs we serve don't need full parsing.
	u := m.URL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return path.Base(u)
}

// Title returns a human-readable name derived from the filename:
// "late-night_coding.mp3" → "Late Night Coding".
func (m *Manifest) Title() string {
	name := m.Filename()
	if name == "" {
		return ""
	}
	// Strip extension.
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	// Normalize separators to spaces.
	name = strings.NewReplacer("-", " ", "_", " ", ".", " ").Replace(name)
	// Collapse runs of whitespace.
	name = strings.Join(strings.Fields(name), " ")
	// Title-case each word.
	parts := strings.Split(name, " ")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// Client fetches the manifest with on-disk caching. One *Client per session
// is fine; the disk cache makes a shared instance unnecessary.
type Client struct {
	url     string // manifest URL
	cache   string // path to cached manifest JSON
	http    *http.Client
	mu      sync.Mutex
	current *Manifest
	fetched time.Time
}

// NewClient returns a Client. cacheDir is the directory where the cached
// manifest lives (typically <user-config>/vibespace/radio). url defaults to
// DefaultManifestURL if empty; override for development.
func NewClient(cacheDir, url string) *Client {
	if url == "" {
		url = DefaultManifestURL
	}
	return &Client{
		url:   url,
		cache: filepath.Join(cacheDir, "manifest.json"),
		http:  &http.Client{Timeout: fetchTimeout},
	}
}

// Load returns the current manifest. If a cached copy exists and is fresher
// than CacheTTL it's used; otherwise the remote is fetched and the cache is
// updated. Network failures fall back to the cached copy even if stale —
// stale data beats no data for a UX-facing screen.
func (c *Client) Load() (*Manifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// In-memory hit (covers repeat Load calls within a session).
	if c.current != nil && time.Since(c.fetched) < CacheTTL {
		return c.current, nil
	}

	// Disk hit (covers cold-start within TTL).
	if disk, age, ok := c.readDisk(); ok && age < CacheTTL {
		c.current = disk
		c.fetched = time.Now().Add(-age)
		return disk, nil
	}

	// Network refresh — but fall back to disk on failure even if disk is stale.
	fresh, err := c.fetch()
	if err != nil {
		if disk, _, ok := c.readDisk(); ok {
			c.current = disk
			return disk, nil
		}
		return nil, err
	}
	if err := c.writeDisk(fresh); err != nil {
		// Don't fail the whole Load on a cache-write failure; the user can still
		// see today's show, just not offline.
		_ = err
	}
	c.current = fresh
	c.fetched = time.Now()
	return fresh, nil
}

// Refresh forces a network fetch, ignoring the TTL. Used by an explicit
// "reload" key in the radio screen.
func (c *Client) Refresh() (*Manifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh, err := c.fetch()
	if err != nil {
		return nil, err
	}
	_ = c.writeDisk(fresh)
	c.current = fresh
	c.fetched = time.Now()
	return fresh, nil
}

// FetchedAt reports when the in-memory manifest was last refreshed. Returns
// zero time before the first successful Load.
func (c *Client) FetchedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetched
}

// fetch performs the actual HTTP request and JSON decode.
func (c *Client) fetch() (*Manifest, error) {
	req, err := http.NewRequest(http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("radio manifest: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var m Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("radio manifest: parse: %w", err)
	}
	return &m, nil
}

// readDisk returns the cached manifest and its age. ok=false means no cache
// (or the cache is unreadable / corrupt — treated the same).
func (c *Client) readDisk() (*Manifest, time.Duration, bool) {
	info, err := os.Stat(c.cache)
	if err != nil {
		return nil, 0, false
	}
	b, err := os.ReadFile(c.cache)
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return nil, 0, false
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, 0, false
	}
	return &m, time.Since(info.ModTime()), true
}

// writeDisk atomically replaces the cached manifest with m.
func (c *Client) writeDisk(m *Manifest) error {
	if err := os.MkdirAll(filepath.Dir(c.cache), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.cache), "manifest-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, c.cache)
}

// FormatDuration renders a duration like "1:23:45" or "12:34". Kept here so
// the Phase 2 download progress bar can reuse it.
func FormatDuration(sec int) string {
	if sec < 0 {
		sec = 0
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// FormatSize renders a byte count as "47 MB" / "1.2 GB".
func FormatSize(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	const unit = 1024
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
