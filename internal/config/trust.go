package config

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/lock"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// A folder's own settings, its project settings, are the gnoshrc and
// gnosh.json files (and their klash and crush names) found from the working
// directory up to the repository root, and the gnosh.json gnosh keeps in the
// project's data directory, .gnosh/. A gnoshrc is a Bash script that runs as
// the config loads, and a gnosh.json can start programs (hooks, MCP and LSP
// servers), let tools run without asking and send the API key somewhere else
// (a provider's base_url). Used as they are, a repository cloned from a
// stranger would run the stranger's code with the user's permissions the
// moment gnosh started in it.
//
// So until the user trusts a folder, gnosh runs none of its shell configs
// and uses, of its JSON ones, only the settings that take something away
// from the agent (untrustedKept). The rest are held back, and listed
// (ProjectSettings) for the question the TUI asks, the line gnosh run prints
// and gnosh doctor. So is a project skills folder (.agents/skills and the
// like) that is a symlink out of the project: the agent reads a skill's
// files without asking, so one linked to the home directory would let it
// read the user's keys without a word. Trust is recorded per folder in the global data
// directory, never in the project: for the git repository's main checkout,
// so a linked worktree shares it, or outside a repository for the start
// directory, which then covers the folders under it that are not
// repositories of their own. The home directory is trusted for a session
// only, never recorded, as in Claude Code's workspace trust.

// untrustedKept are the settings of an untrusted folder's JSON configs that
// gnosh still uses, by JSON path. Each only takes something away from the
// agent, so a stranger's repository can make gnosh do less in it, never
// more.
var untrustedKept = []string{
	"permissions.deny",
	"options.disabled_tools",
	"options.disabled_skills",
}

// trustFile records the trusted folders, beside the global data config: the
// user's own machine state, which a project cannot write.
const trustFile = "trusted-folders.json"

// trustRecord is the trust file's content.
type trustRecord struct {
	// Folders maps each trusted folder to when it was trusted (RFC 3339).
	Folders map[string]string `json:"folders"`
}

// ErrHomeTrust is what recording trust for the home directory returns: it
// is trusted for a session at a time, never for good.
var ErrHomeTrust = errors.New("the home directory is trusted for one session at a time, never recorded")

// trustHome is the home directory, which is trusted for a session only; a
// var so a test can stand a directory of its own in.
var trustHome = home.Dir

// ProjectTrust is whether a load uses its folder's project settings.
type ProjectTrust struct {
	// Folder is the folder trust is recorded for: the git repository's
	// main checkout, or the start directory outside a repository.
	Folder string
	// InRepo is set when Folder is a git repository's main checkout.
	InRepo bool
	// Trusted is set when the project settings are used.
	Trusted bool
	// Session is set when they are trusted for this session only: the
	// home directory answered yes, or --trust-folder.
	Session bool
}

// ProjectSettings is what a folder's project settings do, read without
// running any of them: what an untrusted folder holds back.
type ProjectSettings struct {
	// Folder is the folder trust is recorded for.
	Folder string
	// Home is set when Folder is the home directory, trusted for a session
	// only.
	Home bool
	// InRepo is set when Folder is a git repository's main checkout.
	InRepo bool
	// Dir is the directory gnosh started in, which the file names are
	// shown from.
	Dir string
	// Files are the project's config files that hold something back.
	Files []string
	// Scripts are its shell configs (gnoshrc), which run as Bash.
	Scripts []string
	// Hooks are its hook commands, as "Event: command".
	Hooks []string
	// MCPServers and LSPServers are the servers it would start or set up.
	MCPServers []string
	LSPServers []string
	// Providers are the model providers it sets, with their base_url.
	Providers []string
	// Allowed are the tools and Bash rules it lets run without asking.
	Allowed []string
	// Env are the environment variables it sets, by name.
	Env []string
	// Hosts are the machines it adds for a command to run on.
	Hosts []string
	// Skills are its skills folders that lead out of the project, whose
	// files the agent would read without asking, as "folder (where it
	// leads)".
	Skills []string
	// Other are the other settings it holds, by key.
	Other []string
}

// Holds reports whether the settings do anything an untrusted folder holds
// back.
func (p *ProjectSettings) Holds() bool {
	return p != nil && (len(p.Scripts)+len(p.Hooks)+len(p.MCPServers)+len(p.LSPServers)+
		len(p.Providers)+len(p.Allowed)+len(p.Env)+len(p.Hosts)+len(p.Skills)+len(p.Other)) > 0
}

// Lines says what the settings would do, a line each, for the trust
// question and gnosh trust.
func (p *ProjectSettings) Lines() []string {
	if p == nil {
		return nil
	}
	var lines []string
	for _, s := range p.Scripts {
		lines = append(lines, "run "+p.name(s)+", a shell script, as gnosh starts")
	}
	for _, h := range p.Hooks {
		lines = append(lines, "run the hook "+h)
	}
	if len(p.MCPServers) > 0 {
		lines = append(lines, "start "+plural(len(p.MCPServers), "MCP server")+": "+strings.Join(p.MCPServers, ", "))
	}
	if len(p.LSPServers) > 0 {
		lines = append(lines, "start or set up "+plural(len(p.LSPServers), "LSP server")+": "+strings.Join(p.LSPServers, ", "))
	}
	if len(p.Allowed) > 0 {
		lines = append(lines, "let these run without asking: "+strings.Join(p.Allowed, ", "))
	}
	if len(p.Providers) > 0 {
		lines = append(lines, "use model providers of its own: "+strings.Join(p.Providers, ", "))
	}
	if len(p.Env) > 0 {
		lines = append(lines, "set environment variables: "+strings.Join(p.Env, ", "))
	}
	if len(p.Hosts) > 0 {
		lines = append(lines, "add machines to run commands on: "+strings.Join(p.Hosts, ", "))
	}
	for _, s := range p.Skills {
		lines = append(lines, "let the agent read without asking, as skills, the files in "+s)
	}
	if len(p.Other) > 0 {
		lines = append(lines, "change other settings: "+strings.Join(p.Other, ", "))
	}
	return lines
}

// Brief says what the settings would do in one short line, for the note
// gnosh run prints.
func (p *ProjectSettings) Brief() string {
	if p == nil {
		return ""
	}
	var parts []string
	for _, s := range p.Scripts {
		parts = append(parts, "runs "+p.name(s))
	}
	if n := len(p.Hooks); n > 0 {
		parts = append(parts, plural(n, "hook"))
	}
	if len(p.MCPServers) > 0 {
		parts = append(parts, plural(len(p.MCPServers), "MCP server")+" "+strings.Join(p.MCPServers, ", "))
	}
	if len(p.LSPServers) > 0 {
		parts = append(parts, plural(len(p.LSPServers), "LSP server")+" "+strings.Join(p.LSPServers, ", "))
	}
	if len(p.Allowed) > 0 {
		parts = append(parts, "allows "+strings.Join(p.Allowed, ", "))
	}
	if len(p.Providers) > 0 {
		parts = append(parts, plural(len(p.Providers), "provider"))
	}
	if len(p.Env) > 0 {
		parts = append(parts, "env "+strings.Join(p.Env, ", "))
	}
	if len(p.Hosts) > 0 {
		parts = append(parts, plural(len(p.Hosts), "host")+" "+strings.Join(p.Hosts, ", "))
	}
	if len(p.Skills) > 0 {
		parts = append(parts, plural(len(p.Skills), "skills folder")+" linked out of the project")
	}
	if len(p.Other) > 0 {
		parts = append(parts, strings.Join(p.Other, ", "))
	}
	return strings.Join(parts, "; ")
}

// name is path as the user knows it: from the directory gnosh started in
// when it is at or under it, and otherwise in full; made safe to show.
func (p *ProjectSettings) name(path string) string {
	if p.Dir != "" {
		if rel, err := filepath.Rel(p.Dir, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return Shown(rel)
		}
	}
	return ShowPath(path)
}

// Shown is s made safe to show on a terminal. A project's settings are a
// stranger's text, and a control character in a server's name, a hook's
// command or a folder's name (an escape sequence, a carriage return, a
// bidi override) could rewrite the line it is shown on, or have the
// terminal do something, so each is shown as its Go escape (\x1b,
// \u202e) instead.
func Shown(s string) string {
	if strings.IndexFunc(s, hidden) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if hidden(r) {
			b.WriteString(strings.Trim(strconv.QuoteRune(r), "'"))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hidden reports whether r acts on the text around it rather than showing
// as itself: a control or format character, or a line or paragraph
// separator.
func hidden(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// ShowPath is path as a message shows it: short (~/...) and made safe to
// show.
func ShowPath(path string) string {
	return Shown(home.Short(path))
}

// plural is n and noun, with an s when n is not 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// split returns what of data, the JSON config at path, an untrusted folder
// still uses (untrustedKept), and notes the rest in p.
func (p *ProjectSettings) split(path string, data []byte) ([]byte, error) {
	kept := []byte("{}")
	for _, key := range untrustedKept {
		v := gjson.GetBytes(data, key)
		if !v.Exists() {
			continue
		}
		var err error
		if kept, err = sjson.SetRawBytes(kept, key, []byte(v.Raw)); err != nil {
			return nil, fmt.Errorf("config file %s: %w", path, err)
		}
	}
	if p.note(data) {
		p.Files = appendNew(p.Files, path)
	}
	return kept, nil
}

// addScript notes path, a shell config an untrusted folder does not run.
func (p *ProjectSettings) addScript(path string) {
	p.Scripts = appendNew(p.Scripts, path)
	p.Files = appendNew(p.Files, path)
}

// note adds to p what data, a project's JSON config, holds back, and
// reports whether it held anything.
func (p *ProjectSettings) note(data []byte) bool {
	noted := false
	add := func(list *[]string, items ...string) {
		for _, item := range items {
			item = Shown(item)
			if !slices.Contains(*list, item) {
				*list = append(*list, item)
			}
			noted = true
		}
	}
	gjson.ParseBytes(data).ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		if k == "$schema" {
			return true
		}
		if !value.IsObject() {
			add(&p.Other, k)
			return true
		}
		switch k {
		case "hooks":
			value.ForEach(func(event, entries gjson.Result) bool {
				add(&p.Hooks, hookCommands(event.String(), entries)...)
				return true
			})
		case "mcp":
			add(&p.MCPServers, enabledNames(value)...)
		case "lsp":
			add(&p.LSPServers, enabledNames(value)...)
		case "providers":
			value.ForEach(func(id, provider gjson.Result) bool {
				item := id.String()
				if u := provider.Get("base_url").String(); u != "" {
					item += " (" + u + ")"
				}
				add(&p.Providers, item)
				return true
			})
		case "permissions":
			value.ForEach(func(field, v gjson.Result) bool {
				switch f := field.String(); f {
				case "deny":
				case "allowed_tools", "allow":
					for _, t := range v.Array() {
						add(&p.Allowed, t.String())
					}
				default:
					add(&p.Other, "permissions."+f)
				}
				return true
			})
		case "env":
			add(&p.Env, keys(value)...)
		case "hosts":
			add(&p.Hosts, keys(value)...)
		case "options":
			value.ForEach(func(field, _ gjson.Result) bool {
				if name := "options." + field.String(); !slices.Contains(untrustedKept, name) {
					add(&p.Other, name)
				}
				return true
			})
		default:
			add(&p.Other, k)
		}
		return true
	})
	return noted
}

// hookCommands are the commands event's hook entries run, as "Event:
// command", whether in gnosh's shape or in Claude Code's groups.
func hookCommands(event string, entries gjson.Result) []string {
	var commands []string
	addCommand := func(h gjson.Result) {
		commands = append(commands, event+": "+cmp.Or(h.Get("command").String(), "(no command)"))
	}
	for _, entry := range entries.Array() {
		if group := entry.Get("hooks"); group.IsArray() {
			for _, h := range group.Array() {
				addCommand(h)
			}
			continue
		}
		addCommand(entry)
	}
	if len(commands) == 0 {
		commands = append(commands, event+": (a hook)")
	}
	return commands
}

// enabledNames are the names of the servers in value that are not
// disabled.
func enabledNames(value gjson.Result) []string {
	var names []string
	value.ForEach(func(name, server gjson.Result) bool {
		if !server.Get("disabled").Bool() {
			names = append(names, name.String())
		}
		return true
	})
	return names
}

// keys are value's keys, in order.
func keys(value gjson.Result) []string {
	var names []string
	value.ForEach(func(name, _ gjson.Result) bool {
		names = append(names, name.String())
		return true
	})
	return names
}

// appendNew is list with item appended unless it is there already.
func appendNew(list []string, item string) []string {
	if slices.Contains(list, item) {
		return list
	}
	return append(list, item)
}

// canonicalDir is dir made absolute, with its symlinks resolved, so one
// folder always has one name in the trust record.
func canonicalDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return filepath.Clean(abs)
}

// isHomeDir reports whether dir, a canonicalDir, is the home directory.
func isHomeDir(dir string) bool {
	h := trustHome()
	return h != "" && canonicalDir(h) == dir
}

// TrustFolderOf returns the folder trust is recorded for when gnosh starts
// in dir: the git repository's main checkout, so that a linked worktree
// shares its main checkout's trust (a submodule, a repository of its own,
// has its own), or outside a repository dir itself. inRepo says which.
//
// What git says is checked first, since a folder's own .git can say
// anything: a .git file is one line naming any git directory, and a .git
// directory's config can set core.worktree to any folder. A work tree that
// does not hold dir is not dir's repository, and a worktree is linked to a
// checkout only when the checkout's record of it names it back; otherwise
// the folder is keyed on itself, and a stranger's folder cannot borrow a
// trusted repository's trust by naming it.
func TrustFolderOf(dir string) (folder string, inRepo bool) {
	abs := canonicalDir(dir)
	cmd := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel", "--git-dir", "--git-common-dir")
	cmd.Dir = abs
	out, err := cmd.Output()
	if err != nil {
		return abs, false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 || slices.Contains(lines, "") {
		return abs, false
	}
	// git prints a path relative to the directory it ran in or absolute.
	resolve := func(p string) string {
		if p = strings.TrimSpace(p); !filepath.IsAbs(p) {
			p = filepath.Join(abs, p)
		}
		return canonicalDir(p)
	}
	top, gitDir, common := resolve(lines[0]), resolve(lines[1]), resolve(lines[2])
	if !isWithin(top, abs) {
		// A repository all the same, so no trusted folder above covers it.
		return abs, true
	}
	// The common directory of a checkout and of every worktree linked to
	// it is the checkout's .git, and a linked worktree's own git directory
	// is .git/worktrees/<name> in it, whose gitdir file names the
	// worktree's .git. A submodule's lives in its superproject's
	// .git/modules, and a bare repository's is the repository: those keep
	// their own top level.
	if gitDir != common && filepath.Base(common) == ".git" && filepath.Dir(gitDir) == filepath.Join(common, "worktrees") {
		if back, err := os.ReadFile(filepath.Join(gitDir, "gitdir")); err == nil {
			// Relative to the git directory where git keeps it so
			// (worktree.useRelativePaths).
			named := strings.TrimSpace(string(back))
			if !filepath.IsAbs(named) {
				named = filepath.Join(gitDir, named)
			}
			if sameFile(named, filepath.Join(top, ".git")) {
				return canonicalDir(filepath.Dir(common)), true
			}
		}
	}
	return top, true
}

// trustPath is the trust file.
func trustPath() string {
	return filepath.Join(filepath.Dir(GlobalConfigData()), trustFile)
}

// readTrust reads the trust file; a missing one trusts nothing.
func readTrust() (trustRecord, error) {
	path := trustPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return trustRecord{}, nil
	}
	if err != nil {
		return trustRecord{}, err
	}
	var r trustRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return trustRecord{}, fmt.Errorf("%s is not the JSON gnosh writes there: %w", path, err)
	}
	return r, nil
}

// updateTrust changes the trust file under its lock.
func updateTrust(change func(folders map[string]string)) error {
	path := trustPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), configLockDeadline)
	defer cancel()
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer release()
	r, err := readTrust()
	if err != nil {
		return err
	}
	if r.Folders == nil {
		r.Folders = map[string]string{}
	}
	change(r.Folders)
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, append(data, '\n'), 0o600)
}

// folderTrusted reports whether folder, as TrustFolderOf returns it, is
// recorded as trusted. Outside a repository a trusted folder above it
// counts too, as in Claude Code. The home directory never does: it is
// trusted for a session only. A trust file gnosh cannot read trusts
// nothing.
func folderTrusted(folder string, inRepo bool) bool {
	if folder == "" || isHomeDir(folder) {
		return false
	}
	r, err := readTrust()
	if err != nil {
		slog.Warn("Trusting no folder: the trust file cannot be read", "error", err)
		return false
	}
	if _, ok := r.Folders[folder]; ok {
		return true
	}
	if inRepo {
		return false
	}
	for dir := folder; ; {
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
		if _, ok := r.Folders[dir]; ok && !isHomeDir(dir) {
			return true
		}
	}
}

// FolderTrusted reports whether gnosh started in dir would use its project
// settings without asking: whether its folder is recorded as trusted.
func FolderTrusted(dir string) bool {
	return folderTrusted(TrustFolderOf(dir))
}

// TrustFolder records the folder of a gnosh started in dir (see
// TrustFolderOf) as trusted, and returns it. The home directory is refused
// with ErrHomeTrust.
func TrustFolder(dir string) (string, error) {
	folder, _ := TrustFolderOf(dir)
	if isHomeDir(folder) {
		return folder, ErrHomeTrust
	}
	return folder, updateTrust(func(folders map[string]string) {
		folders[folder] = time.Now().UTC().Format(time.RFC3339)
	})
}

// ForgetFolder removes the record that dir's folder is trusted, returning
// the folder and whether it was recorded.
func ForgetFolder(dir string) (folder string, removed bool, err error) {
	folder, _ = TrustFolderOf(dir)
	err = updateTrust(func(folders map[string]string) {
		_, removed = folders[folder]
		delete(folders, folder)
	})
	return folder, removed, err
}

// TrustedFolders returns the trusted folders and when each was trusted.
func TrustedFolders() (map[string]string, error) {
	r, err := readTrust()
	if err != nil {
		return nil, err
	}
	return maps.Clone(r.Folders), nil
}

// TrustFile is the file the trusted folders are recorded in.
func TrustFile() string {
	return trustPath()
}

// TrustCommand is the command that trusts folder, for a message that tells
// the user how: gnosh trust and the folder, short (~/...) where a shell
// takes it as it is and quoted in full where it would not.
func TrustCommand(folder string) string {
	return Shown(version.Command + " trust " + shellPath(folder))
}

// shellPath is path as it can be typed into a shell.
func shellPath(path string) string {
	short := home.Short(path)
	plain := strings.IndexFunc(short, func(r rune) bool {
		return r >= utf8.RuneSelf || !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/._-+~:@%,=", r))
	}) < 0
	if plain && !strings.HasPrefix(short, "-") {
		return short
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// decideTrust is how a load in workingDir treats the project's own
// settings: trusted for the session when the caller says so (session),
// and otherwise as the trust record says.
func decideTrust(workingDir string, session bool) ProjectTrust {
	if workingDir == "" {
		return ProjectTrust{Trusted: true}
	}
	folder, inRepo := TrustFolderOf(workingDir)
	if session {
		return ProjectTrust{Folder: folder, InRepo: inRepo, Trusted: true, Session: true}
	}
	return ProjectTrust{Folder: folder, InRepo: inRepo, Trusted: folderTrusted(folder, inRepo)}
}

// projectGate is how one load treats the project's own settings: all of
// them where the folder is trusted, and otherwise only what untrustedKept
// lists, the rest noted in held.
type projectGate struct {
	trusted bool
	// global are the user's own config files. Of the files a load reads,
	// every other one is the project's: the gate holds back what it does
	// not know to be the user's, rather than only what it knows to be the
	// project's, so a file the lookup finds that the gate's list missed
	// stays out too.
	global []string
	// top is the project's top directory, its symlinks resolved: a data
	// directory in it is the project's own.
	top  string
	held *ProjectSettings
}

// newProjectGate is the gate for a load in workingDir under trust.
func newProjectGate(workingDir string, trust ProjectTrust) *projectGate {
	g := &projectGate{trusted: trust.Trusted}
	if workingDir == "" {
		return g
	}
	g.global = globalConfigPaths()
	g.top = canonicalDir(projectBoundary(workingDir))
	g.held = &ProjectSettings{
		Folder: trust.Folder,
		Home:   isHomeDir(trust.Folder),
		InRepo: trust.InRepo,
		Dir:    canonicalDir(workingDir),
	}
	return g
}

// holds reports whether path, a config file the load reads, is held back:
// any file but the user's own global ones, where the folder is not
// trusted.
func (g *projectGate) holds(path string) bool {
	if g == nil || g.trusted || g.held == nil {
		return false
	}
	return !slices.ContainsFunc(g.global, func(global string) bool { return sameFile(global, path) })
}

// holdsWorkspace reports whether path, the workspace config in the data
// directory, is the project's own and held back: it is when the data
// directory is in the project, as .gnosh/ is.
func (g *projectGate) holdsWorkspace(path string) bool {
	return g != nil && !g.trusted && g.inProject(filepath.Dir(path))
}

// inProject reports whether dir, which need not exist, is in the project:
// under its top directory with the symlinks of dir's parents resolved, or
// with dir's own resolved as well. A .gnosh that is a symlink out of the
// project is still the project's, as a repository can carry the link.
func (g *projectGate) inProject(dir string) bool {
	if g.top == "" {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return true
	}
	return isWithin(g.top, canonicalPath(abs)) || isWithin(g.top, canonicalDir(abs))
}

// holdSkillDirs takes out of cfg's skills paths, where the folder is not
// trusted, the project's skills folders (ProjectSkillsDir) that lead out of
// the project, and notes them: the agent reads a skill's files without
// asking, so a .agents/skills linked to the home directory would let it
// read the user's keys without a word. A skills folder inside the project
// adds nothing the agent cannot read anyway, and stays.
func (g *projectGate) holdSkillDirs(cfg *Config, workingDir string) {
	if g == nil || g.trusted || g.held == nil || cfg.Options == nil {
		return
	}
	out := g.skillDirsOut(workingDir)
	cfg.Options.SkillsPaths = slices.DeleteFunc(cfg.Options.SkillsPaths, func(dir string) bool {
		return slices.Contains(out, dir)
	})
}

// skillDirsOut returns the project's skills folders that lead out of the
// project, and notes them in held.
func (g *projectGate) skillDirsOut(workingDir string) []string {
	var out []string
	for _, dir := range ProjectSkillsDir(workingDir) {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || isWithin(g.top, real) {
			// Not there, nothing to read; or in the project.
			continue
		}
		out = append(out, dir)
		g.held.Skills = appendNew(g.held.Skills, g.held.name(dir)+" (it leads to "+ShowPath(real)+")")
	}
	return out
}

// heldSettings is what the gate held back, or nil when it held nothing.
func (g *projectGate) heldSettings() *ProjectSettings {
	if g == nil || g.trusted || !g.held.Holds() {
		return nil
	}
	return g.held
}

// canonicalPath is path with its parent directory's symlinks resolved, for
// a file that may not exist.
func canonicalPath(path string) string {
	return filepath.Join(canonicalDir(filepath.Dir(path)), filepath.Base(path))
}

// isWithin reports whether path is dir or under it.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// ScanProject reads what the project settings of a gnosh started in
// workingDir, with the data directory dataDir ("" for the project's own),
// would do, running none of them, and whether its folder is trusted. The
// TUI asks its question from it before the config loads.
func ScanProject(workingDir, dataDir string) (*ProjectSettings, bool, error) {
	trust := decideTrust(workingDir, false)
	g := newProjectGate(workingDir, ProjectTrust{Folder: trust.Folder, InRepo: trust.InRepo})
	if g.held == nil {
		return &ProjectSettings{}, trust.Trusted, nil
	}
	if err := checkProjectConfigOwners(workingDir); err != nil {
		return nil, false, err
	}
	// In the order a load reads them (see lookupConfigs), so the two list
	// the same files the same way.
	paths := projectConfigPaths(workingDir)
	slices.Reverse(paths)
	for _, path := range paths {
		if err := g.scan(path, isShellConfig(path)); err != nil {
			return nil, false, err
		}
	}
	if dataDir == "" {
		dataDir = projectDataDir(workingDir)
	}
	dataDir = filepath.Clean(filepathext.SmartJoin(workingDir, dataDir))
	if workspace := configFileIn(dataDir); g.holdsWorkspace(workspace) {
		if err := g.scan(workspace, false); err != nil {
			return nil, false, err
		}
	}
	g.skillDirsOut(workingDir)
	return g.held, trust.Trusted, nil
}

// scan notes what the config file at path would do, as a load of an
// untrusted folder would.
func (g *projectGate) scan(path string, shell bool) error {
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to open config file %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	if shell {
		g.held.addScript(path)
		return nil
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid JSON in config file %s", path)
	}
	_, err = g.held.split(path, data)
	return err
}

// projectConfigPaths are the project's own config files, found as
// lookupConfigs finds them, less any that is also a global config.
func projectConfigPaths(cwd string) []string {
	found, err := fsext.LookupBounded(cwd, projectBoundary(cwd), projectConfigNames()...)
	if err != nil {
		return nil
	}
	global := globalConfigPaths()
	var paths []string
	for _, path := range found {
		if !slices.ContainsFunc(global, func(g string) bool { return sameFile(g, path) }) {
			paths = append(paths, path)
		}
	}
	return paths
}

// sameFile reports whether a and b name the same file.
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}
