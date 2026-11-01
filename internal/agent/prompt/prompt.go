package prompt

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/gitutil"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/skills"
)

// Prompt represents a template-based prompt generator.
type Prompt struct {
	name       string
	template   string
	now        func() time.Time
	platform   string
	workingDir string
	before     string
	after      string

	interactive bool
}

type PromptDat struct {
	Provider           string
	Model              string
	Config             config.Config
	WorkingDir         string
	IsGitRepo          bool
	Platform           string
	Machine            string
	Date               string
	GitStatus          string
	ContextFiles       []ContextFile
	GlobalContextFiles []ContextFile
	AvailSkillXML      string
	// Skills are the skills found; AvailSkillXML lists those the model
	// may invoke.
	Skills []*skills.Skill
	// AddedDirs are the directories added with --add-dir.
	AddedDirs []string
	// Interactive is whether a user is at a terminal, and so whether the
	// tools only a TUI can answer — question, artifact — are in the
	// toolset. A prompt must not ask for a tool the agent was not given.
	Interactive bool
}

// ContextFile is a context file and what it was loaded as: its path and
// the model's text of it, and, for a file a context file imported with
// @path, the parent that imported it, which the InstructionsLoaded hook
// reports.
type ContextFile struct {
	Path    string
	Content string
	// Parent is the path of the context file that imported this one, set
	// for an @path import only.
	Parent string
}

type Option func(*Prompt)

func WithTimeFunc(fn func() time.Time) Option {
	return func(p *Prompt) {
		p.now = fn
	}
}

func WithPlatform(platform string) Option {
	return func(p *Prompt) {
		p.platform = platform
	}
}

func WithWorkingDir(workingDir string) Option {
	return func(p *Prompt) {
		p.workingDir = workingDir
	}
}

// WithTextBefore puts text, as written, before what the template builds;
// it is not read as a template.
// WithInteractive tells the prompt that a user is at a terminal, so it
// may ask for the tools only a TUI can answer (question, artifact).
func WithInteractive(interactive bool) Option {
	return func(p *Prompt) {
		p.interactive = interactive
	}
}

func WithTextBefore(text string) Option {
	return func(p *Prompt) {
		p.before = text
	}
}

// WithTextAfter puts text, as written, after what the template builds,
// separated by a blank line; it is not read as a template.
func WithTextAfter(text string) Option {
	return func(p *Prompt) {
		p.after = text
	}
}

func NewPrompt(name, promptTemplate string, opts ...Option) (*Prompt, error) {
	p := &Prompt{
		name:     name,
		template: promptTemplate,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Prompt) Build(ctx context.Context, provider, model string, store *config.ConfigStore) (string, error) {
	t, err := template.New(p.name).Parse(p.template)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var sb strings.Builder
	d, err := p.promptData(ctx, provider, model, store)
	if err != nil {
		return "", err
	}
	sb.WriteString(p.before)
	if err := t.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	if p.after != "" {
		sb.WriteString("\n\n")
		sb.WriteString(p.after)
	}

	return sb.String(), nil
}

// maxContextFileSize is the largest context file that is read: as Claude
// Code skips a CLAUDE.md over 4 MiB and loads none of it.
const maxContextFileSize = 4 << 20

// processFile reads a context file as the model gets it: without its
// block-level HTML comments, and not at all when it is over
// maxContextFileSize.
func processFile(filePath string) *ContextFile {
	info, err := os.Stat(filePath)
	if err != nil {
		return nil
	}
	if info.Size() > maxContextFileSize {
		slog.Warn("A context file is over 4 MiB; it is not read", "file", filePath, "size", info.Size())
		return nil
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	return &ContextFile{
		Path:    filePath,
		Content: stripHTMLComments(string(content)),
	}
}

// stripHTMLComments removes a context file's block-level HTML comments,
// as Claude Code does before the model reads the file: notes left for
// the people who keep it, which cost the model tokens and would import
// what they name. A comment block starts a line, after at most three
// spaces, with <!-- and runs to the line with -->, which goes too, as a
// Markdown HTML block does. A comment in a fenced code block, or within
// a line of text, stays.
func stripHTMLComments(content string) string {
	if !strings.Contains(content, "<!--") {
		return content
	}
	lines := strings.Split(content, "\n")
	kept := lines[:0]
	fenced, inComment := false, false
	for _, line := range lines {
		if inComment {
			inComment = !strings.Contains(line, "-->")
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
		} else if indent := len(line) - len(strings.TrimLeft(line, " ")); !fenced && indent <= 3 && strings.HasPrefix(line[indent:], "<!--") {
			// From after "<!", so that "<!-->", an empty comment, ends here.
			inComment = !strings.Contains(line[indent+len("<!"):], "-->")
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func processContextPath(p string, store *config.ConfigStore) []ContextFile {
	var contexts []ContextFile
	fullPath := filepathext.SmartJoin(store.WorkingDir(), p)
	info, err := os.Stat(fullPath)
	if err != nil {
		return contexts
	}
	if info.IsDir() {
		filepath.WalkDir(fullPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				if result := processFile(path); result != nil {
					contexts = append(contexts, *result)
				}
			}
			return nil
		})
	} else {
		result := processFile(fullPath)
		if result != nil {
			contexts = append(contexts, *result)
		}
	}
	return contexts
}

// expandPath expands ~ and environment variables in file paths
func expandPath(path string, store *config.ConfigStore) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

// LoadContextFiles loads the context files at paths, in the order the
// paths are given, each file once. A path listed again in another case
// (GNOSH.md, gnosh.md) is skipped, and so is a file already loaded under
// another name: a symlink such as CLAUDE.md -> AGENTS.md, or CRUSH.md ->
// GNOSH.md after the rename, would otherwise put the same text in the
// prompt twice. The order is fixed because the system prompt is rebuilt
// (on a model switch, a config reload), and a model server's prompt
// cache reuses only the part of a conversation before the first change:
// files reshuffled by map order made the whole conversation be read
// again.
//
// The files a context file imports, with Claude Code's @path, come just
// before it, as Claude Code reads them first (see contextImports). They
// must stay inside importRoot, unless it is "": a project's own context
// files may import only the project's files, or a repository could have
// its AGENTS.md put ~/.ssh/id_ed25519 in the prompt, and so in front of
// the model's provider. The user's global context files import anything.
func LoadContextFiles(paths []string, store *config.ConfigStore, importRoot string) []ContextFile {
	l := newContextLoader(store)
	l.load(paths, importRoot, nil)
	return l.files
}

// ProjectContextFiles loads the project's context files at paths: those
// in each directory above the working directory, the outermost first,
// then the working directory's own, as Claude Code reads CLAUDE.md files,
// so the instructions closest to where gnosh started come last. Started
// in a subdirectory of a repository, gnosh read none of the
// repository's. A directory above gives only files the user or root
// owns, so a shared one such as /tmp cannot slip instructions into a
// session, and their imports stay inside that directory.
func ProjectContextFiles(paths []string, store *config.ConfigStore) []ContextFile {
	l := newContextLoader(store)
	workingDir := store.WorkingDir()
	if workingDir != "" {
		relative := relativeContextPaths(paths, store)
		var above []string
		for dir := filepath.Dir(workingDir); ; dir = filepath.Dir(dir) {
			above = append(above, dir)
			if dir == filepath.Dir(dir) {
				break
			}
		}
		for i := len(above) - 1; i >= 0; i-- {
			l.load(pathsIn(above[i], relative), above[i], ownedByUserOrRoot)
		}
	}
	l.load(paths, cmp.Or(workingDir, "."), nil)
	return l.files
}

// SubdirectoryContextFiles returns the context files at paths in the
// directories from the working directory down to file's, not counting
// the working directory, whose files the system prompt has: the
// outermost first, each file's imports staying inside its directory.
// They are what Claude Code loads once it reads a file in a subdirectory
// with a CLAUDE.md of its own.
func SubdirectoryContextFiles(paths []string, store *config.ConfigStore, file string) []ContextFile {
	workingDir := store.WorkingDir()
	if workingDir == "" {
		return nil
	}
	rel, err := filepath.Rel(workingDir, filepath.Dir(file))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	relative := relativeContextPaths(paths, store)
	l := newContextLoader(store)
	dir := workingDir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		l.load(pathsIn(dir, relative), dir, nil)
	}
	return l.files
}

// relativeContextPaths returns those of paths that name a file relative
// to a directory, as CLAUDE.md does, expanded.
func relativeContextPaths(paths []string, store *config.ConfigStore) []string {
	var relative []string
	for _, p := range paths {
		if expanded := expandPath(p, store); !filepathext.SmartIsAbs(expanded) {
			relative = append(relative, expanded)
		}
	}
	return relative
}

// pathsIn returns paths, each relative, joined to dir.
func pathsIn(dir string, paths []string) []string {
	joined := make([]string, len(paths))
	for i, p := range paths {
		joined[i] = filepath.Join(dir, p)
	}
	return joined
}

// ownedByUserOrRoot reports whether the file at path belongs to the user
// gnosh runs as, or to root.
func ownedByUserOrRoot(path string) bool {
	uid, err := fsext.Owner(path)
	return err == nil && (uid == os.Getuid() || uid == 0)
}

// contextLoader loads context files for the prompt, each once however
// it is reached (see LoadContextFiles).
type contextLoader struct {
	store     *config.ConfigStore
	files     []ContextFile
	seenPaths map[string]bool
	seenFiles map[string]bool
}

func newContextLoader(store *config.ConfigStore) *contextLoader {
	return &contextLoader{store: store, seenPaths: map[string]bool{}, seenFiles: map[string]bool{}}
}

// load adds the context files at paths and the files they import, which
// must stay inside importRoot unless it is "". When trusted is not nil,
// a file it refuses is not read. An imported file records the file that
// imported it, in Parent.
func (l *contextLoader) load(paths []string, importRoot string, trusted func(path string) bool) {
	// add loads the files file imports, then file, each once, depth hops
	// from a configured path.
	var add func(file ContextFile, depth int)
	add = func(file ContextFile, depth int) {
		real, err := filepath.EvalSymlinks(file.Path)
		if err != nil {
			real = file.Path
		}
		if l.seenFiles[real] || (trusted != nil && !trusted(file.Path)) {
			return
		}
		l.seenFiles[real] = true
		defer func() { l.files = append(l.files, file) }()
		if depth == maxImportDepth {
			return
		}
		for _, ref := range contextImports(file.Content) {
			target := home.Long(ref)
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(file.Path), target)
			}
			if info, err := os.Stat(target); err != nil || !info.Mode().IsRegular() {
				// Not a file: an @mention, a handle, a missing file.
				continue
			}
			if importRoot != "" && !within(target, importRoot) {
				slog.Warn("A context file imports a file outside the project; it is not read",
					"file", file.Path, "import", ref)
				continue
			}
			if imported := processFile(target); imported != nil {
				imported.Parent = file.Path
				add(*imported, depth+1)
			}
		}
	}
	for _, pth := range paths {
		expanded := expandPath(pth, l.store)
		pathKey := strings.ToLower(expanded)
		if l.seenPaths[pathKey] {
			continue
		}
		l.seenPaths[pathKey] = true
		for _, file := range processContextPath(expanded, l.store) {
			add(file, 0)
		}
	}
}

// maxImportDepth is how many hops of imports are followed from a
// configured context file, as in Claude Code.
const maxImportDepth = 4

// importRef is an @path import: an @ at the start of a line or after a
// space, then the path, which ends at the first space not escaped with
// a backslash.
var importRef = regexp.MustCompile(`(?:^|\s)@((?:\\ |\S)+)`)

// contextImports returns the paths a context file imports with Claude
// Code's @path syntax, in order: "@AGENTS.md", "see @docs/style.md", a
// space in a path escaped with a backslash. As in Claude Code, nothing in
// a code span or a fenced code block imports.
func contextImports(content string) []string {
	var refs []string
	fenced := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		// Every other piece between backticks is a code span.
		pieces := strings.Split(line, "`")
		for i := 0; i < len(pieces); i += 2 {
			for _, m := range importRef.FindAllStringSubmatch(pieces[i], -1) {
				refs = append(refs, strings.ReplaceAll(m[1], `\ `, " "))
			}
		}
	}
	return refs
}

// within reports whether path, its symlinks resolved, is inside dir.
func within(path, dir string) bool {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Data is what Build fills the template with: the context files, the
// skills and the rest, for a report of what the prompt holds.
func (p *Prompt) Data(ctx context.Context, provider, model string, store *config.ConfigStore) (PromptDat, error) {
	return p.promptData(ctx, provider, model, store)
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store *config.ConfigStore) (PromptDat, error) {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)

	cfg := store.Config()
	contextFiles := ProjectContextFiles(cfg.Options.ContextPaths, store)
	globalContextFiles := LoadContextFiles(cfg.Options.GlobalContextPaths, store, "")

	// Discover and load skills metadata.
	var availSkillXML string

	// Start with builtin skills.
	allSkills := skills.DiscoverBuiltin()
	builtinNames := make(map[string]bool, len(allSkills))
	for _, s := range allSkills {
		builtinNames[s.Name] = true
	}

	// Discover user skills from configured paths.
	if len(cfg.Options.SkillsPaths) > 0 {
		expandedPaths := make([]string, 0, len(cfg.Options.SkillsPaths))
		for _, pth := range cfg.Options.SkillsPaths {
			expandedPaths = append(expandedPaths, expandPath(pth, store))
		}
		for _, userSkill := range skills.Discover(expandedPaths) {
			if builtinNames[userSkill.Name] {
				slog.Warn("User skill overrides builtin skill", "name", userSkill.Name)
			}
			allSkills = append(allSkills, userSkill)
		}
	}

	// Deduplicate: user skills override builtins with the same name.
	allSkills = skills.Deduplicate(allSkills)

	// Filter out disabled skills.
	allSkills = skills.Filter(allSkills, cfg.Options.DisabledSkills)

	if len(allSkills) > 0 {
		availSkillXML = skills.ToPromptXML(allSkills)
	}

	isGit := isGitRepo(store.WorkingDir())
	data := PromptDat{
		Provider:   provider,
		Model:      model,
		Config:     *cfg,
		WorkingDir: filepath.ToSlash(workingDir),
		IsGitRepo:  isGit,
		Platform:   platform,
		Machine:    config.LocalMachine(),
		// ISO, as Claude Code shows it: the model copies it into mem.Wait.
		Date: p.now().Format(time.DateOnly),

		AvailSkillXML: availSkillXML,
		Skills:        allSkills,
	}
	switch runs := cfg.HeldProject.GitRuns(); {
	case isGit && len(runs) > 0:
		// git status would run them: the folder's own git config names
		// programs, and the folder is not trusted.
		data.GitStatus = getGitBranch(store.WorkingDir()) + untrustedGitStatus(runs)
	case isGit:
		var err error
		data.GitStatus, err = getGitStatus(ctx, store.WorkingDir())
		if err != nil {
			return PromptDat{}, err
		}
	}

	data.ContextFiles = contextFiles
	data.GlobalContextFiles = globalContextFiles
	data.AddedDirs = store.AddedDirs()
	data.Interactive = p.interactive
	return data, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func getGitStatus(ctx context.Context, dir string) (string, error) {
	sh := shell.NewShell(&shell.Options{
		WorkingDir: dir,
	})
	branch := getGitBranch(dir)
	status, err := getGitStatusSummary(ctx, sh)
	if err != nil {
		return "", err
	}
	commits, err := getGitRecentCommits(ctx, sh)
	if err != nil {
		return "", err
	}
	return branch + status + commits, nil
}

// getGitBranch reads the checked-out branch straight from the repository.
// This runs on every prompt build, so it stays off the shell: spawning git
// each turn is wasted work, and a detail of prompt assembly has no business
// passing through command blocking and permission policy.
func getGitBranch(dir string) string {
	branch := gitutil.CurrentBranch(dir)
	if branch == "" {
		return ""
	}
	return fmt.Sprintf("Current branch: %s\n", branch)
}

// untrustedGitStatus is what the prompt says for the status of a
// repository whose own git config would have git run programs, runs, in a
// folder not trusted: not read, and why, so the agent knows its git
// commands will ask.
func untrustedGitStatus(runs []string) string {
	keys := make([]string, len(runs))
	for i, run := range runs {
		keys[i], _, _ = strings.Cut(run, "=")
	}
	return fmt.Sprintf("Status: not read. This folder is not trusted, and the repository's own git config would have git run programs (%s), so git commands ask the user first.\n",
		strings.Join(keys, ", "))
}

func getGitStatusSummary(ctx context.Context, sh *shell.Shell) (string, error) {
	// --no-optional-locks: status writes no index, which would run a
	// post-index-change hook.
	out, _, err := sh.Exec(ctx, "git --no-optional-locks status --short 2>/dev/null | head -20")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "Status: clean\n", nil
	}
	return fmt.Sprintf("Status:\n%s\n", out), nil
}

func getGitRecentCommits(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git --no-optional-locks log --oneline --no-show-signature -n 3 2>/dev/null")
	if err != nil || out == "" {
		return "", nil
	}
	out = strings.TrimSpace(out)
	return fmt.Sprintf("Recent commits:\n%s\n", out), nil
}

func (p *Prompt) Name() string {
	return p.name
}
