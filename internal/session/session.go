// Package session turns a configured server into a live, initialized MCP client.
package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/justynroberts/mcp-cli/internal/config"
	"github.com/justynroberts/mcp-cli/internal/mcp"
)

// DefaultTimeout applies when neither the server nor the CLI sets one.
const DefaultTimeout = 60 * time.Second

// Options tune how the session is established.
type Options struct {
	// Timeout overrides the server's configured timeout when non-zero.
	Timeout time.Duration
	// LogStderr mirrors a stdio server's stderr to this process's stderr.
	LogStderr bool
	// Log receives protocol diagnostics.
	Log mcp.Logf
	// ProtocolVersion pins the handshake version instead of negotiating down.
	ProtocolVersion string
}

// Session is a connected server plus the metadata from its handshake.
type Session struct {
	Client   *mcp.Client
	Server   *config.Server
	Init     *mcp.InitializeResult
	Timeout  time.Duration
	closeFns []func()
}

// Close releases the session.
func (s *Session) Close() {
	if s.Client != nil {
		_ = s.Client.Close()
	}
	for _, f := range s.closeFns {
		f()
	}
}

// Context returns a context carrying this session's per-request deadline.
func (s *Session) Context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.Timeout)
}

// Open connects to srv and completes the MCP handshake. If the server rejects
// the advertised protocol version, it retries with older revisions.
func Open(ctx context.Context, srv *config.Server, opts Options) (*Session, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = srv.Timeout.D()
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	versions := []string{mcp.ProtocolVersion}
	if opts.ProtocolVersion != "" {
		versions = []string{opts.ProtocolVersion}
	} else {
		versions = append(versions, mcp.FallbackProtocolVersions...)
	}

	var lastErr error
	for _, version := range versions {
		t, err := dial(ctx, srv, opts)
		if err != nil {
			return nil, err
		}
		client := mcp.NewClient(t, opts.Log)

		initCtx, cancel := context.WithTimeout(ctx, timeout)
		init, err := client.Initialize(initCtx, version)
		cancel()
		if err == nil {
			return &Session{Client: client, Server: srv, Init: init, Timeout: timeout}, nil
		}
		_ = client.Close()
		lastErr = err
		if !isVersionError(err) {
			break
		}
		if opts.Log != nil {
			opts.Log("server rejected protocol version %s, retrying older revision", version)
		}
	}
	return nil, fmt.Errorf("connecting to %q (%s): %w", srv.Name, describe(srv), lastErr)
}

func dial(ctx context.Context, srv *config.Server, opts Options) (mcp.Transport, error) {
	switch srv.Transport {
	case config.TransportStdio:
		return mcp.NewStdioTransport(mcp.StdioOptions{
			Command:   config.ExpandEnv(srv.Command),
			Args:      srv.ResolvedArgs(),
			Env:       srv.ResolvedEnv(),
			Dir:       config.ExpandEnv(srv.Dir),
			LogStderr: opts.LogStderr,
		})
	case config.TransportHTTP, config.TransportSSE:
		headers, err := srv.ResolvedHeaders()
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", srv.Name, err)
		}
		return mcp.NewHTTPTransport(ctx, mcp.HTTPOptions{
			URL:      config.ExpandEnv(srv.URL),
			Headers:  headers,
			Legacy:   srv.Transport == config.TransportSSE,
			Insecure: srv.Insecure,
		})
	default:
		return nil, fmt.Errorf("server %q: unsupported transport %q", srv.Name, srv.Transport)
	}
}

func describe(srv *config.Server) string {
	if srv.URL != "" {
		return string(srv.Transport) + " " + srv.URL
	}
	return string(srv.Transport) + " " + srv.Command
}

// isVersionError reports whether an initialize failure looks like protocol
// version negotiation rather than a real fault.
func isVersionError(err error) bool {
	var rpcErr *mcp.RPCError
	if errors.As(err, &rpcErr) {
		msg := strings.ToLower(rpcErr.Message + " " + string(rpcErr.Data))
		return strings.Contains(msg, "protocol version") || strings.Contains(msg, "protocolversion")
	}
	return false
}
