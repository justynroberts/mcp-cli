# mcp-cli

Query MCP servers from the shell and get JSON back.

`mcp-cli` is a single static Go binary. Point it at a config file that says which
MCP servers exist and how to authenticate to them, then look things up from the
command line — GitHub issues, Confluence pages, anything an MCP server exposes.
The result is one JSON document on stdout, ready for `jq` or a script.

```console
$ mcp-cli call github search_issues query="is:open label:bug" perPage:=5 -raw | jq '.items[].title'
```

- **No LLM needed** — this is a protocol client, not an agent. No model, no API
  key, no tokens; same input, same output. See [No LLM required](#no-llm-required).
- **Portable** — pure Go, `CGO_ENABLED=0`, no runtime dependencies. macOS, Linux
  and Windows on amd64/arm64 all build from `make release`.
- **Both transports** — stdio child processes, Streamable HTTP, and the legacy
  HTTP+SSE transport older hosted servers still speak.
- **stdout is only ever JSON** — logs, warnings and errors go to stderr, so a
  pipe never gets polluted.
- **No dependencies beyond `gopkg.in/yaml.v3`** — the MCP client is implemented
  here, so there is nothing to keep in sync with an SDK.

## No LLM required

MCP is usually drawn as *model ↔ tools*, but the protocol underneath is just
JSON-RPC with self-describing schemas. `mcp-cli` is a plain client: no model, no
API key, no inference, no tokens spent, and the same input gives the same output
every time.

That makes an MCP server usable as an ordinary API from cron jobs, CI steps,
Makefiles, monitoring checks and shell functions. The appeal is leverage: one
config file and one interface across GitHub, Confluence, Jira and anything else
with a server, instead of a bespoke client per API. The tool names in this
README's Confluence walkthrough were read out of the real server with
`mcp-cli tools confluence` — no model involved in that either.

Three things the model *was* doing for you, which now fall to you:

- **Choosing the tool and its arguments.** A model does that per request; you do
  it once with `tools`, `schema` and `discover`, then hard-code it. Ideal for a
  known task, no help for an open-ended one.
- **Tolerating output written for a model.** Descriptions are prose, results are
  often JSON stuffed inside a text block, and response shapes are not versioned
  the way a REST API would be. That is why `-raw` parses the text into an object
  for you, and why the Confluence example says to confirm field names with
  `jq keys` — a server can reshape its output between releases.
- **Answering the server back.** A few servers drive the client: sampling (the
  server asks you to run an inference) and elicitation (interactive prompting).
  `mcp-cli` declines both with `-32601`, which is correct for lookups but means
  a tool built around them will not run.

Worth knowing on speed: an MCP server is slower than the API it wraps. A stdio
server costs roughly a second of process spawn per invocation; an already-running
HTTP server answers a `ping` in about 9ms. For a hot path, call the vendor API
directly — `mcp-cli` earns its place when the alternative is maintaining several
bespoke API clients.


## Install

```bash
make build           # ./mcp-cli
make install         # into $GOBIN
make release         # dist/ for darwin, linux, windows × amd64/arm64
```

Requires Go 1.24+ to build. The resulting binary requires nothing.

## Quick start

```bash
mcp-cli init                        # writes ~/.config/mcp-cli/config.yaml
export GITHUB_TOKEN=ghp_...         # the config refers to it, never stores it
mcp-cli servers                     # what's configured
mcp-cli tools github                # what that server can do
mcp-cli schema github search_issues # exactly what arguments it takes
mcp-cli call github search_issues query="repo:foo/bar is:open" -p
```

No config needed for a one-off:

```bash
mcp-cli -url https://api.githubcopilot.com/mcp/ -token '${GITHUB_TOKEN}' tools
mcp-cli -cmd "npx -y @modelcontextprotocol/server-filesystem /tmp" tools
```

## Examples

Every command below is real, and the output is captured from an actual run
against two reference servers that need no credentials. `examples/tour.sh`
runs the whole sequence — use it to check an install:

```bash
./examples/tour.sh
```

**Ask a server what it can do.** No config file needed; `-cmd` defines the
server inline.

```console
$ mcp-cli -cmd 'npx -y @modelcontextprotocol/server-filesystem /tmp/demo' tools -raw | jq '.count'
14

$ mcp-cli -cmd 'npx -y @modelcontextprotocol/server-filesystem /tmp/demo' tools search -raw | jq -r '.tools[].name'
search_files
```

**Ask what a tool takes**, rather than guessing:

```console
$ mcp-cli -cmd '...server-filesystem /tmp/demo' schema read_text_file -raw | jq -c '.input_schema.properties | keys'
["head","path","tail"]
```

**Call it.** `head=2` is a string on the command line and a number in the
request, because the tool's schema says `integer`:

```console
$ mcp-cli -cmd '...server-filesystem /tmp/demo' call read_text_file path=/tmp/demo/notes.txt head=2 -raw
{"content":"alpha\nbravo"}
```

**A tool that returns JSON hands you an object**, ready for jq:

```console
$ cat /tmp/demo/service.json
{"service":"servicename","tier":1,"oncall":["ada","grace"]}

$ mcp-cli -cmd '...server-filesystem /tmp/demo' call read_text_file path=/tmp/demo/service.json -raw | jq -r '.oncall[]'
ada
grace
```

**Resources and prompts, not just tools:**

```console
$ mcp-cli -cmd 'npx -y @modelcontextprotocol/server-everything stdio' resources -raw | jq -c '{count, first: .resources[0].uri}'
{"count":7,"first":"demo://resource/static/document/architecture.md"}

$ mcp-cli -cmd '...server-everything stdio' prompts -raw | jq -r '.prompts[].name'
simple-prompt
args-prompt
completable-prompt
resource-prompt

$ mcp-cli -cmd '...server-everything stdio' prompt args-prompt city=London state=UK -raw | jq -r '.messages[0].content.text'
What's weather in London, UK?
```

**Mistakes are caught before the call is sent**, with the server's own schema
as the source of truth:

```console
$ mcp-cli -cmd '...server-everything stdio' call get-structured-content
mcp-cli: tool "get-structured-content" requires: location (see `mcp-cli -cmd "..." schema get-structured-content`)
$ echo $?
2

$ mcp-cli -cmd '...server-everything stdio' call get-structured-content location=London
mcp-cli: argument "location": "London" is not one of "New York", "Chicago", "Los Angeles"

$ mcp-cli -cmd '...server-everything stdio' call get-structured-content location='New York' -raw | jq -c .
{"conditions":"Cloudy","humidity":82,"temperature":33}
```

**Exit codes let you branch without parsing:**

```console
$ mcp-cli -cmd '...server-everything stdio' ping >/dev/null 2>&1 && echo reachable || echo unreachable
reachable

$ mcp-cli -url http://localhost:9 ping >/dev/null 2>&1; echo "exit=$?"
exit=4
```

**The same server over HTTP** — only the flags change:

```console
$ mcp-cli -url http://localhost:3101/mcp -raw call get-sum a=40 b=2
"The sum of 40 and 2 is 42."

$ mcp-cli -url http://localhost:3101/mcp -raw info | jq -c '{server: .server.name, protocol: .protocol_version}'
{"server":"mcp-servers/everything","protocol":"2025-06-18"}
```

**Query every configured server at once.** `discover` connects in parallel and
reports per-server, so one dead server does not sink the command:

```console
$ mcp-cli discover read -raw | jq -c '[.servers[] | {server, tools: [.tools[].name]}]'
[{"server":"everything","tools":[]},{"server":"files","tools":["read_file","read_text_file","read_media_file","read_multiple_files","create_directory","directory_tree","get_file_info"]}]
```

Flags may go anywhere on the line — `-raw` at the end reads naturally and works.
Use `--` if a tool argument itself starts with a dash.


## Walkthrough: pulling a runbook out of Confluence

The task: *"get me the runbook for `servicename`."* Confluence is reached
through [mcp-atlassian](https://github.com/sooperset/mcp-atlassian), which runs
as a stdio server in Docker.

**1. Configure it once.** In `~/.config/mcp-cli/config.yaml`:

```yaml
servers:
  confluence:
    transport: stdio
    command: docker
    args: [run, -i, --rm, -e, CONFLUENCE_URL, -e, CONFLUENCE_USERNAME,
           -e, CONFLUENCE_API_TOKEN, ghcr.io/sooperset/mcp-atlassian:latest]
    env:
      CONFLUENCE_URL: ${CONFLUENCE_URL}
      CONFLUENCE_USERNAME: ${CONFLUENCE_USERNAME}
      CONFLUENCE_API_TOKEN: ${CONFLUENCE_API_TOKEN}
```

```bash
export CONFLUENCE_URL=https://your-org.atlassian.net/wiki
export CONFLUENCE_USERNAME=you@your-org.com
export CONFLUENCE_API_TOKEN=...   # id.atlassian.com/manage-profile/security/api-tokens
```

**2. Find the right tool.** The server decides what it offers, so ask it rather
than guessing:

```console
$ mcp-cli tools confluence search
{"ok":true,"command":"tools","server":"confluence","data":{"count":2,"tools":[
  {"name":"confluence_search","description":"Search Confluence content using simple terms or CQL.",
   "arguments":["limit: integer","query: string (required)","spaces_filter: string"]},
  {"name":"confluence_search_user","description":"Search Confluence users.", "...":"..."}]},
 "elapsed_ms":1840}
```

**3. Search for the page.** `query` takes plain text or CQL; CQL is worth it
here because runbooks are usually labelled:

```bash
mcp-cli -p call confluence confluence_search \
    query='label = "runbook" AND title ~ "servicename"' limit:=5
```

**4. Take the page id and fetch the body as markdown.** `convert_to_markdown`
is a boolean in the tool's schema, so `:=true` sends a real boolean:

```bash
PAGE=$(mcp-cli -raw call confluence confluence_search \
         query='label = "runbook" AND title ~ "servicename"' limit:=1 \
       | jq -r '.results[0].id')

mcp-cli -raw call confluence confluence_get_page \
    page_id="$PAGE" convert_to_markdown:=true include_metadata:=true \
  | jq -r '.content.value // .content // .'
```

The field names inside the result (`.results[].id`, `.content.value`) are the
*server's*, not mcp-cli's. Confirm them once for your instance with
`… | jq 'keys'`; the fallback chain above copes with either shape.

**5. Wrap it in a shell function.** Now "the runbook for X" is one word:

```bash
runbook() {
  local page
  page=$(mcp-cli -raw call confluence confluence_search \
           query="label = \"runbook\" AND title ~ \"$1\"" limit:=1 \
         | jq -r '.results[0].id // empty')
  [ -n "$page" ] || { echo "no runbook found for $1" >&2; return 1; }
  mcp-cli -raw call confluence confluence_get_page \
      page_id="$page" convert_to_markdown:=true \
    | jq -r '.content.value // .content // .'
}

$ runbook servicename | head -20        # or: runbook servicename | glow -
```

Because failures are exit codes rather than prose on stdout, this composes:
`runbook servicename || page-oncall`.

Pulling the same runbook from a *hosted* Atlassian server instead needs no
config file at all:

```bash
mcp-cli -url https://mcp.atlassian.com/v1/sse -transport sse \
        -token "$ATLASSIAN_OAUTH_TOKEN" \
        -raw call confluence_search query='title ~ "servicename" AND label = "runbook"'
```

> The tool names and argument types above were read from mcp-atlassian with
> `mcp-cli tools confluence`. The response *shapes* come from your Confluence
> instance — check them with `jq keys` the first time.


## Configuration

Looked up in order: `-c/-config`, `$MCP_CLI_CONFIG`, `./mcp-cli.yaml`,
`~/.config/mcp-cli/config.yaml`. YAML or JSON (YAML is a superset, so a `.json`
file loads unchanged).

```yaml
version: 1

defaults:
  timeout: 60s
  server: github          # used when a command omits the server name

servers:
  github:                              # hosted server, Streamable HTTP
    transport: http
    url: https://api.githubcopilot.com/mcp/
    auth:
      type: bearer
      token: ${GITHUB_TOKEN}

  confluence:                          # local server, stdio
    transport: stdio
    command: docker
    args: [run, -i, --rm, -e, CONFLUENCE_URL, -e, CONFLUENCE_USERNAME,
           -e, CONFLUENCE_API_TOKEN, ghcr.io/sooperset/mcp-atlassian:latest]
    env:
      CONFLUENCE_URL: ${CONFLUENCE_URL}
      CONFLUENCE_USERNAME: ${CONFLUENCE_USERNAME}
      CONFLUENCE_API_TOKEN: ${CONFLUENCE_API_TOKEN}

  atlassian-remote:                    # hosted server, legacy SSE transport
    transport: sse
    url: https://mcp.atlassian.com/v1/sse
    auth:
      type: bearer
      token: ${ATLASSIAN_OAUTH_TOKEN}
```

`transport` is inferred when omitted: a `url` means `http`, a `command` means
`stdio`.

### Credentials

A stdio server is a child process that **inherits this process's environment**
unless `env:` is set, which is what lets `npx` and `docker` find `PATH` and
`HOME`. Anything exported in your shell is therefore visible to the server you
launch — worth remembering before running an MCP server you do not trust.

Secrets belong in the environment, not in the file. Every string value expands
`${VAR}` and `${VAR:-default}` at run time.

| Auth type | Effect |
|---|---|
| `bearer` | `Authorization: Bearer <token>` (override with `prefix`) |
| `basic`  | `Authorization: Basic base64(username:password)` |
| `header` | any header you name, e.g. `header: X-Api-Key` |
| `none`   | no auth header (the default) |

The token itself can come from `token: ${VAR}`, `token_file: /path`, or
`token_command: "op read op://vault/item/token"`. For stdio servers,
credentials usually travel through `env:` instead — a key with an empty value
passes the parent process's variable straight through.

An auth block that resolves to an empty token is an error, not a silent
unauthenticated request.

## Commands

| Command | What it does |
|---|---|
| `servers` | list configured servers |
| `info <server>` | server identity, protocol version, capabilities |
| `ping <server>` | reachability check |
| `tools <server> [substring]` | list tools, optionally filtered |
| `schema <server> <tool>` | one tool's full JSON schema |
| `call <server> <tool> [key=value ...]` | call a tool, print JSON |
| `resources <server> [substring]` | list resources and templates |
| `read <server> <uri>` | read one resource |
| `prompts <server>` / `prompt <server> <name> [key=value ...]` | list / render prompts |
| `discover [substring]` | find matching tools across every server, in parallel |
| `init [path]` | write a starter config |
| `version` | CLI and protocol versions |

Global flags: `-c/-config`, `-p/-pretty`, `-raw`, `-v/-verbose`, `-full`,
`-timeout`, `-url`, `-cmd`, `-token`, `-header`, `-transport`, `-insecure`,
`-protocol`. They may appear before or after the subcommand.

### Arguments

Tool arguments are `key=value` pairs. Values are coerced to whatever the tool's
input schema declares, so `limit=5` arrives as the number `5` and `open=true` as
a boolean. Required arguments and `enum` values are checked locally, before a
call is sent — a typo comes back as `"London" is not one of "New York",
"Chicago", "Los Angeles"` rather than as a server validation error. Prompt
arguments are checked the same way.

| Form | Meaning |
|---|---|
| `key=value` | string, coerced to the schema's type |
| `key:=json` | literal JSON — `limit:=5`, `tags:=["a","b"]`, `f:={"x":1}` |
| `key=@path` | value read from a file (`-` reads stdin) |
| `key:=@path` | JSON parsed from a file |
| `key=a,b,c` | a comma list becomes an array when the schema says `array` |

Tool and prompt names may be given as a unique prefix or substring:
`call github search_iss` works as long as it is unambiguous.

## Output

Success:

```json
{
  "ok": true,
  "command": "call",
  "server": "github",
  "data": {
    "tool": "search_issues",
    "is_error": false,
    "json": { "total_count": 2, "items": [] },
    "text": "{\"total_count\":2,\"items\":[]}"
  },
  "elapsed_ms": 412
}
```

Most MCP servers answer with a text block that itself contains JSON. That text
is parsed and surfaced as `data.json`; the untouched text stays in `data.text`,
and `-full` additionally keeps every raw content block. A server that returns
`structuredContent` gets it surfaced as `data.structured`.

Blocks that text cannot represent — images, audio, resource links, binary
resources — are always kept under `data.content`, so a result is never silently
truncated to its text.

Every list is always a JSON array — an empty one is `[]`, never `null` or an
absent key — so `jq '.tools[]'` is safe against any server.

`-raw` prints just the useful payload — `json`, else `structured`, else the
text — which is normally what you want in a pipeline:

```bash
mcp-cli -raw call confluence confluence_search query='space = ENG' limit:=10 | jq '.[].title'
```

Failure, on stdout, with the human-readable message also on stderr:

```json
{"ok":false,"command":"call","server":"github","error":{"kind":"rpc","message":"rpc error -32602: unknown tool","code":-32602},"elapsed_ms":88}
```

### Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | unclassified error |
| 2 | usage error (bad flag, unknown tool, missing required argument) |
| 3 | config error (no config file, unknown or disabled server) |
| 4 | connection error (process failed to start, HTTP/auth failure, timeout) |
| 5 | JSON-RPC error from the server |
| 6 | the tool ran and reported failure (`isError`) |

## Development

```bash
make test        # everything
make test-race   # with the race detector
make lint        # gofmt + go vet
go test ./internal/cli -run TestToolsAndCallEndToEnd -v
```

Tests run against `internal/mockserver`, a real MCP server implementation used
over both stdio (the test binary re-execs itself) and HTTP (`httptest`). No
network access and no external server are needed.

## Protocol support

Advertises MCP `2025-06-18` and negotiates down to `2025-03-26` or `2024-11-05`
if a server rejects it. Implements the client side of `initialize`, `ping`,
`tools/*`, `resources/*`, `prompts/*`, list pagination, and request
cancellation. Server-initiated requests (sampling, roots, elicitation) are
answered with `-32601` rather than ignored, so servers never block on them.

OAuth flows are out of scope: bring a token the server accepts.
