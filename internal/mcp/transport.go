package mcp

import "context"

// Transport moves raw JSON-RPC messages between this client and a server.
// Implementations must be safe for one sender and one receiver goroutine.
type Transport interface {
	// Send delivers one JSON-RPC message to the server.
	Send(ctx context.Context, msg []byte) error
	// Recv returns the next message from the server, blocking until one
	// arrives, ctx is done, or the connection ends (io.EOF).
	Recv(ctx context.Context) ([]byte, error)
	// Close shuts the connection down and releases its resources.
	Close() error
	// Describe returns a short human-readable label for error messages.
	Describe() string
}
