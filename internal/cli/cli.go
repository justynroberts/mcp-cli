// Package cli implements mcp-cli's command line: flag parsing, server
// resolution, and dispatch to the individual commands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/justynroberts/mcp-cli/internal/config"
	"github.com/justynroberts/mcp-cli/internal/mcp"
	"github.com/justynroberts/mcp-cli/internal/output"
	"github.com/justynroberts/mcp-cli/internal/session"
)

// Version is the CLI version, overridable at build time with
// -ldflags "-X github.com/justynroberts/mcp-cli/internal/cli.Version=...".
var Version = "0.1.0"

// globals are the flags accepted by every command.
type globals struct {
	configPath string
	timeout    time.Duration
	pretty     bool
	raw        bool
	verbose    bool
	full       bool
	protocol   string
	version    bool

	// Ad-hoc server definition, used instead of the config file.
	url       string
	cmdline   string
	token     string
	headers   stringSlice
	transport string
	insecure  bool
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.configPath, "config", g.configPath, "path to the config file (default: $MCP_CLI_CONFIG, ./mcp-cli.yaml, ~/.config/mcp-cli/config.yaml)")
	fs.StringVar(&g.configPath, "c", g.configPath, "shorthand for -config")
	fs.DurationVar(&g.timeout, "timeout", g.timeout, "per-request timeout, e.g. 30s (default: the server's, else 60s)")
	fs.BoolVar(&g.pretty, "pretty", g.pretty, "indent the JSON output")
	fs.BoolVar(&g.pretty, "p", g.pretty, "shorthand for -pretty")
	fs.BoolVar(&g.raw, "raw", g.raw, "print just the result payload, without the {ok,command,data} envelope")
	fs.BoolVar(&g.verbose, "verbose", g.verbose, "log protocol traffic and server stderr to stderr")
	fs.BoolVar(&g.verbose, "v", g.verbose, "shorthand for -verbose")
	fs.BoolVar(&g.full, "full", g.full, "include raw content blocks and full schemas in the output")
	fs.StringVar(&g.protocol, "protocol", g.protocol, "pin the MCP protocol version instead of negotiating")
	fs.BoolVar(&g.version, "version", g.version, "print the CLI and protocol versions, then exit")

	fs.StringVar(&g.url, "url", g.url, "connect to this MCP endpoint instead of a configured server")
	fs.StringVar(&g.cmdline, "cmd", g.cmdline, "launch this command as a stdio MCP server instead of a configured server")
	fs.StringVar(&g.token, "token", g.token, "bearer token for -url (supports ${ENV_VAR})")
	fs.Var(&g.headers, "header", "extra HTTP header as Name:Value (repeatable)")
	fs.StringVar(&g.transport, "transport", g.transport, "transport for -url: http (default) or sse")
	fs.BoolVar(&g.insecure, "insecure", g.insecure, "skip TLS certificate verification")
}

// inline reports whether the server is defined by flags rather than config.
func (g *globals) inline() bool { return g.url != "" || g.cmdline != "" }

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// command is one subcommand.
type command struct {
	name    string
	aliases []string
	usage   string
	summary string
	// run returns the JSON payload to emit, or an error.
	run func(ctx context.Context, c *runContext) (any, error)
}

// runContext carries everything a command needs.
type runContext struct {
	g    *globals
	args []string // positional args after the subcommand
	out  *output.Writer
	// server is the resolved server name, for the output envelope.
	server string
}

var commands []*command

func init() {
	commands = []*command{
		cmdServers, cmdInfo, cmdPing, cmdTools, cmdSchema,
		cmdCall, cmdResources, cmdRead, cmdPrompts, cmdPrompt,
		cmdDiscover, cmdInit, cmdVersion,
	}
}

func lookup(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
		for _, a := range c.aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

// Run executes the CLI and returns a process exit code.
func Run(argv []string) int {
	start := time.Now()
	g := &globals{}

	root := flag.NewFlagSet("mcp-cli", flag.ContinueOnError)
	root.SetOutput(io.Discard)
	g.register(root)
	rootFlags, rootPositional, err := splitArgs(root, argv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if len(rootPositional) > 0 {
				if c := lookup(rootPositional[0]); c != nil {
					fmt.Fprintf(os.Stdout, "Usage: mcp-cli %s\n\n%s\n", c.usage, c.summary)
					return 0
				}
			}
			usage(os.Stdout)
			return 0
		}
		fmt.Fprintf(os.Stderr, "mcp-cli: %v\n\n", err)
		usage(os.Stderr)
		return 2
	}
	if err := root.Parse(rootFlags); err != nil {
		if err == flag.ErrHelp {
			usage(os.Stdout)
			return 0
		}
		fmt.Fprintf(os.Stderr, "mcp-cli: %v\n\n", err)
		usage(os.Stderr)
		return 2
	}

	rest := rootPositional
	if g.version && len(rest) == 0 {
		rest = []string{"version"}
	}
	if len(rest) == 0 {
		usage(os.Stdout)
		return 0
	}

	name := rest[0]
	if name == "help" || name == "-h" || name == "--help" {
		if len(rest) > 1 {
			if c := lookup(rest[1]); c != nil {
				fmt.Fprintf(os.Stdout, "Usage: mcp-cli %s\n\n%s\n", c.usage, c.summary)
				return 0
			}
		}
		usage(os.Stdout)
		return 0
	}

	cmd := lookup(name)
	if cmd == nil {
		fmt.Fprintf(os.Stderr, "mcp-cli: unknown command %q\n\n", name)
		usage(os.Stderr)
		return 2
	}

	// Re-parse so global flags may also appear after the subcommand.
	sub := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	sub.SetOutput(io.Discard)
	g.register(sub)
	subFlags, subPositional, err := splitArgs(sub, rest[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stdout, "Usage: mcp-cli %s\n\n%s\n", cmd.usage, cmd.summary)
			return 0
		}
		fmt.Fprintf(os.Stderr, "mcp-cli: %v\nUsage: mcp-cli %s\n", err, cmd.usage)
		return 2
	}
	if err := sub.Parse(subFlags); err != nil {
		if err == flag.ErrHelp {
			fmt.Fprintf(os.Stdout, "Usage: mcp-cli %s\n\n%s\n", cmd.usage, cmd.summary)
			return 0
		}
		fmt.Fprintf(os.Stderr, "mcp-cli: %v\nUsage: mcp-cli %s\n", err, cmd.usage)
		return 2
	}

	w := output.New(g.pretty, g.raw)
	rc := &runContext{g: g, args: subPositional, out: w}

	ctx, stop := signalContext(context.Background())
	defer stop()

	data, err := cmd.run(ctx, rc)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return w.Failure(cmd.name, rc.server, err, elapsed)
	}
	if err := w.Success(cmd.name, rc.server, data, elapsed); err != nil {
		fmt.Fprintf(os.Stderr, "mcp-cli: writing output: %v\n", err)
		return 1
	}
	return 0
}

// logf returns the diagnostic logger for the current verbosity.
func (c *runContext) logf() mcp.Logf {
	if !c.g.verbose {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "mcp-cli: "+format+"\n", args...)
	}
}

// loadConfig reads the config file, tolerating its absence in inline mode.
func (c *runContext) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(c.g.configPath)
	if err != nil {
		var missing *config.ErrNoConfig
		if ok := asNoConfig(err, &missing); ok {
			if c.g.inline() {
				return &config.Config{Servers: map[string]*config.Server{}}, nil
			}
			return nil, output.Errorf("config", "%v\nRun `mcp-cli init` to write a starter config, or pass -url/-cmd to connect directly.", err)
		}
		return nil, output.Wrap("config", err)
	}
	return cfg, nil
}

// takeServer resolves the server for this invocation, consuming the leading
// positional argument unless the server was defined inline with flags.
func (c *runContext) takeServer() (*config.Server, error) {
	if c.g.inline() {
		srv, err := c.inlineServer()
		if err != nil {
			return nil, err
		}
		c.server = srv.Name
		return srv, nil
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, err
	}
	name := ""
	if len(c.args) > 0 {
		name = c.args[0]
		c.args = c.args[1:]
	}
	srv, err := cfg.Get(name)
	if err != nil {
		return nil, output.Wrap("config", err)
	}
	c.server = srv.Name
	return srv, nil
}

// inlineServer builds a server definition from -url / -cmd and friends.
func (c *runContext) inlineServer() (*config.Server, error) {
	g := c.g
	srv := &config.Server{Name: "inline", Insecure: g.insecure}
	switch {
	case g.url != "":
		srv.Transport = config.TransportHTTP
		if g.transport == string(config.TransportSSE) {
			srv.Transport = config.TransportSSE
		}
		srv.URL = g.url
		srv.Headers = map[string]string{}
		for _, h := range g.headers {
			k, v, ok := strings.Cut(h, ":")
			if !ok {
				return nil, output.Errorf("usage", "-header %q must be Name:Value", h)
			}
			srv.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		if g.token != "" {
			srv.Auth = &config.Auth{Type: "bearer", Token: g.token}
		}
	case g.cmdline != "":
		fields := strings.Fields(g.cmdline)
		srv.Transport = config.TransportStdio
		srv.Command = fields[0]
		srv.Args = fields[1:]
	}
	if err := srv.Validate(); err != nil {
		return nil, output.Wrap("usage", err)
	}
	return srv, nil
}

// open connects to the server resolved from flags or config.
func (c *runContext) open(ctx context.Context) (*session.Session, error) {
	srv, err := c.takeServer()
	if err != nil {
		return nil, err
	}
	return c.openServer(ctx, srv)
}

func (c *runContext) openServer(ctx context.Context, srv *config.Server) (*session.Session, error) {
	s, err := session.Open(ctx, srv, session.Options{
		Timeout:         c.g.timeout,
		LogStderr:       c.g.verbose,
		Log:             c.logf(),
		ProtocolVersion: c.g.protocol,
	})
	if err != nil {
		return nil, output.Classify("connect", err)
	}
	return s, nil
}

// invocationHint renders a copy-pasteable command for error messages, putting
// the server where *this* invocation expects it: inline servers are addressed
// by -url/-cmd, not by the placeholder name "inline".
func (c *runContext) invocationHint(sub string, rest ...string) string {
	parts := []string{"mcp-cli"}
	switch {
	case c.g.url != "":
		parts = append(parts, "-url", c.g.url, sub)
	case c.g.cmdline != "":
		parts = append(parts, "-cmd", strconv.Quote(c.g.cmdline), sub)
	default:
		parts = append(parts, sub, c.server)
	}
	return strings.Join(append(parts, rest...), " ")
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mcp-cli — query MCP servers from the shell and get JSON back.

Usage:
  mcp-cli [flags] <command> [arguments]

Commands:
`)
	for _, c := range commands {
		if len(c.usage) > 28 {
			fmt.Fprintf(w, "  %s\n  %-28s %s\n", c.usage, "", c.summary)
			continue
		}
		fmt.Fprintf(w, "  %-28s %s\n", c.usage, c.summary)
	}
	fmt.Fprint(w, `
Flags:
  -c, -config PATH     config file (default: $MCP_CLI_CONFIG, ./mcp-cli.yaml,
                       ~/.config/mcp-cli/config.yaml)
  -p, -pretty          indent the JSON output
      -raw             print just the payload, without the result envelope
  -v, -verbose         log protocol traffic and server stderr to stderr
      -full            include raw content blocks and full tool schemas
      -timeout 30s     per-request timeout
      -url URL         connect to an endpoint directly, ignoring the config
      -cmd "CMD ARGS"  launch a stdio server directly, ignoring the config
      -token TOKEN     bearer token for -url (supports ${ENV_VAR})
      -header N:V      extra HTTP header (repeatable)
      -transport http|sse   transport for -url (default http)
      -insecure        skip TLS verification
      -protocol VER    pin the MCP protocol version
  -h, -help            show this help (or "mcp-cli help <command>")
      -version         print the CLI and protocol versions

Examples:
  mcp-cli servers
  mcp-cli tools github
  mcp-cli call github search_repositories query=mcp perPage:=5 -p
  mcp-cli call confluence confluence_search query='label = "runbook" AND title ~ "servicename"'
  mcp-cli -url https://api.githubcopilot.com/mcp/ -token '${GITHUB_TOKEN}' tools

Output goes to stdout as a single JSON document; logs and errors go to stderr.
Exit codes: 0 ok, 2 usage, 3 config, 4 connect, 5 rpc error, 6 tool error.
`)
}
