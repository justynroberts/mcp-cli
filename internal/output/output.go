// Package output writes the CLI's single JSON document to stdout. Everything
// that is not the result — logs, warnings, errors in human form — goes to
// stderr, so stdout always stays parseable.
package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Envelope is the default stdout document.
type Envelope struct {
	OK        bool   `json:"ok"`
	Command   string `json:"command"`
	Server    string `json:"server,omitempty"`
	Data      any    `json:"data,omitempty"`
	Error     *Error `json:"error,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// Error is the machine-readable failure detail.
type Error struct {
	// Kind is one of: usage, config, connect, timeout, rpc, tool, io.
	Kind    string          `json:"kind"`
	Message string          `json:"message"`
	Code    int             `json:"code,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// KindError tags an error with a machine-readable kind and exit code.
type KindError struct {
	Kind string
	Err  error
	Exit int
}

func (e *KindError) Error() string { return e.Err.Error() }
func (e *KindError) Unwrap() error { return e.Err }

// Wrap tags err with a kind. The exit code is derived from the kind.
func Wrap(kind string, err error) error {
	if err == nil {
		return nil
	}
	return &KindError{Kind: kind, Err: err, Exit: exitFor(kind)}
}

// Errorf is Wrap with formatting.
func Errorf(kind, format string, args ...any) error {
	return Wrap(kind, fmt.Errorf(format, args...))
}

func exitFor(kind string) int {
	switch kind {
	case "usage":
		return 2
	case "config":
		return 3
	case "connect", "timeout":
		return 4
	case "rpc":
		return 5
	case "tool":
		return 6
	default:
		return 1
	}
}

// Writer renders results to stdout.
type Writer struct {
	Out    io.Writer
	Pretty bool
	// Raw prints just the data payload, without the envelope.
	Raw bool
}

// New returns a Writer over stdout.
func New(pretty, raw bool) *Writer {
	return &Writer{Out: os.Stdout, Pretty: pretty, Raw: raw}
}

func (w *Writer) encode(v any) error {
	enc := json.NewEncoder(w.Out)
	enc.SetEscapeHTML(false)
	if w.Pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}

// Success writes a successful result.
func (w *Writer) Success(command, server string, data any, elapsedMS int64) error {
	if w.Raw {
		if data == nil {
			data = map[string]any{}
		}
		return w.encode(data)
	}
	return w.encode(Envelope{OK: true, Command: command, Server: server, Data: data, ElapsedMS: elapsedMS})
}

// Failure writes an error document to stdout and returns the process exit code.
// The message is also mirrored to stderr so interactive use stays readable.
func (w *Writer) Failure(command, server string, err error, elapsedMS int64) int {
	e := &Error{Kind: "error", Message: err.Error()}
	exit := 1

	var ke *KindError
	if errors.As(err, &ke) {
		e.Kind = ke.Kind
		exit = ke.Exit
	}
	var rpcErr interface {
		error
		RPCDetails() (int, json.RawMessage)
	}
	if errors.As(err, &rpcErr) {
		e.Kind = "rpc"
		e.Code, e.Data = rpcErr.RPCDetails()
		if exit == 1 {
			exit = exitFor("rpc")
		}
	}

	fmt.Fprintf(os.Stderr, "mcp-cli: %s\n", err)
	_ = w.encode(Envelope{OK: false, Command: command, Server: server, Error: e, ElapsedMS: elapsedMS})
	return exit
}

// Classify tags err with "timeout" when it is a deadline or cancellation, and
// with fallback otherwise. Use it where a call can fail either way.
func Classify(fallback string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Wrap("timeout", err)
	}
	return Wrap(fallback, err)
}
