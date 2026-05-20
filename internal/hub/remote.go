package hub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bigboggy/vibespace/internal/hubwire"
	"github.com/bigboggy/vibespace/internal/ui"
	"github.com/coder/websocket"
)

// RemoteHub is a network-backed Hub the local binary uses to share chat state
// with the server (and every other local-binary user). It holds a local mirror
// of channels/messages/presence that's kept in sync by a background read
// goroutine, and forwards write operations (Post, CreateChannel, SetViewing)
// as JSON envelopes over the WebSocket.
//
// One *RemoteHub per process. The lobby calls Subscribe() the same way it
// would with a LocalHub; the RemoteHub fans wire messages out to local
// subscribers so the bubbletea model sees a hub.Event whenever the server
// reports a change.
type RemoteHub struct {
	conn *websocket.Conn

	// Identity reported by the server in the welcome envelope.
	user    string
	ghLogin string

	// Local state mirror — populated from the initial State snapshot and
	// kept in sync by the read goroutine.
	mu       sync.RWMutex
	order    []string
	channels map[string][]ui.ChatMessage
	online   map[string]int

	// Local subscriber bookkeeping (mirrors LocalHub). The lobby subscribes
	// here, not against the server — we fan server events into these.
	subMu   sync.Mutex
	subs    map[uint64]*sub
	nextSub atomic.Uint64

	// readDone is closed when the read goroutine exits (connection died /
	// ctx cancelled). Callers can select on Done() to detect disconnect.
	readDone chan struct{}

	// readErr captures the reason the read loop exited, for diagnostics.
	readErr atomic.Pointer[error]
}

// Dial connects to a vibespace WS endpoint, performs the auth handshake,
// receives the initial state snapshot, and returns a ready-to-use RemoteHub.
//
// url is the full WS URL ("wss://vibespace.sh/ws" or "ws://localhost:8080/ws").
// token is a GitHub access token obtained via the device flow.
//
// On success the hub's background read goroutine is already running. Use
// Subscribe() to receive events; call Close() when done.
func Dial(ctx context.Context, url, token string) (*RemoteHub, error) {
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	c.SetReadLimit(1 << 20)

	// Hello.
	if err := hubwire.WriteEnvelope(ctx, c, hubwire.Envelope{
		T: hubwire.THello, Token: token,
	}); err != nil {
		_ = c.Close(websocket.StatusInternalError, "hello failed")
		return nil, fmt.Errorf("send hello: %w", err)
	}

	// Welcome.
	welcome, err := hubwire.ReadEnvelope(ctx, c)
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "welcome failed")
		return nil, fmt.Errorf("read welcome: %w", err)
	}
	if welcome.T == hubwire.TError {
		_ = c.Close(websocket.StatusPolicyViolation, "auth rejected")
		return nil, fmt.Errorf("server rejected auth: %s", welcome.Error)
	}
	if welcome.T != hubwire.TWelcome {
		_ = c.Close(websocket.StatusProtocolError, "expected welcome")
		return nil, fmt.Errorf("expected welcome, got %q", welcome.T)
	}

	// Initial state snapshot.
	state, err := hubwire.ReadEnvelope(ctx, c)
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "state failed")
		return nil, fmt.Errorf("read state: %w", err)
	}
	if state.T != hubwire.TState {
		_ = c.Close(websocket.StatusProtocolError, "expected state")
		return nil, fmt.Errorf("expected state, got %q", state.T)
	}

	rh := &RemoteHub{
		conn:     c,
		user:     welcome.User,
		ghLogin:  welcome.GhLogin,
		order:    state.Channels,
		channels: state.Messages,
		online:   state.OnlineBy,
		subs:     make(map[uint64]*sub),
		readDone: make(chan struct{}),
	}
	if rh.channels == nil {
		rh.channels = make(map[string][]ui.ChatMessage)
	}
	if rh.online == nil {
		rh.online = make(map[string]int)
	}

	go rh.readLoop()
	return rh, nil
}

// User returns the @-prefixed display nick the server confirmed for this
// session (e.g. "@bogdan"). Set during Dial; never mutates.
func (r *RemoteHub) User() string { return r.user }

// GhLogin returns the bare GitHub login the server confirmed (no @).
func (r *RemoteHub) GhLogin() string { return r.ghLogin }

// Done returns a channel that's closed when the read goroutine exits — the
// connection's been lost or Close was called. Callers (e.g. the bubbletea
// program) can watch this to render a disconnected state.
func (r *RemoteHub) Done() <-chan struct{} { return r.readDone }

// Err reports why the read loop exited. Returns nil if it's still running or
// if Close was called cleanly.
func (r *RemoteHub) Err() error {
	if e := r.readErr.Load(); e != nil {
		return *e
	}
	return nil
}

// Close tears down the WS connection. Safe to call more than once.
func (r *RemoteHub) Close() error {
	return r.conn.Close(websocket.StatusNormalClosure, "")
}

// readLoop consumes server → client envelopes, applies them to the mirror,
// and broadcasts events to local subscribers. Exits on read error.
func (r *RemoteHub) readLoop() {
	defer close(r.readDone)
	defer func() {
		// On exit, close every subscriber channel so the lobby's
		// waitForEvent unblocks and the bubbletea program can react.
		r.subMu.Lock()
		for _, s := range r.subs {
			if s.closed.CompareAndSwap(false, true) {
				close(s.events)
			}
		}
		r.subMu.Unlock()
	}()

	ctx := context.Background()
	for {
		env, err := hubwire.ReadEnvelope(ctx, r.conn)
		if err != nil {
			e := err
			r.readErr.Store(&e)
			return
		}
		switch env.T {
		case hubwire.TMessage:
			r.applyMessage(env.Channel, env.Msg)
			r.broadcast(Event{Kind: EventMessage, Channel: env.Channel})
		case hubwire.TCreated:
			name := env.Name
			if name == "" {
				name = env.Channel
			}
			r.applyCreated(name)
			r.broadcast(Event{Kind: EventChannelCreated, Channel: name})
		case hubwire.TOnline:
			r.applyOnline(env.Channel, env.Count)
			r.broadcast(Event{Kind: EventPresence, Channel: env.Channel})
		case hubwire.TError:
			// Non-fatal server errors land here. Swallow for now — the wire
			// is intentionally chatty about validation failures and there's
			// nowhere useful to surface these without a bigger UX rework.
		}
	}
}

func (r *RemoteHub) applyMessage(channel string, msg *ui.ChatMessage) {
	if channel == "" || msg == nil {
		return
	}
	r.mu.Lock()
	r.channels[channel] = append(r.channels[channel], *msg)
	r.mu.Unlock()
}

func (r *RemoteHub) applyCreated(name string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	if _, exists := r.channels[name]; !exists {
		r.channels[name] = nil
		r.order = append(r.order, name)
	}
	r.mu.Unlock()
}

func (r *RemoteHub) applyOnline(channel string, count int) {
	if channel == "" {
		return
	}
	r.mu.Lock()
	r.online[channel] = count
	r.mu.Unlock()
}

// broadcast fans an event out to every local subscriber. Mirrors LocalHub.
func (r *RemoteHub) broadcast(ev Event) {
	r.subMu.Lock()
	subs := make([]*sub, 0, len(r.subs))
	for _, s := range r.subs {
		subs = append(subs, s)
	}
	r.subMu.Unlock()

	for _, s := range subs {
		if s.closed.Load() {
			continue
		}
		select {
		case s.events <- ev:
		default:
		}
	}
}

// send marshals env and writes it to the connection with a short context
// timeout. Failures are logged via readErr; the next readLoop pass will
// detect the broken connection and tear everything down.
func (r *RemoteHub) send(env hubwire.Envelope) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hubwire.WriteEnvelope(ctx, r.conn, env); err != nil {
		e := err
		r.readErr.Store(&e)
		_ = r.conn.Close(websocket.StatusInternalError, "write failed")
	}
}

// ── Hub interface ──────────────────────────────────────────────────────────

func (r *RemoteHub) ChannelNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

func (r *RemoteHub) Messages(name string) ([]ui.ChatMessage, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	msgs, ok := r.channels[name]
	if !ok {
		return nil, false
	}
	out := make([]ui.ChatMessage, len(msgs))
	copy(out, msgs)
	return out, true
}

func (r *RemoteHub) HasChannel(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.channels[name]
	return ok
}

func (r *RemoteHub) Online(name string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.online[name]
}

// Post sends a message via the WS. The author argument is ignored — the
// server fills it from the authenticated identity. The message lands in the
// local mirror only when the server echoes it back as TMessage; until then
// it isn't visible. (UI-side optimistic rendering can be added later.)
func (r *RemoteHub) Post(channel, _author, body string, kind ui.ChatKind) {
	if channel == "" || body == "" {
		return
	}
	r.send(hubwire.Envelope{
		T:       hubwire.TPost,
		Channel: channel,
		Body:    body,
		Kind:    kind,
	})
}

// CreateChannel asks the server to create the channel. The TCreated echo
// adds it to the mirror; clients don't apply the create locally first.
func (r *RemoteHub) CreateChannel(name string) bool {
	if !strings.HasPrefix(name, "#") {
		name = "#" + name
	}
	r.send(hubwire.Envelope{T: hubwire.TCreate, Channel: name, Name: name})
	// Return value is best-effort — true means "we asked"; the real result
	// arrives later as a TCreated event. The lobby uses CreateChannel
	// alongside HasChannel so it doesn't depend heavily on this.
	return true
}

// Subscribe registers a local subscriber. Mirrors LocalHub.Subscribe; the
// returned id is meaningful only to this RemoteHub and is *not* the server's
// subscriber id. SetViewing translates to a TView wire message that the
// server uses to track presence — the server has its own id for this
// connection.
func (r *RemoteHub) Subscribe() (uint64, <-chan Event) {
	id := r.nextSub.Add(1)
	s := &sub{events: make(chan Event, 16)}
	r.subMu.Lock()
	r.subs[id] = s
	r.subMu.Unlock()
	return id, s.events
}

func (r *RemoteHub) Unsubscribe(id uint64) {
	r.subMu.Lock()
	s, ok := r.subs[id]
	if ok {
		delete(r.subs, id)
	}
	r.subMu.Unlock()
	if ok && s.closed.CompareAndSwap(false, true) {
		close(s.events)
	}
}

// SetViewing tells the server which channel this client is viewing. The
// server uses it to compute online counts; the local mirror doesn't track
// our own viewing state explicitly (the lobby already knows what it's
// rendering).
func (r *RemoteHub) SetViewing(_id uint64, channel string) {
	r.send(hubwire.Envelope{T: hubwire.TView, Channel: channel})
}

// Compile-time check.
var _ Hub = (*RemoteHub)(nil)

// ensure errors package is referenced even when builds end up tree-shaking
// helpers. (Used by error-wrapping in Dial; keeps the import unconditional.)
var _ = errors.New
