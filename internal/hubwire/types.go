// Package hubwire is the JSON-over-WebSocket protocol the local binary and the
// server use to share chat state. A single Envelope struct carries every
// message type; the receiver dispatches on the T field.
//
// Wire is intentionally minimal — there's no schema versioning, no per-channel
// pagination, no batching. The whole channel state fits in a single State
// snapshot at connect time, and deltas afterwards are individual events.
package hubwire

import "github.com/bigboggy/vibespace/internal/ui"

// Message types. Kept as untyped string consts so the wire stays human-grokable
// in a tcpdump. The set is closed — clients dispatch on T with a default that
// logs and ignores unknown types, so adding a new type is forward-compatible.
const (
	// Client → Server.

	// THello is the first message every client sends. The server replies with
	// either TWelcome (auth ok) or TError (rejected).
	THello = "hello"

	// TPost asks the server to append a message to a channel. Server ignores
	// any Author the client sets and fills it from the authenticated identity.
	TPost = "post"

	// TCreate asks the server to create a channel if it doesn't exist.
	TCreate = "create"

	// TView updates this client's presence. Empty Channel clears presence.
	TView = "view"

	// Server → Client.

	// TWelcome confirms auth and tells the client who the server thinks they
	// are. Sent exactly once per connection, before TState.
	TWelcome = "welcome"

	// TState is the initial snapshot of every channel's messages + online
	// counts. Sent right after TWelcome. Subsequent updates come as deltas.
	TState = "state"

	// TMessage is a single new message landing in a channel.
	TMessage = "msg"

	// TCreated is a new channel announcement.
	TCreated = "created"

	// TOnline updates the online count for a single channel.
	TOnline = "online"

	// TError is a non-fatal error sent in response to a client message
	// (validation, rate limit, etc.). For fatal errors the server closes the
	// connection with a WS close frame instead.
	TError = "error"
)

// Envelope is the one struct on the wire. Fields are tagged omitempty so each
// message only carries what it needs — a TOnline message is just
// {"t":"online","channel":"#dev","count":3}.
//
// Senders zero everything they don't need; receivers dispatch on T and read
// only the fields relevant to that type.
type Envelope struct {
	T string `json:"t"`

	// Common identifiers.
	Channel string `json:"channel,omitempty"` // post, create, view, msg, created, online
	Name    string `json:"name,omitempty"`    // create (intended channel name when different from Channel)

	// Post payload.
	Body string      `json:"body,omitempty"` // post
	Kind ui.ChatKind `json:"kind,omitempty"` // post; 0 == ChatNormal == default

	// Hello payload.
	Token string `json:"token,omitempty"` // hello (GH access token)

	// Welcome payload.
	User    string `json:"user,omitempty"`    // welcome (display nick, including @)
	GhLogin string `json:"ghLogin,omitempty"` // welcome (raw GH login, no @)

	// State payload.
	Channels []string                    `json:"channels,omitempty"` // state
	Messages map[string][]ui.ChatMessage `json:"messages,omitempty"` // state
	OnlineBy map[string]int              `json:"onlineBy,omitempty"` // state

	// Message payload.
	Msg *ui.ChatMessage `json:"msg,omitempty"` // msg

	// Online payload.
	Count int `json:"count,omitempty"` // online

	// Error payload.
	Error string `json:"error,omitempty"` // error
}
