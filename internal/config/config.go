// Package config loads the mcp-cli configuration file: the set of MCP servers
// the CLI can talk to, plus how to authenticate to each one.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Transport identifies how the CLI reaches an MCP server.
type Transport string

const (
	// TransportStdio launches the server as a child process and speaks
	// newline-delimited JSON-RPC over its stdin/stdout.
	TransportStdio Transport = "stdio"
	// TransportHTTP is the Streamable HTTP transport (MCP 2025-03-26 and later).
	TransportHTTP Transport = "http"
	// TransportSSE is the legacy HTTP+SSE transport (MCP 2024-11-05).
	TransportSSE Transport = "sse"
)

// Auth describes how to authenticate to an HTTP-based MCP server. For stdio
// servers credentials normally travel in Env instead.
type Auth struct {
	// Type is one of: bearer, basic, header, none. Empty means none.
	Type string `yaml:"type" json:"type,omitempty"`
	// Token is the bearer token / API key. Supports ${ENV_VAR} expansion.
	Token string `yaml:"token" json:"-"`
	// TokenFile reads the token from a file (trailing whitespace trimmed).
	TokenFile string `yaml:"token_file" json:"token_file,omitempty"`
	// TokenCommand runs a command and uses its stdout as the token. Useful for
	// `op read`, `gcloud auth print-access-token`, `pass show`, etc.
	TokenCommand string `yaml:"token_command" json:"token_command,omitempty"`
	// Username/Password back Type: basic.
	Username string `yaml:"username" json:"username,omitempty"`
	Password string `yaml:"password" json:"-"`
	// Header names the header for Type: header (default Authorization).
	Header string `yaml:"header" json:"header,omitempty"`
	// Prefix is prepended to the token for Type: header / bearer.
	// Defaults to "Bearer " for bearer.
	Prefix string `yaml:"prefix" json:"prefix,omitempty"`
}

// Server is one configured MCP server.
type Server struct {
	Name      string    `yaml:"-" json:"name"`
	Transport Transport `yaml:"transport" json:"transport"`
	Disabled  bool      `yaml:"disabled" json:"disabled,omitempty"`

	// stdio
	Command string            `yaml:"command" json:"command,omitempty"`
	Args    []string          `yaml:"args" json:"args,omitempty"`
	Env     map[string]string `yaml:"env" json:"env,omitempty"`
	Dir     string            `yaml:"dir" json:"dir,omitempty"`

	// http / sse
	URL     string            `yaml:"url" json:"url,omitempty"`
	Headers map[string]string `yaml:"headers" json:"headers,omitempty"`
	Auth    *Auth             `yaml:"auth" json:"auth,omitempty"`
	// Insecure skips TLS verification. For self-signed internal endpoints.
	Insecure bool `yaml:"insecure" json:"insecure,omitempty"`

	Timeout     Duration `yaml:"timeout" json:"timeout,omitempty"`
	Description string   `yaml:"description" json:"description,omitempty"`
}

// Defaults apply to every server unless the server overrides them.
type Defaults struct {
	Timeout Duration `yaml:"timeout" json:"timeout,omitempty"`
	Server  string   `yaml:"server" json:"server,omitempty"`
}

// Config is the whole config file.
type Config struct {
	Version  int                `yaml:"version" json:"version"`
	Defaults Defaults           `yaml:"defaults" json:"defaults"`
	Servers  map[string]*Server `yaml:"servers" json:"servers"`

	// Path records where the config was loaded from ("" for a synthesised one).
	Path string `yaml:"-" json:"path,omitempty"`
}

// Duration is a time.Duration that unmarshals from "30s"-style YAML strings and
// from a plain number of seconds.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	// A bare number means seconds; anything else must be a Go duration string.
	if n.Tag == "!!int" || n.Tag == "!!float" {
		var secs float64
		if err := n.Decode(&secs); err != nil {
			return err
		}
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	var s string
	if err := n.Decode(&s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var secs float64
	if err := n.Decode(&secs); err != nil {
		return fmt.Errorf("invalid duration: want a string like \"30s\" or a number of seconds")
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

// D returns the duration as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// ExpandEnv replaces ${VAR} and ${VAR:-default} with values from the
// environment. Unset variables without a default expand to "".
func ExpandEnv(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		g := envRef.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(g[1]); ok && v != "" {
			return v
		}
		return g[3]
	})
}

// DefaultPaths lists where Load looks when no explicit path is given, in order.
func DefaultPaths() []string {
	var out []string
	if p := os.Getenv("MCP_CLI_CONFIG"); p != "" {
		out = append(out, p)
	}
	out = append(out, "mcp-cli.yaml", "mcp-cli.yml", ".mcp-cli.yaml")
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, ".config", "mcp-cli", "config.yaml"),
			filepath.Join(home, ".config", "mcp-cli", "config.yml"),
			filepath.Join(home, ".config", "mcp-cli", "config.json"),
			filepath.Join(home, ".mcp-cli.yaml"),
		)
	}
	return out
}

// ErrNoConfig is returned by Load when no config file exists at any candidate
// path. Callers that can operate from flags alone may ignore it.
type ErrNoConfig struct{ Tried []string }

func (e *ErrNoConfig) Error() string {
	return "no config file found (tried: " + strings.Join(e.Tried, ", ") + ")"
}

// Load reads the config from path, or from the first existing default path.
func Load(path string) (*Config, error) {
	candidates := []string{path}
	if path == "" {
		candidates = DefaultPaths()
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) && path == "" {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		cfg, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		cfg.Path = p
		return cfg, nil
	}
	return nil, &ErrNoConfig{Tried: candidates}
}

// Parse decodes config bytes. YAML is a superset of JSON, so .json files work.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]*Server{}
	}
	for name, s := range cfg.Servers {
		if s == nil {
			return nil, fmt.Errorf("server %q: empty definition", name)
		}
		s.Name = name
		if s.Transport == "" {
			// Infer: a URL means HTTP, a command means stdio.
			switch {
			case s.URL != "":
				s.Transport = TransportHTTP
			case s.Command != "":
				s.Transport = TransportStdio
			}
		}
		if s.Timeout == 0 {
			s.Timeout = cfg.Defaults.Timeout
		}
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("server %q: %w", name, err)
		}
	}
	return &cfg, nil
}

// Validate checks that a server definition is internally consistent.
func (s *Server) Validate() error {
	switch s.Transport {
	case TransportStdio:
		if s.Command == "" {
			return fmt.Errorf("transport stdio requires \"command\"")
		}
		if s.URL != "" {
			return fmt.Errorf("transport stdio does not use \"url\"")
		}
	case TransportHTTP, TransportSSE:
		if s.URL == "" {
			return fmt.Errorf("transport %s requires \"url\"", s.Transport)
		}
		if s.Command != "" {
			return fmt.Errorf("transport %s does not use \"command\"", s.Transport)
		}
	case "":
		return fmt.Errorf("missing \"transport\" (and neither \"url\" nor \"command\" was set to infer it)")
	default:
		return fmt.Errorf("unknown transport %q (want stdio, http or sse)", s.Transport)
	}
	if s.Auth != nil {
		switch s.Auth.Type {
		case "", "none", "bearer", "basic", "header":
		default:
			return fmt.Errorf("unknown auth type %q (want bearer, basic, header or none)", s.Auth.Type)
		}
	}
	return nil
}

// Get returns the named server, or the configured default when name is "".
func (c *Config) Get(name string) (*Server, error) {
	if name == "" {
		name = c.Defaults.Server
		if name == "" && len(c.Servers) == 1 {
			for n := range c.Servers {
				name = n
			}
		}
		if name == "" {
			return nil, fmt.Errorf("no server given and no default configured (known: %s)", strings.Join(c.Names(), ", "))
		}
	}
	s, ok := c.Servers[name]
	if !ok {
		return nil, fmt.Errorf("unknown server %q (known: %s)", name, strings.Join(c.Names(), ", "))
	}
	if s.Disabled {
		return nil, fmt.Errorf("server %q is disabled in %s", name, c.Path)
	}
	return s, nil
}

// Names returns configured server names in sorted order.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ResolvedEnv returns the environment for a stdio server: the parent
// environment plus the server's Env, with ${VAR} references expanded. A value
// of "" passes the parent variable through unchanged (docker -e style).
func (s *Server) ResolvedEnv() []string {
	if len(s.Env) == 0 {
		return nil
	}
	env := os.Environ()
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := ExpandEnv(s.Env[k])
		if v == "" {
			if pv, ok := os.LookupEnv(k); ok {
				v = pv
			}
		}
		env = append(env, k+"="+v)
	}
	return env
}

// ResolvedArgs returns Args with ${VAR} references expanded.
func (s *Server) ResolvedArgs() []string {
	if len(s.Args) == 0 {
		return nil
	}
	out := make([]string, len(s.Args))
	for i, a := range s.Args {
		out[i] = ExpandEnv(a)
	}
	return out
}

// ResolvedHeaders returns the HTTP headers for the server, combining explicit
// headers with whatever the auth block resolves to.
func (s *Server) ResolvedHeaders() (map[string]string, error) {
	h := map[string]string{}
	for k, v := range s.Headers {
		h[k] = ExpandEnv(v)
	}
	if s.Auth == nil {
		return h, nil
	}
	tok, err := s.Auth.Resolve()
	if err != nil {
		return nil, err
	}
	switch s.Auth.Type {
	case "", "none":
	case "bearer":
		if tok == "" {
			return nil, fmt.Errorf("auth type bearer but the token resolved to empty (check the referenced env var or token_file)")
		}
		prefix := s.Auth.Prefix
		if prefix == "" {
			prefix = "Bearer "
		}
		h["Authorization"] = prefix + tok
	case "basic":
		user := ExpandEnv(s.Auth.Username)
		pass := ExpandEnv(s.Auth.Password)
		if pass == "" {
			pass = tok
		}
		h["Authorization"] = "Basic " + basicEncode(user, pass)
	case "header":
		name := s.Auth.Header
		if name == "" {
			name = "Authorization"
		}
		if tok == "" {
			return nil, fmt.Errorf("auth type header but the token resolved to empty")
		}
		h[name] = s.Auth.Prefix + tok
	}
	return h, nil
}

// Resolve returns the auth token from whichever source is configured.
func (a *Auth) Resolve() (string, error) {
	switch {
	case a.Token != "":
		return ExpandEnv(a.Token), nil
	case a.TokenFile != "":
		b, err := os.ReadFile(ExpandEnv(a.TokenFile))
		if err != nil {
			return "", fmt.Errorf("token_file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	case a.TokenCommand != "":
		cmd := exec.Command("sh", "-c", ExpandEnv(a.TokenCommand))
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("token_command %q: %w", a.TokenCommand, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	return "", nil
}

func basicEncode(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
