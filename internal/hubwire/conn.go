package hubwire

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"
)

// ReadEnvelope reads exactly one envelope from c, blocking until one lands
// or the connection closes / ctx is cancelled. Non-text frames are rejected.
func ReadEnvelope(ctx context.Context, c *websocket.Conn) (Envelope, error) {
	var env Envelope
	_, data, err := c.Read(ctx)
	if err != nil {
		return env, err
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return env, err
	}
	return env, nil
}

// WriteEnvelope marshals env as JSON and sends it as a single text frame.
func WriteEnvelope(ctx context.Context, c *websocket.Conn, env Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, data)
}
