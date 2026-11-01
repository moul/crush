package gitutil

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Programs lists what the git repository dir is in would run of its own
// when git only reads it (git status, log, diff, show, blame, grep,
// ls-remote): the programs its own config names (the repository's
// .git/config, its worktree config and the files they include, not the
// user's), and a post-index-change hook, which git status runs when it
// writes the index. A repository someone hands you with its .git, as a
// download or a copy rather than a clone, can carry these, so that
// reading it runs their program as you: core.fsmonitor=./x.sh runs on
// every git status. Each is "key=value", or "hook post-index-change". It
// is nil outside a repository, and when git cannot be run or read.
//
// Reading the config runs nothing: git config only parses files.
func Programs(dir string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var found []string
	for _, entry := range ownConfig(ctx, dir) {
		if runsProgram(entry.key, entry.value) {
			found = append(found, entry.key+"="+entry.value)
		}
	}
	if hook := gitPath(ctx, dir, "hooks/post-index-change"); hook != "" {
		if info, err := os.Stat(hook); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			found = append(found, "hook post-index-change")
		}
	}
	return found
}

// configEntry is one setting of a git config.
type configEntry struct {
	key, value string
}

// ownConfig is the config of the repository dir is in, its local and
// worktree scopes with their includes, by key (git writes keys in lower
// case but for a subsection's name). git before 2.26 has no --show-scope:
// its local config alone is read then.
func ownConfig(ctx context.Context, dir string) []configEntry {
	out, err := git(ctx, dir, "config", "--list", "--show-scope", "--includes", "-z")
	if err == nil {
		// scope NUL key LF value NUL, for each setting.
		var entries []configEntry
		fields := bytes.Split(out, []byte{0})
		for i := 0; i+1 < len(fields); i += 2 {
			if scope := string(fields[i]); scope != "local" && scope != "worktree" {
				continue
			}
			key, value, _ := strings.Cut(string(fields[i+1]), "\n")
			entries = append(entries, configEntry{key, value})
		}
		return entries
	}
	out, err = git(ctx, dir, "config", "--local", "--list", "--includes", "-z")
	if err != nil {
		return nil
	}
	var entries []configEntry
	for field := range bytes.SplitSeq(out, []byte{0}) {
		if len(field) == 0 {
			continue
		}
		key, value, _ := strings.Cut(string(field), "\n")
		entries = append(entries, configEntry{key, value})
	}
	return entries
}

// runsProgram reports whether the setting key=value makes git run a
// program when it only reads the repository: to watch it, to clean or
// show a file, to check a signature, to reach a remote (git ls-remote).
func runsProgram(key, value string) bool {
	lower := strings.ToLower(key)
	first, _, _ := strings.Cut(lower, ".")
	last := lower[strings.LastIndex(lower, ".")+1:]
	switch {
	case lower == "core.fsmonitor":
		// true and false are git's own watcher, or none.
		return !slices.Contains([]string{"", "true", "false", "yes", "no", "on", "off", "1", "0"}, strings.ToLower(value))
	case lower == "core.sshcommand", lower == "core.gitproxy", lower == "core.askpass",
		lower == "core.alternaterefscommand", lower == "diff.external":
		return value != ""
	case first == "diff" && (last == "command" || last == "textconv"):
		return value != ""
	case first == "filter" && (last == "clean" || last == "smudge" || last == "process"):
		return value != ""
	case first == "gpg" && (last == "program" || last == "defaultkeycommand"):
		return value != ""
	case first == "credential" && last == "helper":
		return value != ""
	case first == "remote" && (last == "uploadpack" || last == "receivepack"):
		return value != ""
	case first == "protocol" && last == "allow":
		// ext:: runs a command as the remote; git allows it only when
		// told to.
		return strings.EqualFold(value, "always")
	}
	return false
}

// gitPath is git rev-parse --git-path path in dir: where git keeps path,
// core.hooksPath followed for hooks; "" when git cannot say.
func gitPath(ctx context.Context, dir, path string) string {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// git runs git with args in dir, with no terminal and no pager, and
// returns what it wrote.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "PAGER=cat")
	return cmd.Output()
}
