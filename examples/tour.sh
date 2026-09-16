#!/usr/bin/env bash
# A tour of mcp-cli against two public MCP servers that need no credentials.
#
#   ./examples/tour.sh            # uses ./mcp-cli, or mcp-cli from $PATH
#
# Everything here is a real command you can copy. The only prerequisites are
# npx (to fetch the reference servers) and jq.

set -u

MCP=${MCP:-$( [ -x ./mcp-cli ] && echo ./mcp-cli || echo mcp-cli )}
DEMO=$(mktemp -d)
trap 'rm -rf "$DEMO"' EXIT
FILES="npx -y @modelcontextprotocol/server-filesystem $DEMO"
EVERYTHING="npx -y @modelcontextprotocol/server-everything stdio"
PORT=${PORT:-3101}

step() { printf '\n\033[1m$ %s\033[0m\n' "$*"; }
run()  { step "$@"; eval "$@"; }

command -v jq >/dev/null || { echo "this tour needs jq" >&2; exit 1; }

echo "### 1. What am I running?"
run "$MCP version -raw"

echo
echo "### 2. What can a server do? (no config file — -cmd defines it inline)"
run "$MCP -cmd '$FILES' tools -raw | jq '.count'"
run "$MCP -cmd '$FILES' tools search -raw | jq -r '.tools[].name'"

echo
echo "### 3. What arguments does a tool take?"
run "$MCP -cmd '$FILES' schema read_text_file -raw | jq -c '.input_schema.properties | keys'"

echo
echo "### 4. Call it. Strings are coerced to the schema's types (head=2 -> number)"
printf 'alpha\nbravo\ncharlie\n' > "$DEMO/notes.txt"
run "$MCP -cmd '$FILES' call read_text_file path=$DEMO/notes.txt head=2 -raw"

echo
echo "### 5. A tool that returns JSON gives you a JSON object, ready for jq"
printf '{\"service\":\"servicename\",\"tier\":1,\"oncall\":[\"ada\",\"grace\"]}\n' > "$DEMO/service.json"
run "$MCP -cmd '$FILES' call read_text_file path=$DEMO/service.json -raw | jq -r '.oncall[]'"

echo
echo "### 6. Resources and prompts, not just tools"
run "$MCP -cmd '$EVERYTHING' resources -raw | jq -c '{count, first: .resources[0].uri}'"
run "$MCP -cmd '$EVERYTHING' read demo://resource/static/document/features.md -raw | head -c 120"
echo
run "$MCP -cmd '$EVERYTHING' prompts -raw | jq -r '.prompts[].name'"
run "$MCP -cmd '$EVERYTHING' prompt args-prompt city=London state=UK -raw | jq -r '.messages[0].content.text'"

echo
echo "### 7. Mistakes are caught locally, before a call is sent"
run "$MCP -cmd '$EVERYTHING' call get-structured-content 2>&1 | grep '^mcp-cli:'; echo \"exit=\${PIPESTATUS[0]}\""
run "$MCP -cmd '$EVERYTHING' call get-structured-content location=London 2>&1 | grep '^mcp-cli:'"
run "$MCP -cmd '$EVERYTHING' call get-structured-content location='New York' -raw | jq -c ."

echo
echo "### 8. Exit codes let you branch without parsing"
step "$MCP -cmd '$EVERYTHING' ping >/dev/null 2>&1 && echo reachable || echo unreachable"
$MCP -cmd "$EVERYTHING" ping >/dev/null 2>&1 && echo reachable || echo unreachable
step "$MCP -url http://localhost:9 ping >/dev/null 2>&1; echo \"exit=\$? (4 = could not connect)\""
$MCP -url http://localhost:9 ping >/dev/null 2>&1; echo "exit=$? (4 = could not connect)"

echo
echo "### 9. The same server over HTTP instead of stdio"
(PORT=$PORT npx -y @modelcontextprotocol/server-everything streamableHttp >/tmp/mcp-cli-tour-http.log 2>&1 &)
for _ in $(seq 1 40); do
  grep -q 'listening on port' /tmp/mcp-cli-tour-http.log 2>/dev/null && break
  sleep 1
done
run "$MCP -url http://localhost:$PORT/mcp -raw call get-sum a=40 b=2"
run "$MCP -url http://localhost:$PORT/mcp -raw info | jq -c '{server: .server.name, protocol: .protocol_version}'"
pkill -f 'server-everything streamableHttp' 2>/dev/null

echo
echo "### 10. Query every configured server at once"
cat > /tmp/mcp-cli-tour.yaml <<YAML
version: 1
servers:
  files:
    command: npx
    args: [-y, "@modelcontextprotocol/server-filesystem", "$DEMO"]
  everything:
    command: npx
    args: [-y, "@modelcontextprotocol/server-everything", stdio]
YAML
run "$MCP -c /tmp/mcp-cli-tour.yaml servers -raw | jq -r '.servers[].name'"
run "$MCP -c /tmp/mcp-cli-tour.yaml discover read -raw | jq -c '[.servers[] | {server, tools: [.tools[].name]}]'"

rm -f /tmp/mcp-cli-tour.yaml /tmp/mcp-cli-tour-http.log
printf '\n\033[1mTour complete.\033[0m\n'
