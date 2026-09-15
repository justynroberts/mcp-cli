package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// HTTPTransport speaks MCP over HTTP. It supports both the Streamable HTTP
// transport (one POST per message, replies as JSON or as an SSE stream) and the
// legacy HTTP+SSE transport (a long-lived GET stream plus POSTs to an endpoint
// the server advertises).
type HTTPTransport struct {
	endpoint  string
	headers   map[string]string
	client    *http.Client
	legacySSE bool

	// postURL is where messages are POSTed. For Streamable HTTP it equals
	// endpoint; for legacy SSE the server supplies it in an "endpoint" event.
	postURL   string
	postReady chan struct{}
	readyOnce sync.Once

	incoming chan []byte
	errc     chan error

	mu        sync.Mutex
	sessionID string
	protoVer  string

	cancelStream context.CancelFunc
	wg           sync.WaitGroup
	closeOnce    sync.Once
}

// HTTPOptions configures an HTTP-based server connection.
type HTTPOptions struct {
	URL     string
	Headers map[string]string
	// Legacy selects the 2024-11-05 HTTP+SSE transport.
	Legacy bool
	// Insecure skips TLS certificate verification.
	Insecure bool
	Client   *http.Client
}

// NewHTTPTransport prepares an HTTP transport. For the legacy SSE transport it
// opens the event stream and waits for the endpoint event on first Send.
func NewHTTPTransport(ctx context.Context, opts HTTPOptions) (*HTTPTransport, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("http transport: no url configured")
	}
	if _, err := url.Parse(opts.URL); err != nil {
		return nil, fmt.Errorf("http transport: invalid url %q: %w", opts.URL, err)
	}
	client := opts.Client
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if opts.Insecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		// No client timeout: SSE streams are long-lived; per-call deadlines
		// come from the context instead.
		client = &http.Client{Transport: tr}
	}
	t := &HTTPTransport{
		endpoint:  opts.URL,
		headers:   opts.Headers,
		client:    client,
		legacySSE: opts.Legacy,
		postURL:   opts.URL,
		postReady: make(chan struct{}),
		incoming:  make(chan []byte, 16),
		errc:      make(chan error, 1),
	}
	if !opts.Legacy {
		t.markReady()
		return t, nil
	}
	if err := t.openLegacyStream(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *HTTPTransport) markReady() { t.readyOnce.Do(func() { close(t.postReady) }) }

// openLegacyStream starts the GET event stream used by the 2024-11-05 transport.
func (t *HTTPTransport) openLegacyStream(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.cancelStream = cancel

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, t.endpoint, nil)
	if err != nil {
		cancel()
		return err
	}
	t.applyHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-store")

	resp, err := t.client.Do(req)
	if err != nil {
		cancel()
		return fmt.Errorf("opening SSE stream %s: %w", t.endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		return httpStatusError(t.endpoint, resp.StatusCode, body)
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer resp.Body.Close()
		t.consumeSSE(resp.Body, true)
	}()
	return nil
}

// consumeSSE parses an SSE body, forwarding message events to Recv. When
// handleEndpoint is set, an "endpoint" event updates the POST target.
func (t *HTTPTransport) consumeSSE(body io.Reader, handleEndpoint bool) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var event string
	var data []string
	flush := func() {
		if len(data) == 0 {
			event = ""
			return
		}
		payload := strings.Join(data, "\n")
		data = nil
		switch {
		case handleEndpoint && event == "endpoint":
			t.setPostURL(payload)
		case event == "" || event == "message":
			if strings.HasPrefix(strings.TrimSpace(payload), "{") || strings.HasPrefix(strings.TrimSpace(payload), "[") {
				t.incoming <- []byte(payload)
			}
		}
		event = ""
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		select {
		case t.errc <- err:
		default:
		}
	}
}

func (t *HTTPTransport) setPostURL(raw string) {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil {
		if base, err := url.Parse(t.endpoint); err == nil {
			raw = base.ResolveReference(u).String()
		}
	}
	t.mu.Lock()
	t.postURL = raw
	t.mu.Unlock()
	t.markReady()
}

func (t *HTTPTransport) applyHeaders(req *http.Request) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	sid, ver := t.sessionID, t.protoVer
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if ver != "" {
		req.Header.Set("MCP-Protocol-Version", ver)
	}
	req.Header.Set("User-Agent", ClientName+"/"+ClientVersion)
}

// SetProtocolVersion records the negotiated version, sent on later requests.
func (t *HTTPTransport) SetProtocolVersion(v string) {
	t.mu.Lock()
	t.protoVer = v
	t.mu.Unlock()
}

// SessionID returns the server-assigned session id, if any.
func (t *HTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// Send POSTs one JSON-RPC message and routes whatever comes back to Recv.
func (t *HTTPTransport) Send(ctx context.Context, msg []byte) error {
	select {
	case <-t.postReady:
	case <-ctx.Done():
		return fmt.Errorf("waiting for SSE endpoint event: %w", ctx.Err())
	}
	t.mu.Lock()
	target := t.postURL
	t.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	t.applyHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", target, err)
	}

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		return httpStatusError(target, resp.StatusCode, body)
	}

	ct := resp.Header.Get("Content-Type")
	switch {
	case resp.StatusCode == http.StatusAccepted || resp.ContentLength == 0:
		// Notification acknowledged; nothing to read.
		resp.Body.Close()
		return nil
	case strings.HasPrefix(ct, "text/event-stream"):
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer resp.Body.Close()
			t.consumeSSE(resp.Body, false)
		}()
		return nil
	default:
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("reading response from %s: %w", target, err)
		}
		if len(bytes.TrimSpace(body)) == 0 {
			return nil
		}
		return t.deliverJSON(ctx, body)
	}
}

// deliverJSON queues a JSON body, splitting a JSON-RPC batch into messages.
func (t *HTTPTransport) deliverJSON(ctx context.Context, body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return fmt.Errorf("decoding batch response: %w", err)
		}
		for _, m := range batch {
			select {
			case t.incoming <- m:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	select {
	case t.incoming <- trimmed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recv returns the next message received over HTTP.
func (t *HTTPTransport) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-t.errc:
		return nil, err
	case msg := <-t.incoming:
		return msg, nil
	}
}

// Close tears down the session: it sends the DELETE that ends a Streamable HTTP
// session (when one was established) and stops any in-flight streams.
func (t *HTTPTransport) Close() error {
	t.closeOnce.Do(func() {
		if sid := t.SessionID(); sid != "" && !t.legacySSE {
			req, err := http.NewRequest(http.MethodDelete, t.endpoint, nil)
			if err == nil {
				t.applyHeaders(req)
				if resp, err := t.client.Do(req); err == nil {
					io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
					resp.Body.Close()
				}
			}
		}
		if t.cancelStream != nil {
			t.cancelStream()
		}
		t.client.CloseIdleConnections()
	})
	return nil
}

// Describe returns the endpoint, for error messages.
func (t *HTTPTransport) Describe() string {
	if t.legacySSE {
		return "sse: " + t.endpoint
	}
	return "http: " + t.endpoint
}

func httpStatusError(target string, code int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 512 {
		snippet = snippet[:512] + "…"
	}
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s returned %d %s — check the auth block for this server: %s",
			target, code, http.StatusText(code), snippet)
	case http.StatusNotFound:
		return fmt.Errorf("%s returned 404 — wrong URL, or the MCP session expired: %s", target, snippet)
	}
	if snippet == "" {
		return fmt.Errorf("%s returned %d %s", target, code, http.StatusText(code))
	}
	return fmt.Errorf("%s returned %d %s: %s", target, code, http.StatusText(code), snippet)
}
