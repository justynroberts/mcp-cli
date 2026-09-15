package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/justynroberts/mcp-cli/internal/config"
)

// signalContext cancels the returned context on SIGINT/SIGTERM so child
// processes and HTTP sessions get torn down cleanly.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

func asNoConfig(err error, target **config.ErrNoConfig) bool { return errors.As(err, target) }
