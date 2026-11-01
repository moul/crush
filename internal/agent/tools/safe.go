package tools

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/shell"
	"mvdan.cc/sh/v3/syntax"
)

// readOnlyCommand reports whether command only reads, so the bash tool
// may run it without asking permission, as Claude Code runs its read-only
// commands: the shell parses all of it into commands each of which is one
// of readOnlyChecks, joined only by &&, ||, ; , newlines or pipes, with
// no substitution, assignment or background job, no expansion but a plain
// $NAME, and redirections only from a file, from a here-document with no
// substitution, or to /dev/null. Anything else, or anything it cannot parse, asks. It
// used to take any command that started with a safe name and held no
// ";", "|", "&&", "$(" or backquote, so "echo hi" and a second line, "ls &
// rm -rf ~", "echo x > ~/.zshrc" and "timeout 5 rm -rf ~" all ran
// unasked.
func readOnlyCommand(command string, dirs workDirs) bool {
	return eachCallIn(command, dirs, readOnlyCall) && readsProject(command, dirs, nil)
}

// workDirs are where a command line starts and the project it may cd
// around in (see eachCallIn); with none, a cd asks.
type workDirs struct {
	start, project string
}

// eachCallIn is eachCall that takes cd as Claude Code's read-only set does:
// a cd, written in plain text, into the project or a directory in it is
// let through, and any other cd asks. A line that cds into another
// directory and runs git asks too: git in a repository other than the one
// it was in can run a command that repository's config names
// (core.fsmonitor, say). Each cd is resolved from where the line starts.
func eachCallIn(command string, dirs workDirs, callOK func(*syntax.CallExpr) bool) bool {
	moved, git := false, false
	ok := eachCall(command, func(call *syntax.CallExpr) bool {
		words := plainWords(call)
		if len(words) > 0 && words[0] == "cd" {
			into, ok := cdInto(call, dirs)
			moved = moved || (ok && into != realPath(dirs.start))
			return ok
		}
		if form := shell.AllowForm(words); len(form) > 0 && form[0] == "git" {
			git = true
		}
		return callOK(call)
	})
	return ok && !(moved && git)
}

// plainWords is call's words when each is plain text with nothing to
// expand, else as many as come before the first that is not.
func plainWords(call *syntax.CallExpr) []string {
	var words []string
	for _, word := range call.Args {
		text, uncertain, ok := plainWord(word)
		if !ok || uncertain {
			break
		}
		words = append(words, text)
	}
	return words
}

// cdInto is the directory a cd call moves into, as a real path, when it
// is one directory written in plain text (no option, no ~, nothing to
// expand) that exists in the project; ok is false for any other cd.
func cdInto(call *syntax.CallExpr, dirs workDirs) (string, bool) {
	words := plainWords(call)
	if dirs.start == "" || dirs.project == "" || len(call.Assigns) > 0 || len(call.Args) != 2 || len(words) != 2 {
		return "", false
	}
	target := words[1]
	if strings.HasPrefix(target, "-") || strings.HasPrefix(target, "~") {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dirs.start, target)
	}
	into, project := realPath(target), realPath(dirs.project)
	if into == "" || project == "" {
		return "", false
	}
	rel, err := filepath.Rel(project, into)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return into, true
}

// realPath is path with its links resolved, "" when it does not exist.
func realPath(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return real
}

// eachCall reports whether the shell parses all of command into commands
// each of which callOK takes, joined only by &&, ||, ; , newlines or
// pipes, with no background job and redirections only as
// harmlessRedirect allows. As Claude Code, a command too long to analyze
// or one that cannot be parsed is asked about.
func eachCall(command string, callOK func(*syntax.CallExpr) bool) bool {
	if len(command) > 10000 {
		return false
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) == 0 {
		return false
	}
	for _, stmt := range file.Stmts {
		if !stmtCalls(stmt, callOK) {
			return false
		}
	}
	return true
}

// stmtCalls reports whether stmt is commands callOK takes: see eachCall.
func stmtCalls(stmt *syntax.Stmt, callOK func(*syntax.CallExpr) bool) bool {
	if stmt.Background || stmt.Coprocess {
		return false
	}
	for _, redir := range stmt.Redirs {
		if !harmlessRedirect(redir) {
			return false
		}
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		return callOK(cmd)
	case *syntax.BinaryCmd:
		// &&, ||, | and |&: each side on its own.
		return stmtCalls(cmd.X, callOK) && stmtCalls(cmd.Y, callOK)
	default:
		return false
	}
}

// harmlessRedirect reports whether redir only reads a file or a
// here-document, or writes to /dev/null or another descriptor.
func harmlessRedirect(redir *syntax.Redirect) bool {
	switch redir.Op {
	case syntax.RdrIn:
		_, _, ok := plainWord(redir.Word)
		return ok
	case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob:
		target, _, ok := plainWord(redir.Word)
		return ok && target == "/dev/null"
	case syntax.DplOut, syntax.DplIn:
		// 2>&1, >&2, 2>&-: another descriptor, not a file.
		target, _, ok := plainWord(redir.Word)
		return ok && (target == "-" || strings.Trim(target, "0123456789") == "")
	case syntax.Hdoc, syntax.DashHdoc:
		// A here-document's body may hold a command substitution,
		// which would run.
		return redir.Hdoc == nil || harmlessParts(redir.Hdoc.Parts)
	case syntax.WordHdoc:
		_, _, ok := plainWord(redir.Word)
		return ok
	}
	return false
}

// harmlessParts reports whether parts are text and plain $NAME
// expansions only, as a here-document's body or a double-quoted word
// that runs nothing.
func harmlessParts(parts []syntax.WordPart) bool {
	for _, part := range parts {
		switch part := part.(type) {
		case *syntax.Lit:
		case *syntax.ParamExp:
			if !plainParam(part) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// plainParam reports whether p is a plain $NAME or ${NAME}, which only
// puts a variable's value in, with no operator that could assign, run a
// substitution or reach another variable.
func plainParam(p *syntax.ParamExp) bool {
	return p.Param != nil && p.Flags == nil && !p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.Split == syntax.OptUnset && p.GlobSubst == syntax.OptUnset && p.RcExpand == syntax.OptUnset &&
		p.NestedParam == nil && p.Index == nil && len(p.Modifiers) == 0 &&
		p.Slice == nil && p.Repl == nil && p.Names == 0 && p.Exp == nil
}

// plainWord is word's text, if it runs nothing: plain text, single-quoted
// text, double-quoted text, and plain $NAME expansions. uncertain says
// whether the text the command gets can differ from it: an unquoted glob
// or brace expansion, which could expand to an option, or an expansion,
// whose value it does not know.
func plainWord(word *syntax.Word) (text string, uncertain, ok bool) {
	if word == nil {
		return "", false, false
	}
	var b strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			// A brace expansion makes words of its own: {a,--output=x}
			// is two arguments, the second an option.
			uncertain = uncertain || strings.ContainsAny(part.Value, "*?[{")
			b.WriteString(part.Value)
		case *syntax.SglQuoted:
			if part.Dollar {
				return "", false, false
			}
			b.WriteString(part.Value)
		case *syntax.DblQuoted:
			if !harmlessParts(part.Parts) {
				return "", false, false
			}
			for _, inner := range part.Parts {
				if lit, isLit := inner.(*syntax.Lit); isLit {
					b.WriteString(lit.Value)
				} else {
					uncertain = true
				}
			}
		case *syntax.ParamExp:
			if !plainParam(part) {
				return "", false, false
			}
			uncertain = true
		default:
			return "", false, false
		}
	}
	return b.String(), uncertain, true
}

// readOnlyCall reports whether call runs one of readOnlyChecks with
// arguments that keep it reading only.
func readOnlyCall(call *syntax.CallExpr) bool {
	if len(call.Assigns) > 0 || len(call.Args) == 0 {
		// An assignment such as GIT_EXTERNAL_DIFF=... can make a
		// reading command run a program.
		return false
	}
	args := make([]string, len(call.Args))
	uncertain := false
	for i, word := range call.Args {
		text, unsure, ok := plainWord(word)
		if !ok || (i == 0 && unsure) {
			// The command's own name must be known.
			return false
		}
		args[i], uncertain = text, uncertain || unsure
	}
	check, ok := readOnlyChecks[args[0]]
	if !ok {
		return false
	}
	return check(args[1:], uncertain)
}

// readOnlyCheck reports whether a command, given args, only reads.
// uncertain says whether an argument's text may differ from what the
// command gets (a glob, an expansion): a check that reads the arguments
// asks then.
type readOnlyCheck func(args []string, uncertain bool) bool

// anyArgs is the check of a command no argument makes write or run
// anything.
func anyArgs([]string, bool) bool { return true }

// noArgs is the check of a command that reads only when it is given none.
func noArgs(args []string, _ bool) bool { return len(args) == 0 }

// readOnlyChecks are the commands the bash tool runs without asking, as
// Claude Code runs its read-only set, each with the check of its
// arguments. Wrappers that run another command (timeout, nice, nohup,
// time, env with arguments), commands that change something (kill,
// killall) and forms of git that write are not in it.
var readOnlyChecks = map[string]readOnlyCheck{
	"cal":      anyArgs,
	"cat":      anyArgs,
	"df":       anyArgs,
	"diff":     anyArgs,
	"du":       anyArgs,
	"echo":     anyArgs,
	"free":     anyArgs,
	"grep":     anyArgs,
	"groups":   anyArgs,
	"head":     anyArgs,
	"id":       anyArgs,
	"ls":       anyArgs,
	"printenv": anyArgs,
	"pwd":      anyArgs,
	"stat":     anyArgs,
	"tail":     anyArgs,
	"type":     anyArgs,
	"uname":    anyArgs,
	"uptime":   anyArgs,
	"wc":       anyArgs,
	"whatis":   anyArgs,
	"whereis":  anyArgs,
	"which":    anyArgs,
	"whoami":   anyArgs,
	"env":      noArgs,
	"date": func(args []string, uncertain bool) bool {
		// date -s and --set change the clock.
		return !uncertain && !slices.ContainsFunc(args, func(arg string) bool {
			return strings.HasPrefix(arg, "--set") || (strings.HasPrefix(arg, "-s") && !strings.HasPrefix(arg, "--"))
		})
	},
	"hostname": func(args []string, uncertain bool) bool {
		// A name, or -F and --file with a file of one, sets the hostname;
		// these options only show it.
		return !uncertain && !slices.ContainsFunc(args, func(arg string) bool {
			return !slices.Contains([]string{"-f", "-s", "-i", "-I", "-d", "-a", "-A", "--fqdn", "--short", "--domain", "--ip-address", "--all-ip-addresses", "--alias"}, arg)
		})
	},
	"ps": func(args []string, uncertain bool) bool {
		// ps e (as in "ps auxe") prints every process's environment.
		if uncertain {
			return false
		}
		switch strings.Join(args, " ") {
		case "", "aux", "ax", "-ef", "-e", "-A", "-f":
			return true
		}
		return false
	},
	"git": readOnlyGit,
}

// readOnlyGitCommands are git's subcommands that read only, each with the
// check of its arguments.
var readOnlyGitCommands = map[string]readOnlyCheck{
	"blame":     anyArgs,
	"describe":  anyArgs,
	"diff":      anyArgs,
	"grep":      anyArgs,
	"log":       anyArgs,
	"ls-files":  anyArgs,
	"ls-remote": anyArgs,
	"rev-parse": anyArgs,
	"shortlog":  anyArgs,
	"show":      anyArgs,
	"status":    anyArgs,
	// A name creates a branch or a tag, unless -l lists by pattern.
	"branch": gitListing("-a", "-r", "-v", "-vv", "--all", "--remotes", "--verbose", "--show-current", "--color", "--no-color"),
	"tag":    gitListing("-n", "--color", "--no-color"),
	"remote": func(args []string, _ bool) bool {
		switch strings.Join(args, " ") {
		case "", "-v", "--verbose":
			return true
		}
		return len(args) == 2 && args[0] == "get-url"
	},
	"config": func(args []string, _ bool) bool {
		if len(args) == 0 || !slices.Contains([]string{"--get", "--get-all", "--get-regexp", "--list", "-l"}, args[0]) {
			return false
		}
		return !slices.ContainsFunc(args, func(arg string) bool {
			return slices.Contains([]string{"--add", "--unset", "--unset-all", "--replace-all", "--rename-section", "--remove-section", "--edit", "-e"}, arg)
		})
	},
}

// readOnlyGit is git's check: a reading subcommand right after "git",
// with no global option before it (git -c sets a program to run), no
// glob or expansion (either could become an option), and no option that
// writes a file or runs a program.
func readOnlyGit(args []string, uncertain bool) bool {
	if len(args) == 0 || uncertain {
		return false
	}
	check, ok := readOnlyGitCommands[args[0]]
	if !ok {
		return false
	}
	for _, arg := range args[1:] {
		for _, writes := range []string{"--output", "--ext-diff", "--open-files-in-pager", "-O"} {
			if strings.HasPrefix(arg, writes) {
				return false
			}
		}
	}
	return check(args[1:], uncertain)
}

// gitListing is the check of git branch and git tag: they only list when
// given no name, or names after -l or --list, which are patterns; the
// options they may carry besides are flags.
func gitListing(flags ...string) readOnlyCheck {
	return func(args []string, _ bool) bool {
		listing := slices.Contains(args, "-l") || slices.Contains(args, "--list")
		for _, arg := range args {
			switch {
			case arg == "-l" || arg == "--list" || slices.Contains(flags, arg):
			case strings.HasPrefix(arg, "--sort=") || strings.HasPrefix(arg, "--format=") || strings.HasPrefix(arg, "-n"):
			case !strings.HasPrefix(arg, "-") && listing:
			default:
				return false
			}
		}
		return true
	}
}

func init() {
	if runtime.GOOS == "windows" {
		for _, name := range []string{"ipconfig", "nslookup", "ping", "systeminfo", "tasklist", "where"} {
			readOnlyChecks[name] = anyArgs
		}
	}
}

// fileReaders are the read-only commands that read what their arguments
// name, or list or measure it, each with the options it is refused with:
// those that make it follow the links it meets on the way, since a link
// in the project can lead out of it (grep -R, ls -L, du -L, diff -r). A
// command not here reads no file its arguments name (echo, which, pwd).
var fileReaders = map[string]func(arg string) bool{
	"cat":  nil,
	"head": nil,
	"tail": nil,
	"wc":   nil,
	"stat": nil,
	"git":  nil,
	"grep": followsWith("RS", "--dereference-recursive"),
	"ls":   followsWith("L", "--dereference"),
	"du":   followsWith("L", "--dereference"),
	"diff": followsWith("r", "--recursive"),
}

// followsWith is a fileReaders refusal: whether arg is one of the long
// options, or a group of short ones that holds one of the letters.
func followsWith(letters string, long ...string) func(arg string) bool {
	return func(arg string) bool {
		if strings.HasPrefix(arg, "--") {
			name, _, _ := strings.Cut(arg, "=")
			return slices.Contains(long, name)
		}
		return strings.HasPrefix(arg, "-") && strings.ContainsAny(arg[1:], letters)
	}
}

// readsInside reports whether call, a read-only command, reads only what
// lies in the project, as Claude Code asks before its read-only commands
// read outside the working directory. Each argument of a command in
// fileReaders is taken as a path (an option's value too: --file=x,
// -f/etc/x), since which of them name files differs from command to
// command, and must lie in project, from each of cwds, written and with
// its links resolved; a word that names nothing yet lies where it would
// be. A glob is expanded to check what it names. A word whose text is not
// known ($NAME, a brace expansion) asks, as does a refused option.
func readsInside(call *syntax.CallExpr, cwds []string, project string) bool {
	if len(call.Args) == 0 {
		return true
	}
	name, _, ok := plainWord(call.Args[0])
	refused, reads := fileReaders[name]
	if !ok || !reads {
		return true
	}
	operands := false // past --, every argument is a path
	for _, word := range call.Args[1:] {
		text, glob, known := wordGlob(word)
		if !known {
			return false
		}
		if !operands && !glob && text == "--" {
			operands = true
			continue
		}
		path := text
		if !operands && strings.HasPrefix(text, "-") && text != "-" {
			if refused != nil && refused(text) {
				return false
			}
			// The shell globs the whole word, which names no file, so
			// the command gets the value as written: --include=*.go.
			path, glob = optionPath(text), false
		}
		if path == "" {
			continue
		}
		if project == "" {
			return false
		}
		for _, cwd := range cwds {
			if !pathsInside(path, glob, cwd, project) {
				return false
			}
		}
	}
	return true
}

// optionPath is the path an option could name: its value after =
// (--file=x), or one written onto a short option (-f/etc/x, -f../x); ""
// when it holds none.
func optionPath(option string) string {
	if _, value, ok := strings.Cut(option, "="); ok {
		return value
	}
	if strings.HasPrefix(option, "--") {
		return ""
	}
	if i := strings.IndexAny(option[1:], "./~"); i >= 0 {
		return option[1+i:]
	}
	return ""
}

// pathsInside reports whether path, read from cwd, lies in project (see
// pathInside); a glob, each path it names, or itself when it names none,
// as the shell then passes it as it is.
func pathsInside(path string, glob bool, cwd, project string) bool {
	if !glob {
		return pathInside(path, cwd, project)
	}
	pattern, ok := fromCwd(path, cwd)
	if !ok {
		return false
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false
	}
	if len(matches) == 0 {
		return pathInside(path, cwd, project)
	}
	for _, match := range matches {
		if !pathInside(match, cwd, project) {
			return false
		}
	}
	return true
}

// pathInside reports whether path, read from cwd, lies in project: as
// written, and with its links resolved (fsext.LeadsOut), against the
// project as named or as its real path (on macOS /var is a link to
// /private/var, and a cd moves into a real path). "-" is standard input.
func pathInside(path, cwd, project string) bool {
	if path == "-" {
		return true
	}
	abs, ok := fromCwd(path, cwd)
	if !ok {
		return false
	}
	if outside, _ := fsext.LeadsOut(project, abs); !outside {
		return true
	}
	real := realPath(project)
	if real == "" || real == project {
		return false
	}
	outside, _ := fsext.LeadsOut(real, abs)
	return !outside
}

// fromCwd is path as the shell takes it in cwd: ~ is the home directory;
// ok is false for ~user, another's home, which it does not look up.
func fromCwd(path, cwd string) (string, bool) {
	switch {
	case path == "~" || strings.HasPrefix(path, "~/"):
		return filepath.Join(home.Dir(), path[1:]), true
	case strings.HasPrefix(path, "~"):
		return "", false
	case filepath.IsAbs(path):
		return path, true
	}
	return filepath.Join(cwd, path), true
}

// wordGlob is word's text for readsInside: glob is set when it holds an
// unquoted glob, and known is false when its text cannot be known before
// the shell expands it ($NAME, a brace expansion) or it is anything but
// text.
func wordGlob(word *syntax.Word) (text string, glob, known bool) {
	var b strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			if strings.Contains(part.Value, "{") {
				return "", false, false
			}
			glob = glob || strings.ContainsAny(part.Value, "*?[")
			b.WriteString(part.Value)
		case *syntax.SglQuoted:
			if part.Dollar {
				return "", false, false
			}
			b.WriteString(part.Value)
		case *syntax.DblQuoted:
			for _, inner := range part.Parts {
				lit, isLit := inner.(*syntax.Lit)
				if !isLit {
					return "", false, false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false, false
		}
	}
	return b.String(), glob, true
}

// readsProject reports whether the commands of command's line that read
// files read only what lies in the project (readsInside), and its <
// inputs too, each from the directories the line may be in when it runs
// it: a cd moves the rest of an && chain, and may or may not have moved
// what follows it after ; or ||. allowed, when set, names the calls an
// allow rule of the user's lets run as they are, which are not checked.
func readsProject(command string, dirs workDirs, allowed func(*syntax.CallExpr) bool) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	cwds := []string{dirs.start}
	for _, stmt := range file.Stmts {
		_, after, ok := stmtReads(stmt, cwds, dirs, allowed)
		if !ok {
			return false
		}
		cwds = after
	}
	return true
}

// stmtReads checks the reads of stmt run from any of cwds (see
// readsProject), and returns the directories the line may be in after it:
// ok when it succeeded, and any whatever happened.
func stmtReads(stmt *syntax.Stmt, cwds []string, dirs workDirs, allowed func(*syntax.CallExpr) bool) (ok, any []string, fine bool) {
	for _, redir := range stmt.Redirs {
		if redir.Op == syntax.RdrIn && !inputInside(redir.Word, cwds, dirs.project) {
			return nil, nil, false
		}
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		if words := plainWords(cmd); len(words) > 0 && words[0] == "cd" {
			into, moved := cdInto(cmd, dirs)
			if !moved {
				return nil, nil, false
			}
			ok, any = []string{into}, union(cwds, []string{into})
		} else {
			if (allowed == nil || !allowed(cmd)) && !readsInside(cmd, cwds, dirs.project) {
				return nil, nil, false
			}
			ok, any = cwds, cwds
		}
	case *syntax.BinaryCmd:
		okX, anyX, fine := stmtReads(cmd.X, cwds, dirs, allowed)
		if !fine {
			return nil, nil, false
		}
		var okY, anyY []string
		switch cmd.Op {
		case syntax.AndStmt:
			okY, anyY, fine = stmtReads(cmd.Y, okX, dirs, allowed)
			ok = okY
		case syntax.OrStmt:
			okY, anyY, fine = stmtReads(cmd.Y, anyX, dirs, allowed)
			ok = union(okX, okY)
		default:
			// Each side of a pipe runs in a shell of its own, where a cd
			// moves nothing after it.
			_, _, fine = stmtReads(cmd.Y, cwds, dirs, allowed)
			okX, anyX, okY, anyY = cwds, cwds, cwds, cwds
			ok = cwds
		}
		if !fine {
			return nil, nil, false
		}
		any = union(anyX, anyY)
	default:
		return nil, nil, false
	}
	if stmt.Negated {
		// "! cd sub && cat ../x" runs cat when the cd failed.
		ok = any
	}
	return ok, any, true
}

// inputInside reports whether word, the file a < reads, lies in the
// project from each of cwds: "cat < ~/.ssh/id_ed25519" reads as much as
// cat would.
func inputInside(word *syntax.Word, cwds []string, project string) bool {
	text, glob, known := wordGlob(word)
	if !known || glob || project == "" {
		return false
	}
	for _, cwd := range cwds {
		if !pathInside(text, cwd, project) {
			return false
		}
	}
	return true
}

// union is a with the directories of b it does not hold.
func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, dir := range b {
		if !slices.Contains(out, dir) {
			out = append(out, dir)
		}
	}
	return out
}
