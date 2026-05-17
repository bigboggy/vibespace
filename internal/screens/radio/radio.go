// Package radio renders the radio show screen. There's exactly one show
// queued up at a time (the manifest points at a single MP3 that will loop
// once Phase 3 playback lands). Phase 2: download the file to local cache
// with a resumable, progress-bar'd HTTP fetch. No audio yet.
package radio

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bigboggy/vibespace/internal/radio"
	"github.com/bigboggy/vibespace/internal/screens"
	"github.com/bigboggy/vibespace/internal/theme"
	"github.com/bigboggy/vibespace/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Mode tells the screen whether it's running inside the local binary (where
// downloads + playback work) or inside an SSH session (where the user can
// only see what's on and needs to install the binary to listen).
type Mode int

const (
	ModeLocal Mode = iota
	ModeRemote
)

// Screen is the radio show view.
type Screen struct {
	styles     *theme.Styles
	client     *radio.Client
	downloader *radio.Downloader // nil in remote mode
	player     radio.Player      // never nil; stub builds use a no-op
	mode       Mode

	manifest *radio.Manifest
	loading  bool
	err      error
	loadedAt time.Time

	// Download state. progress is the latest snapshot; downloadCh is the
	// channel the goroutine is writing to; cancelDownload aborts it on
	// explicit cancel. dlReady=true once the file is fully on disk.
	progress       *radio.Progress
	downloadCh     <-chan radio.Progress
	cancelDownload context.CancelFunc
	dlReady        bool // file already on disk (or just finished)
}

// New returns an empty radio screen. dl may be nil — radio.NewDownloader
// is only wired in local mode. player must not be nil (use the stub when
// audio isn't available).
func New(styles *theme.Styles, client *radio.Client, dl *radio.Downloader, player radio.Player, mode Mode) *Screen {
	return &Screen{styles: styles, client: client, downloader: dl, player: player, mode: mode}
}

// ── tea.Cmd plumbing ────────────────────────────────────────────────────────

// manifestLoadedMsg lands when the background manifest fetch returns.
type manifestLoadedMsg struct {
	manifest *radio.Manifest
	err      error
}

// DownloadProgressMsg is exported so the app router can recognize it and
// forward it to the radio screen regardless of which screen is active.
type DownloadProgressMsg struct {
	Progress radio.Progress
}

// DownloadFinishedMsg signals the progress channel closed. The screen uses
// it to clear the download cmd loop.
type DownloadFinishedMsg struct{}

func (s *Screen) loadCmd(force bool) tea.Cmd {
	c := s.client
	return func() tea.Msg {
		var m *radio.Manifest
		var err error
		if force {
			m, err = c.Refresh()
		} else {
			m, err = c.Load()
		}
		return manifestLoadedMsg{manifest: m, err: err}
	}
}

// awaitProgress reads one update from ch and returns the next-step Cmd
// embedded in the returned message via the screen's stored channel.
func awaitProgress(ch <-chan radio.Progress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return DownloadFinishedMsg{}
		}
		return DownloadProgressMsg{Progress: p}
	}
}

// ── Screen interface ────────────────────────────────────────────────────────

// Init is a no-op — the manifest fetch happens via OnEnter so the response
// is routed back to the radio screen (which only becomes active when the
// user opens /radio).
func (s *Screen) Init() tea.Cmd { return nil }

// OnEnter fires the manifest fetch each time the screen is activated, and
// refreshes the "already downloaded?" check.
func (s *Screen) OnEnter() tea.Cmd {
	s.loading = true
	s.refreshDownloadedFlag()
	return s.loadCmd(false)
}

func (s *Screen) refreshDownloadedFlag() {
	if s.downloader == nil || s.manifest == nil || !s.manifest.HasShow() {
		s.dlReady = false
		return
	}
	s.dlReady = s.downloader.IsDownloaded(s.manifest.URL)
}

func (s *Screen) Name() string  { return screens.RadioID }
func (s *Screen) Title() string { return "radio" }

func (s *Screen) HeaderContext() string {
	st := s.styles
	ps := s.player.Status()
	switch {
	case ps.Playing && !ps.Paused:
		return st.NewStyle().Foreground(st.OK).Bold(true).Render("♪ playing")
	case ps.Playing && ps.Paused:
		return st.NewStyle().Foreground(st.Muted).Render("‖ paused")
	case s.loading:
		return st.NewStyle().Foreground(st.Muted).Italic(true).Render("loading…")
	case s.downloading():
		return st.NewStyle().Foreground(st.Accent2).Render("downloading")
	case s.err != nil:
		return st.NewStyle().Foreground(st.Warn).Render("offline")
	case s.manifest == nil || !s.manifest.HasShow():
		return st.NewStyle().Foreground(st.Muted).Render("off air")
	case s.dlReady:
		return st.NewStyle().Foreground(st.OK).Render("● ready")
	default:
		return st.NewStyle().Foreground(st.OK).Render("● on air")
	}
}

func (s *Screen) Footer() []screens.KeyHint {
	ps := s.player.Status()
	hints := []screens.KeyHint{}
	switch {
	case ps.Playing:
		hints = append(hints,
			screens.KeyHint{Key: "space", Desc: "pause"},
			screens.KeyHint{Key: "+/-", Desc: "volume"},
			screens.KeyHint{Key: "s", Desc: "stop"},
		)
	case s.downloading():
		hints = append(hints, screens.KeyHint{Key: "x", Desc: "cancel"})
	case s.dlReady:
		hints = append(hints, screens.KeyHint{Key: "enter", Desc: "play"})
	default:
		hints = append(hints, screens.KeyHint{Key: "enter", Desc: "download"})
	}
	hints = append(hints,
		screens.KeyHint{Key: "r", Desc: "reload"},
		screens.KeyHint{Key: "esc", Desc: "back to lobby"},
	)
	return hints
}

func (s *Screen) InputFocused() bool { return false }

func (s *Screen) downloading() bool {
	return s.progress != nil && !s.progress.Done && s.downloadCh != nil
}

func (s *Screen) Update(msg tea.Msg) (screens.Screen, tea.Cmd) {
	switch m := msg.(type) {
	case manifestLoadedMsg:
		s.loading = false
		s.err = m.err
		if m.manifest != nil {
			s.manifest = m.manifest
		}
		s.loadedAt = time.Now()
		s.refreshDownloadedFlag()
		return s, nil

	case DownloadProgressMsg:
		p := m.Progress
		s.progress = &p
		if p.Done {
			s.cancelDownload = nil
			if p.Err == nil {
				s.dlReady = true
			}
			// The goroutine will close the channel; awaitProgress catches the
			// close and emits DownloadFinishedMsg.
			return s, awaitProgress(s.downloadCh)
		}
		return s, awaitProgress(s.downloadCh)

	case DownloadFinishedMsg:
		s.downloadCh = nil
		return s, nil

	case tea.KeyMsg:
		return s.handleKey(m)
	}
	return s, nil
}

func (s *Screen) handleKey(msg tea.KeyMsg) (screens.Screen, tea.Cmd) {
	ps := s.player.Status()
	switch msg.String() {
	case "r":
		if s.downloading() {
			return s, nil
		}
		s.loading = true
		s.err = nil
		return s, s.loadCmd(true)
	case "x":
		if s.downloading() && s.cancelDownload != nil {
			s.cancelDownload()
		}
		return s, nil
	case " ":
		// Space pauses/resumes when something's loaded.
		if ps.Playing {
			s.player.PauseToggle()
		}
		return s, nil
	case "+", "=":
		if ps.Playing {
			s.player.AdjustVolume(0.1)
		}
		return s, nil
	case "-", "_":
		if ps.Playing {
			s.player.AdjustVolume(-0.1)
		}
		return s, nil
	case "s":
		if ps.Playing {
			s.player.Stop()
		}
		return s, nil
	case "enter":
		return s.handleEnter()
	}
	return s, nil
}

// handleEnter is the action key. Behavior depends on state:
//   - remote (SSH): no-op — the screen shows the install one-liner instead
//   - manifest missing / errored / off-air: no-op
//   - already playing: no-op (use space/s to control)
//   - already downloaded: hand the file to the player
//   - mid-download: no-op
//   - else: start the download
func (s *Screen) handleEnter() (screens.Screen, tea.Cmd) {
	if s.mode == ModeRemote || s.downloader == nil {
		return s, nil
	}
	if s.manifest == nil || !s.manifest.HasShow() {
		return s, nil
	}
	if s.downloading() {
		return s, nil
	}
	if s.player.Status().Playing {
		return s, nil
	}
	if s.dlReady {
		// File is on disk — hand it to the player. Loop playback is the
		// player's default; we just give it the path.
		path := s.downloader.LocalPath(s.manifest.URL)
		_ = s.player.Play(path)
		return s, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelDownload = cancel
	s.progress = &radio.Progress{}
	s.downloadCh = s.downloader.Start(ctx, s.manifest.URL)
	return s, awaitProgress(s.downloadCh)
}

// ── Rendering ───────────────────────────────────────────────────────────────

func (s *Screen) View(width, height int) string {
	st := s.styles

	if s.loading && (s.manifest == nil || !s.manifest.HasShow()) {
		return s.center(width, height, st.NewStyle().
			Foreground(st.Muted).Italic(true).Render("tuning in…"))
	}
	if s.err != nil && (s.manifest == nil || !s.manifest.HasShow()) {
		return s.center(width, height,
			st.NewStyle().Foreground(st.Warn).Render("can't reach the radio: "+s.err.Error())+
				"\n\n"+
				st.NewStyle().Foreground(st.Muted).Render("press r to retry"))
	}
	if s.manifest == nil || !s.manifest.HasShow() {
		return s.center(width, height, st.NewStyle().
			Foreground(st.Muted).Render("nothing on air right now — check back soon"))
	}

	return s.renderShow(width, height)
}

func (s *Screen) center(width, height int, body string) string {
	return s.styles.Place(width, height, lipgloss.Center, lipgloss.Center, body)
}

// renderShow is the single happy-path layout: title, metadata, action area.
func (s *Screen) renderShow(width, height int) string {
	st := s.styles
	m := s.manifest

	title := st.NewStyle().Foreground(st.Accent).Bold(true).Render(m.Title())
	filename := st.NewStyle().Foreground(st.Muted).Italic(true).Render(m.Filename())

	var metaParts []string
	if !m.Updated.IsZero() {
		metaParts = append(metaParts, "updated "+ui.HumanizeTime(m.Updated))
	}
	if !s.loadedAt.IsZero() {
		metaParts = append(metaParts, "synced "+ui.HumanizeTime(s.loadedAt))
	}
	meta := ""
	if len(metaParts) > 0 {
		meta = st.NewStyle().Foreground(st.Muted).Render(strings.Join(metaParts, " · "))
	}

	action := s.renderAction()

	body := strings.Join([]string{title, filename, "", meta, "", action}, "\n")
	wrap := st.NewStyle().Padding(1, 2).Render(body)
	return st.Place(width, height, lipgloss.Center, lipgloss.Center, wrap)
}

// renderAction shows the action area. Differs based on mode + download +
// playback state.
func (s *Screen) renderAction() string {
	st := s.styles

	if s.mode == ModeRemote {
		hint := st.NewStyle().Foreground(st.Muted).
			Render("audio plays in the local binary — install:")
		cmd := st.NewStyle().Foreground(st.OK).Bold(true).
			Render("curl -fsSL https://raw.githubusercontent.com/bigboggy/vibespace/main/scripts/install.sh | bash")
		return hint + "\n" + cmd
	}

	ps := s.player.Status()

	if !ps.Available {
		// Stub player (no-audio build). Tell the user what's wrong and how
		// to get an audio-capable binary.
		return st.NewStyle().Foreground(st.Warn).
			Render("this build has no audio output") +
			"\n" +
			st.NewStyle().Foreground(st.Muted).
				Render("install the official release for a binary with audio")
	}

	if ps.Playing {
		return s.renderNowPlaying(ps)
	}

	if s.downloading() {
		return s.renderProgress()
	}

	if s.progress != nil && s.progress.Done && s.progress.Err != nil {
		return st.NewStyle().Foreground(st.Warn).
			Render("download failed: "+s.progress.Err.Error()) +
			"\n" +
			st.NewStyle().Foreground(st.Muted).Render("press enter to retry")
	}

	if ps.Err != nil {
		return st.NewStyle().Foreground(st.Warn).
			Render("player error: "+ps.Err.Error()) +
			"\n" +
			st.NewStyle().Foreground(st.Muted).Render("press enter to retry")
	}

	if s.dlReady {
		return st.NewStyle().Foreground(st.OK).Bold(true).
			Render("ready to play") +
			"\n" +
			st.NewStyle().Foreground(st.Muted).Render("press enter to tune in (loops forever)")
	}

	return st.NewStyle().Foreground(st.Muted).Render(
		"press enter to download   " +
			st.NewStyle().Foreground(st.BorderLo).
				Render("(file caches locally; one-time fetch)"),
	)
}

// renderNowPlaying renders the live playback status: pause indicator and
// volume bar.
func (s *Screen) renderNowPlaying(ps radio.PlayerStatus) string {
	st := s.styles

	label := "♪ now playing"
	if ps.Paused {
		label = "‖ paused"
	}
	top := st.NewStyle().Foreground(st.OK).Bold(true).Render(label)

	// Volume slider: 10 cells, filled count = level * 10.
	const slots = 10
	filled := int(ps.Volume*float64(slots) + 0.5)
	if filled > slots {
		filled = slots
	}
	bar := st.NewStyle().Foreground(st.Accent).Render(strings.Repeat("█", filled)) +
		st.NewStyle().Foreground(st.BorderLo).Render(strings.Repeat("░", slots-filled))
	vol := st.NewStyle().Foreground(st.Muted).Render("vol ") + bar +
		st.NewStyle().Foreground(st.Muted).Render(fmt.Sprintf("  %d%%", int(ps.Volume*100)))

	hint := st.NewStyle().Foreground(st.BorderLo).
		Render("space = pause   +/- = volume   s = stop")

	return top + "\n" + vol + "\n" + hint
}

// renderProgress is the percent + bar + bytes line shown during download.
func (s *Screen) renderProgress() string {
	st := s.styles
	p := s.progress
	pct := 0.0
	if p.BytesTotal > 0 {
		pct = float64(p.BytesDone) / float64(p.BytesTotal)
		if pct > 1 {
			pct = 1
		}
	}

	const barWidth = 32
	filled := int(float64(barWidth) * pct)
	if filled > barWidth {
		filled = barWidth
	}
	bar := st.NewStyle().Foreground(st.Accent2).Render(strings.Repeat("█", filled)) +
		st.NewStyle().Foreground(st.BorderLo).Render(strings.Repeat("░", barWidth-filled))

	var sizeLine string
	if p.BytesTotal > 0 {
		sizeLine = fmt.Sprintf("%s / %s", radio.FormatSize(p.BytesDone), radio.FormatSize(p.BytesTotal))
	} else {
		sizeLine = radio.FormatSize(p.BytesDone)
	}
	pctText := fmt.Sprintf("%3.0f%%", pct*100)

	line := st.NewStyle().Foreground(st.Accent2).Bold(true).Render("downloading  ") +
		bar + "  " +
		st.NewStyle().Foreground(st.Fg).Render(pctText) + "  " +
		st.NewStyle().Foreground(st.Muted).Render(sizeLine)
	return line
}
