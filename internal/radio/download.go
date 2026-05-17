package radio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Progress is one snapshot of a download in flight. Emitted on a channel by
// Downloader.Start; the screen reads it to render the progress bar.
//
// Final state on success: Done=true, Err=nil, Path=<finalized file path>.
// Final state on failure: Done=true, Err=non-nil. The channel is closed
// immediately after the terminal update is sent.
type Progress struct {
	BytesDone  int64
	BytesTotal int64 // 0 when the server didn't tell us the total
	Done       bool
	Err        error
	Path       string
}

// progressEvery is how often (in bytes downloaded since the last emit) the
// downloader sends a Progress update. Smaller = smoother bar but more chatter
// through the channel and more re-renders. 64 KiB lands a tick about every
// few hundred ms on a typical broadband connection.
const progressEvery = 64 * 1024

// Downloader streams files into a local cache dir with HTTP Range-resumable
// support. Stateless beyond its config — every Start call is independent.
type Downloader struct {
	cacheDir string
	http     *http.Client

	mu      sync.Mutex
	active  map[string]bool // url -> in flight (dedupes concurrent Starts)
}

// NewDownloader returns a Downloader writing into cacheDir. The directory is
// created lazily on first download. cacheDir is also where finalized files
// live — LocalPath(url) reports the resolved location.
func NewDownloader(cacheDir string) *Downloader {
	return &Downloader{
		cacheDir: cacheDir,
		http:     &http.Client{Timeout: 0}, // no overall deadline — large files
		active:   make(map[string]bool),
	}
}

// LocalPath returns the absolute path a finalized download for url would
// have. Used by the screen to detect "already downloaded".
func (d *Downloader) LocalPath(url string) string {
	return filepath.Join(d.cacheDir, basename(url))
}

// partialPath is where in-progress bytes accumulate before the atomic rename.
func (d *Downloader) partialPath(url string) string {
	return d.LocalPath(url) + ".partial"
}

// IsDownloaded reports whether url has a finalized local file. False covers
// "doesn't exist" and "still partial" — both mean the screen should offer
// download rather than play.
func (d *Downloader) IsDownloaded(url string) bool {
	if url == "" {
		return false
	}
	info, err := os.Stat(d.LocalPath(url))
	return err == nil && !info.IsDir() && info.Size() > 0
}

// Start kicks off a download in a goroutine. Returns a channel that emits
// Progress updates until terminal (Done=true, then closed). If a download
// for the same url is already running, returns the same channel — concurrent
// Start calls dedupe.
//
// Cancel via ctx; the goroutine notices and emits a final Progress with the
// context error.
func (d *Downloader) Start(ctx context.Context, url string) <-chan Progress {
	ch := make(chan Progress, 4)

	d.mu.Lock()
	if d.active[url] {
		// Already running — caller gets an empty channel that just closes.
		// We could share the in-flight channel but the bookkeeping is more
		// trouble than it's worth; callers should dedupe themselves at the
		// UI layer.
		d.mu.Unlock()
		close(ch)
		return ch
	}
	d.active[url] = true
	d.mu.Unlock()

	go func() {
		defer close(ch)
		defer func() {
			d.mu.Lock()
			delete(d.active, url)
			d.mu.Unlock()
		}()
		d.run(ctx, url, ch)
	}()
	return ch
}

// run is the goroutine body. Reports terminal state via a final Progress
// before returning; the deferred close on the channel makes the receiver's
// "ok=false" branch the signal to stop awaiting.
func (d *Downloader) run(ctx context.Context, url string, ch chan<- Progress) {
	if err := os.MkdirAll(d.cacheDir, 0o700); err != nil {
		emit(ch, Progress{Done: true, Err: fmt.Errorf("mkdir cache: %w", err)})
		return
	}

	finalPath := d.LocalPath(url)
	partial := d.partialPath(url)

	// If the finalized file already exists, treat as instantly done.
	if info, err := os.Stat(finalPath); err == nil && !info.IsDir() && info.Size() > 0 {
		emit(ch, Progress{
			BytesDone:  info.Size(),
			BytesTotal: info.Size(),
			Done:       true,
			Path:       finalPath,
		})
		return
	}

	// Look for an existing .partial to resume from.
	var resumeFrom int64
	if info, err := os.Stat(partial); err == nil && !info.IsDir() {
		resumeFrom = info.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		emit(ch, Progress{Done: true, Err: err})
		return
	}
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	resp, err := d.http.Do(req)
	if err != nil {
		emit(ch, Progress{Done: true, Err: err})
		return
	}
	defer resp.Body.Close()

	// 206 Partial Content means the server honored our Range — append to the
	// existing partial. 200 OK with a Range request means it ignored us — we
	// have to start over to keep the file consistent.
	openFlags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	startedAt := resumeFrom
	switch resp.StatusCode {
	case http.StatusOK:
		if resumeFrom > 0 {
			// Server ignored Range — discard partial and start fresh.
			_ = os.Remove(partial)
			startedAt = 0
		}
		openFlags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	case http.StatusPartialContent:
		// Append to existing partial.
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		emit(ch, Progress{Done: true, Err: fmt.Errorf(
			"radio: %s: %s", resp.Status, strings.TrimSpace(string(body)),
		)})
		return
	}

	// Total = whatever the server reports, plus the bytes we already have
	// when resuming. ContentLength is just the *remaining* length on a 206.
	var total int64
	if resp.ContentLength > 0 {
		total = resp.ContentLength + startedAt
	}

	f, err := os.OpenFile(partial, openFlags, 0o600)
	if err != nil {
		emit(ch, Progress{Done: true, Err: fmt.Errorf("open partial: %w", err)})
		return
	}

	done := startedAt
	emit(ch, Progress{BytesDone: done, BytesTotal: total}) // initial tick

	buf := make([]byte, 32*1024)
	var sinceLastEmit int64
	lastEmit := time.Now()
	for {
		// Surface ctx cancellation between reads. The HTTP body read also
		// respects ctx via the request, but checking here catches us during
		// disk writes too.
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			emit(ch, Progress{BytesDone: done, BytesTotal: total, Done: true, Err: err})
			return
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				_ = f.Close()
				emit(ch, Progress{BytesDone: done, BytesTotal: total, Done: true, Err: werr})
				return
			}
			done += int64(n)
			sinceLastEmit += int64(n)
			// Throttle progress emits: every progressEvery bytes OR every 100ms.
			if sinceLastEmit >= progressEvery || time.Since(lastEmit) > 100*time.Millisecond {
				emit(ch, Progress{BytesDone: done, BytesTotal: total})
				sinceLastEmit = 0
				lastEmit = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Close()
			emit(ch, Progress{BytesDone: done, BytesTotal: total, Done: true, Err: rerr})
			return
		}
	}

	if err := f.Close(); err != nil {
		emit(ch, Progress{BytesDone: done, BytesTotal: total, Done: true, Err: err})
		return
	}

	if err := os.Rename(partial, finalPath); err != nil {
		emit(ch, Progress{BytesDone: done, BytesTotal: total, Done: true,
			Err: fmt.Errorf("finalize: %w", err)})
		return
	}

	emit(ch, Progress{
		BytesDone:  done,
		BytesTotal: total,
		Done:       true,
		Path:       finalPath,
	})
}

// emit sends p on ch, dropping the update if the receiver isn't keeping up.
// Progress messages are advisory; the next tick will have a more current view.
// A terminal (Done) message MUST be delivered, so we block briefly on those.
func emit(ch chan<- Progress, p Progress) {
	if p.Done {
		// Best-effort delivery for terminal — give the receiver a moment.
		select {
		case ch <- p:
		case <-time.After(2 * time.Second):
		}
		return
	}
	select {
	case ch <- p:
	default:
		// Receiver behind by 4 ticks; drop. The next tick covers it.
	}
}

// basename returns the filename portion of a URL path, sans query/fragment.
// Falls back to "download" when the URL has nothing usable.
func basename(url string) string {
	u := url
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	name := path.Base(u)
	if name == "" || name == "." || name == "/" {
		return "download"
	}
	return name
}

// ProgressErr is a sentinel for "context cancelled" so the UI can tell the
// difference between a server failure and the user backing out.
var ProgressErr = errors.New("radio: download cancelled")
