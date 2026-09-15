package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/justynroberts/mcp-cli/internal/config"
	"github.com/justynroberts/mcp-cli/internal/mcp"
	"github.com/justynroberts/mcp-cli/internal/output"
	"github.com/justynroberts/mcp-cli/internal/session"
)

var cmdServers = &command{
	name:    "servers",
	aliases: []string{"ls"},
	usage:   "servers",
	summary: "List the servers defined in the config file.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		cfg, err := c.loadConfig()
		if err != nil {
			return nil, err
		}
		type entry struct {
			Name        string           `json:"name"`
			Transport   config.Transport `json:"transport"`
			Target      string           `json:"target"`
			Auth        string           `json:"auth,omitempty"`
			Disabled    bool             `json:"disabled,omitempty"`
			Description string           `json:"description,omitempty"`
		}
		servers := make([]entry, 0, len(cfg.Servers))
		for _, name := range cfg.Names() {
			s := cfg.Servers[name]
			target := s.URL
			if target == "" {
				target = strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
			}
			auth := ""
			if s.Auth != nil && s.Auth.Type != "" && s.Auth.Type != "none" {
				auth = s.Auth.Type
			}
			servers = append(servers, entry{
				Name: name, Transport: s.Transport, Target: target,
				Auth: auth, Disabled: s.Disabled, Description: s.Description,
			})
		}
		return map[string]any{
			"config":  cfg.Path,
			"default": cfg.Defaults.Server,
			"servers": servers,
		}, nil
	},
}

var cmdInfo = &command{
	name:    "info",
	usage:   "info <server>",
	summary: "Show a server's identity, protocol version and capabilities.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		s, err := c.open(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return map[string]any{
			"server":           s.Init.ServerInfo,
			"protocol_version": s.Init.ProtocolVersion,
			"capabilities":     s.Init.Capabilities,
			"instructions":     s.Init.Instructions,
			"transport":        s.Server.Transport,
		}, nil
	},
}

var cmdPing = &command{
	name:    "ping",
	usage:   "ping <server>",
	summary: "Check that a server is reachable and responding.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		s, err := c.open(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		reqCtx, cancel := s.Context(ctx)
		defer cancel()
		if err := s.Client.Ping(reqCtx); err != nil {
			return nil, output.Classify("rpc", err)
		}
		return map[string]any{"alive": true, "server": s.Init.ServerInfo}, nil
	},
}

var cmdTools = &command{
	name:    "tools",
	usage:   "tools <server> [substring]",
	summary: "List a server's tools; add -full for input schemas.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		s, err := c.open(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		filter := ""
		if len(c.args) > 0 {
			filter = strings.ToLower(c.args[0])
		}
		reqCtx, cancel := s.Context(ctx)
		defer cancel()
		tools, err := s.Client.ListTools(reqCtx)
		if err != nil {
			return nil, output.Classify("rpc", err)
		}
		out := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			if filter != "" && !strings.Contains(strings.ToLower(t.Name+" "+t.Description), filter) {
				continue
			}
			e := map[string]any{"name": t.Name}
			if t.Title != "" {
				e["title"] = t.Title
			}
			if t.Description != "" {
				e["description"] = t.Description
			}
			if c.g.full {
				if len(t.InputSchema) > 0 {
					e["input_schema"] = json.RawMessage(t.InputSchema)
				}
				if len(t.OutputSchema) > 0 {
					e["output_schema"] = json.RawMessage(t.OutputSchema)
				}
			} else if args := schemaSummary(t.InputSchema); args != nil {
				e["arguments"] = args
			}
			out = append(out, e)
		}
		return map[string]any{"count": len(out), "tools": out}, nil
	},
}

var cmdSchema = &command{
	name:    "schema",
	usage:   "schema <server> <tool>",
	summary: "Print one tool's full input (and output) JSON schema.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		s, err := c.open(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if len(c.args) < 1 {
			return nil, output.Errorf("usage", "schema needs a tool name")
		}
		name := c.args[0]
		reqCtx, cancel := s.Context(ctx)
		defer cancel()
		tool, err := findTool(reqCtx, c, s, name)
		if err != nil {
			return nil, err
		}
		res := map[string]any{"name": tool.Name, "description": tool.Description}
		if len(tool.InputSchema) > 0 {
			res["input_schema"] = json.RawMessage(tool.InputSchema)
		}
		if len(tool.OutputSchema) > 0 {
			res["output_schema"] = json.RawMessage(tool.OutputSchema)
		}
		return res, nil
	},
}

var cmdCall = &command{
	name:    "call",
	usage:   "call <server> <tool> [key=value ...]",
	summary: "Call a tool and print its result as JSON. Use key:=json for non-strings, key=@file to read a file.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		s, err := c.open(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if len(c.args) < 1 {
			return nil, output.Errorf("usage", "call needs a tool name: mcp-cli call <server> <tool> [key=value ...]")
		}
		name, rest := c.args[0], c.args[1:]

		specs := make([]argSpec, 0, len(rest))
		for _, tok := range rest {
			spec, err := parseArg(tok)
			if err != nil {
				return nil, output.Wrap("usage", err)
			}
			specs = append(specs, spec)
		}

		reqCtx, cancel := s.Context(ctx)
		defer cancel()

		tool, err := findTool(reqCtx, c, s, name)
		if err != nil {
			return nil, err
		}
		args, err := buildArguments(nil, specs, tool.InputSchema)
		if err != nil {
			return nil, output.Wrap("usage", err)
		}
		if missing := missingRequired(args, tool.InputSchema); len(missing) > 0 {
			return nil, output.Errorf("usage", "tool %q requires: %s (see `%s`)",
				tool.Name, strings.Join(missing, ", "), c.invocationHint("schema", tool.Name))
		}

		res, err := s.Client.CallTool(reqCtx, tool.Name, args)
		if err != nil {
			return nil, output.Classify("rpc", err)
		}
		out := buildToolOutput(tool.Name, res, c.g.full)
		if res.IsError {
			msg := out.Text
			if msg == "" {
				msg = "tool reported an error with no text content"
			}
			return nil, output.Errorf("tool", "%s: %s", tool.Name, msg)
		}
		if c.g.raw {
			return out.payload(), nil
		}
		return out, nil
	},
}

var cmdResources = &command{
	name:    "resources",
	usage:   "resources <server> [substring]",
	summary: "List a server's resources and resource templates.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		s, err := c.open(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		filter := ""
		if len(c.args) > 0 {
			filter = strings.ToLower(c.args[0])
		}
		reqCtx, cancel := s.Context(ctx)
		defer cancel()
		res, err := s.Client.ListResources(reqCtx)
		if err != nil {
			return nil, output.Classify("rpc", err)
		}
		tmpl, err := s.Client.ListResourceTemplates(reqCtx)
		if err != nil {
			return nil, output.Classify("rpc", err)
		}
		keep := make([]mcp.Resource, 0, len(res))
		for _, r := range res {
			if filter == "" || strings.Contains(strings.ToLower(r.URI+" "+r.Name+" "+r.Description), filter) {
				keep = append(keep, r)
			}
		}
		return map[string]any{
			"count":     len(keep),
			"resources": keep,
			"templates": tmpl,
		}, nil
	},
}

var cmdRead = &command{
	name:    "read",
	usage:   "read <server> <uri>",
	summary: "Read one resource URI and print its contents.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		s, err := c.open(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if len(c.args) < 1 {
			return nil, output.Errorf("usage", "read needs a resource URI")
		}
		uri := c.args[0]
		reqCtx, cancel := s.Context(ctx)
		defer cancel()
		res, err := s.Client.ReadResource(reqCtx, uri)
		if err != nil {
			return nil, output.Classify("rpc", err)
		}
		var texts []string
		for _, ct := range res.Contents {
			if ct.Text != "" {
				texts = append(texts, ct.Text)
			}
		}
		joined := strings.Join(texts, "\n")
		out := map[string]any{"uri": uri, "contents": res.Contents}
		if parsed, ok := parseJSON(joined); ok {
			out["json"] = parsed
			if c.g.raw {
				return parsed, nil
			}
		} else if c.g.raw && joined != "" {
			return joined, nil
		}
		return out, nil
	},
}

var cmdPrompts = &command{
	name:    "prompts",
	usage:   "prompts <server>",
	summary: "List a server's prompt templates.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		s, err := c.open(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		reqCtx, cancel := s.Context(ctx)
		defer cancel()
		prompts, err := s.Client.ListPrompts(reqCtx)
		if err != nil {
			return nil, output.Classify("rpc", err)
		}
		return map[string]any{"count": len(prompts), "prompts": prompts}, nil
	},
}

var cmdPrompt = &command{
	name:    "prompt",
	usage:   "prompt <server> <name> [key=value ...]",
	summary: "Render a prompt template with arguments.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		s, err := c.open(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if len(c.args) < 1 {
			return nil, output.Errorf("usage", "prompt needs a prompt name")
		}
		name, rest := c.args[0], c.args[1:]
		args := map[string]any{}
		for _, tok := range rest {
			spec, err := parseArg(tok)
			if err != nil {
				return nil, output.Wrap("usage", err)
			}
			// Prompt arguments are strings by protocol definition.
			args[spec.key] = fmt.Sprint(spec.value)
		}
		reqCtx, cancel := s.Context(ctx)
		defer cancel()

		prompt, err := findPrompt(reqCtx, c, s, name)
		if err != nil {
			return nil, err
		}
		var missing []string
		for _, a := range prompt.Arguments {
			if _, ok := args[a.Name]; a.Required && !ok {
				missing = append(missing, a.Name)
			}
		}
		if len(missing) > 0 {
			return nil, output.Errorf("usage", "prompt %q requires: %s (run `%s` to see them)",
				prompt.Name, strings.Join(missing, ", "), c.invocationHint("prompts"))
		}
		res, err := s.Client.GetPrompt(reqCtx, prompt.Name, args)
		if err != nil {
			return nil, output.Classify("rpc", err)
		}
		return res, nil
	},
}

var cmdDiscover = &command{
	name:    "discover",
	usage:   "discover [substring]",
	summary: "List matching tools across every configured server, in parallel.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		cfg, err := c.loadConfig()
		if err != nil {
			return nil, err
		}
		filter := ""
		if len(c.args) > 0 {
			filter = strings.ToLower(c.args[0])
		}
		type serverTools struct {
			Server string           `json:"server"`
			Tools  []map[string]any `json:"tools,omitempty"`
			Error  string           `json:"error,omitempty"`
		}
		names := cfg.Names()
		results := make([]serverTools, len(names))
		var wg sync.WaitGroup
		for i, name := range names {
			srv := cfg.Servers[name]
			results[i] = serverTools{Server: name}
			if srv.Disabled {
				results[i].Error = "disabled"
				continue
			}
			wg.Add(1)
			go func(i int, srv *config.Server) {
				defer wg.Done()
				s, err := c.openServer(ctx, srv)
				if err != nil {
					results[i].Error = err.Error()
					return
				}
				defer s.Close()
				reqCtx, cancel := s.Context(ctx)
				defer cancel()
				tools, err := s.Client.ListTools(reqCtx)
				if err != nil {
					results[i].Error = err.Error()
					return
				}
				for _, t := range tools {
					if filter != "" && !strings.Contains(strings.ToLower(t.Name+" "+t.Description), filter) {
						continue
					}
					e := map[string]any{"name": t.Name}
					if t.Description != "" {
						e["description"] = t.Description
					}
					if args := schemaSummary(t.InputSchema); args != nil {
						e["arguments"] = args
					}
					results[i].Tools = append(results[i].Tools, e)
				}
			}(i, srv)
		}
		wg.Wait()
		total := 0
		for _, r := range results {
			total += len(r.Tools)
		}
		return map[string]any{"count": total, "servers": results}, nil
	},
}

var cmdInit = &command{
	name:    "init",
	usage:   "init [path]",
	summary: "Write a starter config file (default ~/.config/mcp-cli/config.yaml).",
	run: func(ctx context.Context, c *runContext) (any, error) {
		path := ""
		if len(c.args) > 0 {
			path = c.args[0]
		}
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, output.Wrap("io", err)
			}
			path = filepath.Join(home, ".config", "mcp-cli", "config.yaml")
		}
		if _, err := os.Stat(path); err == nil {
			return nil, output.Errorf("io", "%s already exists — delete it or pass a different path", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, output.Wrap("io", err)
		}
		if err := os.WriteFile(path, []byte(starterConfig), 0o600); err != nil {
			return nil, output.Wrap("io", err)
		}
		return map[string]any{"written": path, "next": "edit the file, then run: mcp-cli servers"}, nil
	},
}

var cmdVersion = &command{
	name:    "version",
	usage:   "version",
	summary: "Print the CLI and protocol versions.",
	run: func(ctx context.Context, c *runContext) (any, error) {
		return map[string]any{
			"version":           Version,
			"protocol":          mcp.ProtocolVersion,
			"protocol_fallback": mcp.FallbackProtocolVersions,
		}, nil
	},
}

// findTool looks up a tool by exact name, falling back to a unique
// case-insensitive or substring match so callers can be approximate.
func findTool(ctx context.Context, c *runContext, s *session.Session, name string) (*mcp.Tool, error) {
	tools, err := s.Client.ListTools(ctx)
	if err != nil {
		return nil, output.Classify("rpc", err)
	}
	var partial []mcp.Tool
	for _, t := range tools {
		if t.Name == name {
			return &t, nil
		}
		if strings.EqualFold(t.Name, name) || strings.Contains(strings.ToLower(t.Name), strings.ToLower(name)) {
			partial = append(partial, t)
		}
	}
	switch len(partial) {
	case 1:
		return &partial[0], nil
	case 0:
		return nil, output.Errorf("usage", "server %q has no tool %q (run `%s` to list them)",
			s.Server.Name, name, c.invocationHint("tools"))
	default:
		names := make([]string, len(partial))
		for i, t := range partial {
			names[i] = t.Name
		}
		sort.Strings(names)
		return nil, output.Errorf("usage", "%q matches several tools: %s", name, strings.Join(names, ", "))
	}
}

// findPrompt resolves a prompt name the same way findTool resolves a tool
// name: exact match first, then a unique case-insensitive or substring match.
func findPrompt(ctx context.Context, c *runContext, s *session.Session, name string) (*mcp.Prompt, error) {
	prompts, err := s.Client.ListPrompts(ctx)
	if err != nil {
		return nil, output.Classify("rpc", err)
	}
	var partial []mcp.Prompt
	for _, p := range prompts {
		if p.Name == name {
			return &p, nil
		}
		if strings.EqualFold(p.Name, name) || strings.Contains(strings.ToLower(p.Name), strings.ToLower(name)) {
			partial = append(partial, p)
		}
	}
	switch len(partial) {
	case 1:
		return &partial[0], nil
	case 0:
		return nil, output.Errorf("usage", "server %q has no prompt %q (run `%s` to list them)",
			s.Server.Name, name, c.invocationHint("prompts"))
	default:
		names := make([]string, len(partial))
		for i, p := range partial {
			names[i] = p.Name
		}
		sort.Strings(names)
		return nil, output.Errorf("usage", "%q matches several prompts: %s", name, strings.Join(names, ", "))
	}
}

// schemaSummary condenses an input schema to "name: type" pairs, with required
// arguments marked, so `tools` output stays readable without -full.
func schemaSummary(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s jsonSchema
	if err := json.Unmarshal(raw, &s); err != nil || len(s.Properties) == 0 {
		return nil
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		p := s.Properties[n]
		typ := "any"
		if ts := p.typeNames(); len(ts) > 0 {
			typ = strings.Join(ts, "|")
		}
		entry := fmt.Sprintf("%s: %s", n, typ)
		if required[n] {
			entry += " (required)"
		}
		out = append(out, entry)
	}
	return out
}
