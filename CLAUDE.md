# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`mcp-cli` — a single static Go binary that queries MCP servers from the shell
and prints JSON to stdout. Config file says which servers exist and how to
authenticate; CLI arguments say what to look up. Built for GitHub- and
Confluence-style servers, but nothing in it is server-specific.

Two hard rules the whole design serves:

1. **stdout is only ever the JSON result.** Logs, protocol diagnostics, child
   stderr and error text go to stderr. Never `fmt.Println` a result.
2. **Portability.** Pure Go, `CGO_ENABLED=0`, one third-party dependency
   (`gopkg.in/yaml.v3`). The MCP client is implemented in-tree rather than
   pulled from an SDK — don't add an SDK dependency to "simplify" it.

## Commands

```bash
make build                  # ./mcp-cli
make test                   # go test ./...
make test-race              # race detector; the transports are concurrent
make lint                   # gofmt -l + go vet
make release                # dist/: darwin, linux, windows × amd64/arm64, plus linux/arm
make                        # no target = `make help`
go test ./internal/cli -run TestToolsAndCallEndToEnd -v   # one test
```

Smoke test against a real server (no credentials needed):

```bash
make build                                      # ALWAYS rebuild first: `go build ./...`
                                                # does not refresh ./mcp-cli
./mcp-cli -cmd "npx -y @modelcontextprotocol/server-filesystem /tmp" tools -p
./examples/tour.sh                              # the full documented tour
```

## Architecture

Dependencies point one way: `cli → session → {config, mcp} → output`.

| Package | Responsibility |
|---|---|
| `main.go` | `os.Exit(cli.Run(os.Args[1:]))`, nothing else |
| `internal/config` | parse the YAML/JSON config, `${VAR}` expansion, resolve auth into HTTP headers or child env |
| `internal/mcp` | the MCP client: JSON-RPC framing, the three transports, typed protocol calls |
| `internal/session` | `config.Server` + options → a connected, initialized client |
| `internal/cli` | flags, subcommand dispatch, argument coercion, result flattening |
| `internal/output` | the stdout envelope and the error-kind → exit-code mapping |
| `internal/mockserver` | a real MCP server implementation used only by tests |

### The parts that need reading together

**Transports are interchangeable behind `mcp.Transport`** (`Send`/`Recv`/
`Close`). `StdioTransport` runs a child process and scans newline-delimited
JSON; `HTTPTransport` covers *both* HTTP modes — Streamable HTTP (one POST per
message, reply either `application/json` or an SSE stream) and the legacy
2024-11-05 transport (`Legacy: true`: a long-lived GET whose `endpoint` event
names the POST URL, with every reply arriving on that GET stream). Both feed a
single `incoming` channel, so `Client` is transport-agnostic.

**`Client` (internal/mcp/client.go) owns the dispatch loop.** One goroutine
reads messages forever and routes them by JSON-RPC id to the channel the
matching `Call` is waiting on. Three message kinds are distinguished in
`readLoop`, and all three must stay handled: responses, peer notifications
(logged), and *peer requests* (sampling/roots/elicitation), which are answered
with `-32601` — dropping them makes servers hang.

**Protocol version negotiation lives in `session.Open`, not in `Client`.** A
rejected version needs a *fresh* transport (a stdio child usually exits on a
failed handshake), so `Open` loops over `ProtocolVersion` + fallbacks and
re-dials each time. `isVersionError` decides whether a failure is worth
retrying. The ladder is `2025-06-18` → `2025-03-26` → `2024-11-05`.

**Tool arguments are coerced against the server's own schema.** `call` fetches
the tool via `findTool` first, then `buildArguments` uses its `inputSchema` to
turn `limit=5` into a number and `tags=a,b` into an array (`internal/cli/
args.go`). `key:=json` bypasses coercion entirely. This costs one extra
round trip per call and is deliberate: it is what makes shell-shaped input
usable, and it lets required arguments be caught before a call is sent.

**Local validation happens before the call.** `call` and `prompt` both resolve
the name (exact → unique prefix/substring), then check required arguments and
`enum` membership from the server's own schema. This is deliberate: it turns a
round trip and an opaque server validation error into an immediate message
listing the allowed values. `invocationHint` renders the "run this instead"
suggestion — always use it rather than interpolating `c.server`, which is the
placeholder `"inline"` under `-url`/`-cmd`.

**`buildToolOutput` is the "return a JSON object" promise** (`internal/cli/
result.go`). Servers overwhelmingly return JSON *inside* a text content block;
that text is parsed into `data.json`, with `data.text` kept alongside. Bare
scalars and text with trailing prose are deliberately *not* treated as JSON —
see `parseJSON`. `-raw` prints `json` → `structured` → `text` → blocks, in that
order of preference. `hasUncapturedContent` decides what must survive:
images, audio, resource links and binary resources are kept as raw blocks
because flattening cannot express them. Dropping them was a real bug — the
regression test is `TestNonTextContentSurvivesFlattening`.

**Errors carry a kind, and the kind is the exit code.** Wrap failures with
`output.Wrap("usage"|"config"|"connect"|"rpc"|"tool"|"io", err)`; the mapping to
exit codes 2–6 lives in `output.exitFor`. `"io"` has no mapping and falls
through to exit 1 like an unwrapped error; only `init` uses it. Use
`output.Classify(kind, err)` wherever a deadline can fire: it re-tags timeouts
and cancellations as `"timeout"` (exit 4) instead of reporting them as RPC
faults. An unwrapped error exits 1 — that is a code smell, not a default.
`*mcp.RPCError` is detected via its `RPCDetails` method so the JSON-RPC code
reaches the output envelope.

**Flags are accepted anywhere on the line**, including after positional
arguments (`call github search_issues query=bug -raw`). Go's `flag` package
stops at the first positional, which silently handed `-raw` to the tool as an
argument — the built-in help advertised a form that did not work.
`splitArgs` (`internal/cli/flags.go`) pre-sorts tokens into flags and
positionals before either FlagSet parses, honouring `-flag=value`, value-taking
vs boolean flags, and a `--` terminator. Both the root and subcommand parses go
through it; don't call `fs.Parse` on raw argv.

**Every list is an array, never null.** The `List*` methods in
`internal/mcp/client.go` start from an empty slice, and `discover` seeds each
server's `tools`, so `jq '.tools[]'` never breaks on a server with nothing to
report. A Go `var xs []T` that reaches the encoder marshals as `null` — that is
the bug this prevents.

### Adding a subcommand

Append a `*command` var in `internal/cli/commands.go` and add it to the slice in
`init()` (`cli.go`). `run` returns the payload to emit — the envelope, timing
and exit code are handled for you. Call `c.open(ctx)` to get a session; it
consumes the leading positional argument as the server name unless `-url`/`-cmd`
defined the server inline, which is why commands read their own arguments from
`c.args` *after* calling `open`.

`mcp-cli init` writes `starterConfig` (`internal/cli/starter.go`), a
commented example config. If you change the config schema, update it and the
README's Configuration section in the same change.

The version string is injected at build time via `-ldflags -X
.../internal/cli.Version`; a plain `go build` reports the default.

## Testing

`internal/mockserver` is a working MCP server, not a stub — the same `Handle`
function backs `ServeStdio`, the Streamable HTTP `Handler` (with options for SSE
replies, session ids, auth headers and pagination) and `LegacyHandler`. stdio
tests re-exec the test binary with `MCP_CLI_TEST_STDIO_MOCK=1`, which `TestMain`
turns into a server; that is why both `internal/mcp` and `internal/cli` have a
`TestMain`. End-to-end CLI tests swap `os.Stdout` for a pipe and assert on the
parsed envelope and the exit code, so the output contract above is enforced by
tests — expect them to fail if you change the envelope shape.

No test touches the network or needs credentials. Keep it that way.

`examples/tour.sh` is the manual counterpart: it runs the documented commands
against the real `@modelcontextprotocol` reference servers over stdio and HTTP.
Run it after changing output shapes or flag handling — it caught both the
flag-placement bug and the null-list bug. The README's Examples section is
captured from its output, so update them together.

## Things that will bite

- `gofmt -l` is part of `make lint` and CI-shaped checks; run it after editing.
- The `discover` command connects to every configured server concurrently; a
  failing server must degrade to a per-server `error` field, never fail the
  whole command.
- `config.Duration` accepts `"30s"` and a bare number of seconds — the YAML tag
  is checked, because yaml.v3 will happily decode `5` into a string.
- `prompts/get` must always send an `arguments` object, even an empty one;
  schema-validating servers reject an absent one.
- Prefer returning tool failures as data (`isError` → exit 6) over Go errors;
  only transport and protocol faults are Go errors.
