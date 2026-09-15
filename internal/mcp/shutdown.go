package mcp

import "time"

// shutdownGrace is how long Close waits for a child process to exit on its own
// after its stdin is closed.
var shutdownGrace = 2 * time.Second

func timeAfterShutdown() <-chan time.Time { return time.After(shutdownGrace) }
