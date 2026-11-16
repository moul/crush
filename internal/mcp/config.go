package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// This file reads which MCP servers there are, from files in Claude Code's
// shape (spec/cc/mcp.md, sections 1 and 2):
//
//	{"mcpServers": {"<name>": {"command": "...", "args": [...], "env": {...}}}}
//
// gnosh reads two of them. The user's own, mcp.json in gnosh's config
// folder, holds the user's servers at its top, for every project, and
// under "projects", by each project's root, the servers of one project
// that are the person's alone (Claude Code's local scope, kept in the same
// shape as its ~/.claude.json). The project's .mcp.json at the project's
// root counts only in a folder the person has trusted, because each entry
// is a program to run. A name in more than one is the local entry, else
// the project's, whole; fields are not merged.

// Config is one configured server.
type Config struct {
	// Name is the server's name, as the configuration gives it.
	Name string
	// Type is "stdio", or "http" for a server reached at URL.
	Type string
	// Command, Args and Env start a stdio server; ${VAR} and
	// ${VAR:-default} in them are filled in from the environment.
	Command string
	Args    []string
	Env     map[string]string
	// URL and Headers reach an http server.
	URL     string
	Headers map[string]string
	// Timeout bounds one tool call; zero means no bound.
	Timeout time.Duration
	// Scope is "local", "project", "user" or "command line", and Path the
	// file it came from ("--mcp-config" for JSON given whole).
	Scope string
	Path  string
	// Shown is the server in a few words, to show the person: a started
	// server's command and arguments as they are written (with ${VAR}
	// unfilled), or the scheme and host of a server's URL, the rest of
	// which may hold a key.
	Shown string
}

// Problem is something wrong with one entry, or with a whole file, which
// is then not used.
type Problem struct {
	Path, Server, Message string
}

func (p Problem) String() string {
	if p.Server == "" {
		return p.Path + ": " + p.Message
	}
	return fmt.Sprintf("%s: the server %q: %s", p.Path, p.Server, p.Message)
}

// Sources are the files LoadConfig reads, and how it fills in ${VAR}.
type Sources struct {
	// User is the user's own file, and Project the project's .mcp.json,
	// which counts only when Trusted. Root is the project's root.
	User, Project, Root string
	Trusted             bool
	// Getenv fills in ${VAR}; for a stdio server, CLAUDE_PROJECT_DIR and
	// GNOSH_PROJECT_DIR are the project's root.
	Getenv func(string) (string, bool)
	// Withheld are variables a server at a URL is never sent, beside the
	// credentials Withheld names on its own: gnosh's model keys, say.
	Withheld []string
	// Given are servers from the command line (--mcp-config): each a file,
	// or the JSON itself, in the shape of .mcp.json. A relative file is
	// found from GivenFrom. Their entries win over every file's, and with
	// Strict they are the only ones read.
	Given     []string
	GivenFrom string
	Strict    bool
}

// withheld are the variables that read as empty in the url and headers of
// a server at a URL, so that a configuration, a repository's above all,
// cannot send them to a server it names: model providers' keys, cloud
// providers' credentials, proxies (whose addresses may hold a password)
// and package registries' tokens.
var withheld = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	"OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "MISTRAL_API_KEY",
	"GROQ_API_KEY", "DEEPSEEK_API_KEY", "OPENROUTER_API_KEY", "XAI_API_KEY",
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	"AWS_BEARER_TOKEN_BEDROCK", "GOOGLE_APPLICATION_CREDENTIALS",
	"AZURE_CLIENT_SECRET", "AZURE_OPENAI_API_KEY", "AZURE_API_KEY",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy",
	"NPM_TOKEN", "NODE_AUTH_TOKEN",
}

// LoadConfig reads the user's file and, when trusted, the project's. A file
// that is not there is no problem.
func LoadConfig(src Sources) ([]Config, []Problem) {
	var probs []Problem
	byName := map[string]Config{}
	held := map[string]bool{}
	for _, v := range append(append([]string(nil), withheld...), src.Withheld...) {
		held[v] = true
	}
	read := func(path string) *configFile {
		if path == "" {
			return nil
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			probs = append(probs, Problem{Path: path, Message: err.Error()})
			return nil
		}
		var f configFile
		if err := json.NewDecoder(bytes.NewReader(data)).Decode(&f); err != nil {
			probs = append(probs, Problem{Path: path, Message: fmt.Sprintf("it is not JSON in the shape {\"mcpServers\": {...}} (%v), so none of it is used", err)})
			return nil
		}
		return &f
	}
	use := func(path, scope string, servers map[string]json.RawMessage) {
		got, wrong := parseConfig(path, scope, servers, src.Root, src.Getenv, held)
		probs = append(probs, wrong...)
		for _, c := range got {
			byName[c.Name] = c
		}
	}
	if !src.Strict {
		user := read(src.User)
		if user != nil {
			use(src.User, "user", user.Servers)
		}
		if src.Trusted {
			if project := read(src.Project); project != nil {
				use(src.Project, "project", project.Servers)
			}
		}
		if user != nil && src.Root != "" {
			if own, ok := user.Projects[src.Root]; ok {
				use(src.User, "local", own.Servers)
			}
		}
	}
	for _, given := range src.Given {
		if t := strings.TrimSpace(given); strings.HasPrefix(t, "{") {
			var f configFile
			if err := json.Unmarshal([]byte(t), &f); err != nil {
				probs = append(probs, Problem{Path: "--mcp-config", Message: fmt.Sprintf("the JSON given is not in the shape {\"mcpServers\": {...}} (%v), so none of it is used", err)})
				continue
			}
			use("--mcp-config", "command line", f.Servers)
			continue
		}
		path := given
		if !filepath.IsAbs(path) && src.GivenFrom != "" {
			path = filepath.Join(src.GivenFrom, path)
		}
		f := read(path)
		if f == nil {
			if _, err := os.Stat(path); err != nil {
				probs = append(probs, Problem{Path: path, Message: "--mcp-config names it, and it cannot be read: " + err.Error()})
			}
			continue
		}
		use(path, "command line", f.Servers)
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Config, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out, probs
}

// configFile is one file of servers: its own at the top, and the user's
// file's servers of each project under "projects".
type configFile struct {
	Servers  map[string]json.RawMessage `json:"mcpServers"`
	Projects map[string]struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	} `json:"projects"`
}

// parseConfig reads the servers of one scope of a file.
func parseConfig(path, scope string, servers map[string]json.RawMessage, projectRoot string, getenv func(string) (string, bool), held map[string]bool) ([]Config, []Problem) {
	var probs []Problem
	env := func(name string) (string, bool) {
		if name == "CLAUDE_PROJECT_DIR" || name == "GNOSH_PROJECT_DIR" {
			if v, ok := getenv(name); ok {
				return v, true
			}
			return projectRoot, projectRoot != ""
		}
		return getenv(name)
	}
	var out []Config
	// By name, so that the problems a file raises are the same text in
	// the same order every run; a map is read in no order at all.
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := servers[name]
		var e struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Timeout *float64          `json:"timeout"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			probs = append(probs, Problem{Path: path, Server: name, Message: fmt.Sprintf("its entry cannot be read (%v)", err)})
			continue
		}
		c := Config{Name: name, Scope: scope, Path: path}
		var unset, kept []string
		x := func(s string) string {
			out, missing := expand(s, env)
			unset = append(unset, missing...)
			return out
		}
		// A server at a URL is sent no credential, and nothing of the
		// folders here.
		remote := func(s string) string {
			out, missing := expand(s, func(name string) (string, bool) {
				if held[name] {
					if !slices.Contains(kept, name) {
						kept = append(kept, name)
					}
					return "", true
				}
				return getenv(name)
			})
			unset = append(unset, missing...)
			return out
		}
		switch strings.ToLower(e.Type) {
		case "", "stdio":
			if e.Type == "" && e.URL != "" {
				probs = append(probs, Problem{Path: path, Server: name, Message: "it has a url and no type; a server reached at a URL says \"type\": \"http\""})
				continue
			}
			if strings.TrimSpace(e.Command) == "" {
				probs = append(probs, Problem{Path: path, Server: name, Message: "it has no command to start"})
				continue
			}
			c.Type, c.Command = "stdio", x(e.Command)
			for _, a := range e.Args {
				c.Args = append(c.Args, x(a))
			}
			c.Shown = strings.Join(append([]string{e.Command}, e.Args...), " ")
			if len(e.Env) > 0 {
				c.Env = map[string]string{}
				for k, v := range e.Env {
					c.Env[k] = x(v)
				}
			}
		case "http", "streamable-http":
			if strings.TrimSpace(e.URL) == "" {
				probs = append(probs, Problem{Path: path, Server: name, Message: "it is an http server with no url"})
				continue
			}
			c.Type, c.URL = "http", remote(e.URL)
			c.Shown = siteOf(c.URL)
			if len(e.Headers) > 0 {
				c.Headers = map[string]string{}
				for k, v := range e.Headers {
					c.Headers[k] = remote(v)
				}
			}
		case "sse", "ws":
			probs = append(probs, Problem{Path: path, Server: name, Message: fmt.Sprintf("the %s transport is not one gnosh speaks; it speaks stdio and http", e.Type)})
			continue
		default:
			probs = append(probs, Problem{Path: path, Server: name, Message: fmt.Sprintf("%q is not a type of server; the types are stdio and http", e.Type)})
			continue
		}
		if e.Timeout != nil && *e.Timeout >= 1000 {
			c.Timeout = time.Duration(*e.Timeout) * time.Millisecond
		}
		if len(unset) > 0 {
			sort.Strings(unset)
			unset = slices.Compact(unset)
			probs = append(probs, Problem{Path: path, Server: name, Message: "it names variables that are not set and have no default: " + strings.Join(unset, ", ") + "; they are left as written"})
		}
		if len(kept) > 0 {
			sort.Strings(kept)
			probs = append(probs, Problem{Path: path, Server: name, Message: "its url or headers name " + strings.Join(kept, ", ") + ", which a server at a URL is never sent; they read as empty"})
		}
		out = append(out, c)
	}
	return out, probs
}

// siteOf is a URL's scheme and host, or as much of the URL as is not its
// path, query or fragment when it cannot be read.
func siteOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return raw[:i+3] + rest
	}
	return "a URL that cannot be read"
}

var variable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expand fills in ${VAR} and ${VAR:-default}. A variable set to nothing is
// set. One that is not set and has no default is left as it is written,
// and named.
func expand(s string, getenv func(string) (string, bool)) (string, []string) {
	var unset []string
	out := variable.ReplaceAllStringFunc(s, func(m string) string {
		parts := variable.FindStringSubmatch(m)
		if v, ok := getenv(parts[1]); ok {
			return v
		}
		if parts[2] != "" {
			return parts[3]
		}
		unset = append(unset, parts[1])
		return m
	})
	return out, unset
}

// ToolName is the name a server's tool goes by: mcp__<server>__<tool>,
// with each character of the server's name that is not a letter, a digit,
// _ or - made a _.
func ToolName(server, tool string) string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	return "mcp__" + clean(server) + "__" + clean(tool)
}
