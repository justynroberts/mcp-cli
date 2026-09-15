package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// StdioTransport runs an MCP server as a child process and exchanges
// newline-delimited JSON-RPC messages over its stdin/stdout.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    chan []byte
	errc   chan error
	stderr *tailBuffer

	closeOnce sync.Once
	label     string
}

// StdioOptions configures a child-process server.
type StdioOptions struct {
	Command string
	Args    []string
	Env     []string // nil inherits the parent environment
	Dir     string
	// LogStderr mirrors the child's stderr to os.Stderr as it arrives.
	LogStderr bool
}

// maxLineBytes caps a single JSON-RPC line. MCP servers can return large
// payloads (file contents, search results), so this is generous.
const maxLineBytes = 32 << 20

// NewStdioTransport starts the child process.
func NewStdioTransport(opts StdioOptions) (*StdioTransport, error) {
	if opts.Command == "" {
		return nil, errors.New("stdio transport: no command configured")
	}
	cmd := exec.Command(opts.Command, opts.Args...)
	cmd.Env = opts.Env
	cmd.Dir = opts.Dir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	t := &StdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		out:    make(chan []byte, 16),
		errc:   make(chan error, 1),
		stderr: &tailBuffer{limit: 8192},
		label:  strings.TrimSpace(opts.Command + " " + strings.Join(opts.Args, " ")),
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", opts.Command, err)
	}

	go t.readLoop(stdout)
	go t.drainStderr(stderr, opts.LogStderr)

	return t, nil
}

func (t *StdioTransport) readLoop(stdout io.Reader) {
	defer close(t.out)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// Servers occasionally print banners on stdout before speaking
		// JSON-RPC; ignore anything that clearly isn't a message.
		if line[0] != '{' && line[0] != '[' {
			continue
		}
		t.out <- []byte(line)
	}
	if err := sc.Err(); err != nil {
		select {
		case t.errc <- err:
		default:
		}
	}
}

func (t *StdioTransport) drainStderr(r io.Reader, mirror bool) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			t.stderr.Write(buf[:n])
			if mirror {
				os.Stderr.Write(buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

// Send writes one message followed by a newline.
func (t *StdioTransport) Send(ctx context.Context, msg []byte) error {
	if _, err := t.stdin.Write(append(msg, '\n')); err != nil {
		return fmt.Errorf("writing to server stdin: %w%s", err, t.stderrHint())
	}
	return nil
}

// Recv returns the next message from the child's stdout.
func (t *StdioTransport) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-t.errc:
		return nil, err
	case msg, ok := <-t.out:
		if !ok {
			return nil, fmt.Errorf("server exited%s: %w", t.stderrHint(), io.EOF)
		}
		return msg, nil
	}
}

// Close closes stdin and waits briefly for the child to exit, then kills it.
func (t *StdioTransport) Close() error {
	var err error
	t.closeOnce.Do(func() {
		_ = t.stdin.Close()
		done := make(chan struct{})
		go func() { _ = t.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-timeAfterShutdown():
			if t.cmd.Process != nil {
				_ = t.cmd.Process.Kill()
			}
			<-done
		}
	})
	return err
}

// Describe returns the command line, for error messages.
func (t *StdioTransport) Describe() string { return "stdio: " + t.label }

// Stderr returns whatever the child wrote to stderr (tail-truncated).
func (t *StdioTransport) Stderr() string { return t.stderr.String() }

func (t *StdioTransport) stderrHint() string {
	s := strings.TrimSpace(t.stderr.String())
	if s == "" {
		return ""
	}
	return " (server stderr: " + s + ")"
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
