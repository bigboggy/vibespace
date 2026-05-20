// ws.go — WebSocket endpoint for local-binary clients.
//
// The local binary connects to /ws, sends a `hello` envelope with a GitHub
// access token, the server verifies the token by calling api.github.com/user,
// then subscribes the connection to the shared LocalHub. Wire messages map
// 1:1 to hub operations (Post, CreateChannel, SetViewing) on the way in;
// hub Events map to message/created/online deltas on the way out.
//
// The HTTP server speaks plain HTTP only — Caddy (or any other reverse proxy)
// is responsible for TLS termination. Run behind `reverse_proxy /ws localhost:8080`
// and the binary connects to wss://vibespace.sh/ws.

package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/bigboggy/vibespace/internal/auth"
	"github.com/bigboggy/vibespace/internal/github"
	"github.com/bigboggy/vibespace/internal/hub"
	"github.com/bigboggy/vibespace/internal/hubwire"
	"github.com/bigboggy/vibespace/internal/identity"
	"github.com/bigboggy/vibespace/internal/ui"
	"github.com/coder/websocket"
)

// wsServer holds the dependencies the /ws handler needs. authSvc is required
// — without GitHub auth there's no way to identify a client, so we don't run
// the WS endpoint at all if authSvc is nil.
type wsServer struct {
	world    *hub.LocalHub
	authSvc  *auth.Service
	identity *identity.Store // for Link on first auth
	device   *github.DeviceFlow

	// tokenCache memoizes "this token resolves to this login" for a short
	// window so we don't hit api.github.com on every connect retry. GitHub
	// access tokens are stable for the life of the OAuth grant; a 10-minute
	// TTL is plenty for reconnect storms and bounds the staleness window if
	// a token is revoked.
	mu         sync.Mutex
	tokenCache map[string]tokenCacheEntry
}

type tokenCacheEntry struct {
	login    string
	verified time.Time
}

const tokenCacheTTL = 10 * time.Minute

// runHTTPServer starts the HTTP server in a goroutine if addr is non-empty.
// Returns the *http.Server so main can Shutdown it gracefully. Returns nil
// (and logs) when addr is empty — the WS endpoint is opt-in via env var.
func runHTTPServer(addr string, world *hub.LocalHub, authSvc *auth.Service, idStore *identity.Store, clientID string) *http.Server {
	if addr == "" {
		log.Printf("websocket disabled (set VIBESPACE_HTTP_ADDR to enable)")
		return nil
	}
	if authSvc == nil {
		log.Printf("websocket disabled (GitHub auth not configured — set VIBESPACE_GH_CLIENT_ID)")
		return nil
	}

	ws := &wsServer{
		world:    world,
		authSvc:  authSvc,
		identity: idStore,
		device: &github.DeviceFlow{
			ClientID: clientID,
			Scope:    "read:user",
			HTTP:     &http.Client{Timeout: 15 * time.Second},
		},
		tokenCache: make(map[string]tokenCacheEntry),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", ws.handle)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("vibespace-server WS listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("ws serve: %v", err)
		}
	}()
	return srv
}

// handle is the /ws HTTP handler. It upgrades to WebSocket, runs the auth
// handshake, and then loops until the client disconnects.
func (ws *wsServer) handle(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// No origin check — clients are headless binaries, not browsers, so
		// the standard CSRF concern doesn't apply. If we ever ship a web
		// client we'll allowlist its origin here.
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	// Pings every 30s; disconnect within ~minute of a dead client.
	c.SetReadLimit(1 << 20) // 1 MiB per envelope is plenty

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// 1. Auth handshake: first message must be hello + token.
	login, err := ws.handshake(ctx, c)
	if err != nil {
		// Best-effort error frame; the close that follows is the real signal.
		_ = hubwire.WriteEnvelope(ctx, c, hubwire.Envelope{T: hubwire.TError, Error: err.Error()})
		_ = c.Close(websocket.StatusPolicyViolation, "auth failed")
		return
	}
	user := "@" + login

	// 2. Subscribe to the hub.
	subID, events := ws.world.Subscribe()
	defer ws.world.Unsubscribe(subID)
	ws.world.SetViewing(subID, "#lobby")

	// 3. Welcome + initial state snapshot.
	if err := hubwire.WriteEnvelope(ctx, c, hubwire.Envelope{T: hubwire.TWelcome, User: user, GhLogin: login}); err != nil {
		return
	}
	if err := hubwire.WriteEnvelope(ctx, c, ws.stateSnapshot()); err != nil {
		return
	}

	// We don't announce "entered the chat" here — the client's app.router
	// triggers lobby.EnsureJoined() on first lobby navigation, which sends
	// the join via TPost. Doing it again here would duplicate the message.

	// 4. Read + write loops. Either side ending tears the connection down via
	// ctx cancellation.
	done := make(chan struct{}, 2)
	go func() { ws.writeLoop(ctx, c, events); done <- struct{}{} }()
	go func() { ws.readLoop(ctx, c, subID, user); done <- struct{}{} }()
	<-done
	cancel()

	_ = c.Close(websocket.StatusNormalClosure, "")
}

// handshake reads the first envelope, expects hello+token, returns the
// resolved GH login or an error.
func (ws *wsServer) handshake(ctx context.Context, c *websocket.Conn) (string, error) {
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	env, err := hubwire.ReadEnvelope(hctx, c)
	if err != nil {
		return "", err
	}
	if env.T != hubwire.THello {
		return "", errors.New("expected hello as first message")
	}
	if env.Token == "" {
		return "", errors.New("missing token")
	}
	return ws.resolveToken(hctx, env.Token)
}

// resolveToken validates a GitHub access token and returns the login. Hits
// api.github.com/user the first time and on cache miss; subsequent calls
// within tokenCacheTTL are served from memory.
func (ws *wsServer) resolveToken(ctx context.Context, token string) (string, error) {
	ws.mu.Lock()
	if e, ok := ws.tokenCache[token]; ok && time.Since(e.verified) < tokenCacheTTL {
		ws.mu.Unlock()
		return e.login, nil
	}
	ws.mu.Unlock()

	login, err := ws.device.UserLogin(ctx, token)
	if err != nil {
		return "", err
	}

	ws.mu.Lock()
	ws.tokenCache[token] = tokenCacheEntry{login: login, verified: time.Now()}
	ws.mu.Unlock()

	return login, nil
}

// readLoop reads client → server envelopes until the connection closes.
func (ws *wsServer) readLoop(ctx context.Context, c *websocket.Conn, subID uint64, user string) {
	for {
		env, err := hubwire.ReadEnvelope(ctx, c)
		if err != nil {
			return
		}
		switch env.T {
		case hubwire.TPost:
			if env.Channel == "" || env.Body == "" {
				continue
			}
			ws.world.Post(env.Channel, user, env.Body, env.Kind)
		case hubwire.TCreate:
			name := env.Name
			if name == "" {
				name = env.Channel
			}
			if name == "" {
				continue
			}
			ws.world.CreateChannel(name)
		case hubwire.TView:
			ws.world.SetViewing(subID, env.Channel)
		default:
			// Unknown ops are ignored — keeps the wire forward-compatible.
		}
	}
}

// writeLoop translates hub.Event into wire messages until ctx is cancelled
// or the events channel closes (Unsubscribe).
func (ws *wsServer) writeLoop(ctx context.Context, c *websocket.Conn, events <-chan hub.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := ws.dispatchEvent(ctx, c, ev); err != nil {
				return
			}
		}
	}
}

// dispatchEvent translates one hub.Event into a wire envelope. The wire shape
// includes the actual delta (message body, online count) so clients don't
// need to round-trip back to the server on every event.
func (ws *wsServer) dispatchEvent(ctx context.Context, c *websocket.Conn, ev hub.Event) error {
	switch ev.Kind {
	case hub.EventMessage:
		msgs, ok := ws.world.Messages(ev.Channel)
		if !ok || len(msgs) == 0 {
			return nil
		}
		last := msgs[len(msgs)-1]
		return hubwire.WriteEnvelope(ctx, c, hubwire.Envelope{
			T: hubwire.TMessage, Channel: ev.Channel, Msg: &last,
		})
	case hub.EventChannelCreated:
		return hubwire.WriteEnvelope(ctx, c, hubwire.Envelope{
			T: hubwire.TCreated, Channel: ev.Channel, Name: ev.Channel,
		})
	case hub.EventPresence:
		return hubwire.WriteEnvelope(ctx, c, hubwire.Envelope{
			T: hubwire.TOnline, Channel: ev.Channel, Count: ws.world.Online(ev.Channel),
		})
	}
	return nil
}

// stateSnapshot builds the initial-state envelope: every channel, every
// message, every online count. Cheap on a freshly-started server; if/when
// the message store grows past in-memory, this is the first thing we paginate.
func (ws *wsServer) stateSnapshot() hubwire.Envelope {
	names := ws.world.ChannelNames()
	messages := make(map[string][]ui.ChatMessage, len(names))
	online := make(map[string]int, len(names))
	for _, n := range names {
		if msgs, ok := ws.world.Messages(n); ok {
			messages[n] = msgs
		}
		online[n] = ws.world.Online(n)
	}
	return hubwire.Envelope{
		T:        hubwire.TState,
		Channels: names,
		Messages: messages,
		OnlineBy: online,
	}
}
