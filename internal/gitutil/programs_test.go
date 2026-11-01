package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// isolateGit gives the test a global git config of its own, and no system
// one, and returns a new repository.
func isolateGit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	run(t, repo, "init", "-q")
	run(t, repo, "config", "user.email", "t@example.com")
	run(t, repo, "config", "user.name", "t")
	return repo
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// What a repository handed over with its .git can make git run when git
// only reads it is found: in its own config, a file that config includes,
// and a post-index-change hook. git status really runs such a program.
func TestProgramsFindsWhatReadingTheRepositoryRuns(t *testing.T) {
	repo := isolateGit(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(repo, "x.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644))
	run(t, repo, "add", "a.txt")
	run(t, repo, "commit", "-qm", "a")

	require.Empty(t, Programs(repo), "a plain repository runs nothing of its own")

	run(t, repo, "config", "core.fsmonitor", script)
	run(t, repo, "config", "filter.evil.clean", "sh -c id")
	run(t, repo, "config", "diff.pic.textconv", "exiftool")
	include := filepath.Join(t.TempDir(), "more.cfg")
	require.NoError(t, os.WriteFile(include, []byte("[gpg]\n\tprogram = ./g.sh\n[core]\n\tsshCommand = ./s.sh\n"), 0o644))
	run(t, repo, "config", "include.path", include)
	hook := filepath.Join(repo, ".git", "hooks", "post-index-change")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o755))

	require.ElementsMatch(t, []string{
		"core.fsmonitor=" + script,
		"filter.evil.clean=sh -c id",
		"diff.pic.textconv=exiftool",
		"gpg.program=./g.sh",
		"core.sshcommand=./s.sh",
		"hook post-index-change",
	}, Programs(repo))
	require.ElementsMatch(t, Programs(repo), Programs(filepath.Join(repo, ".git", "..")))

	// The threat is real: git status runs the watcher the config names.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("changed\n"), 0o644))
	run(t, repo, "-c", "filter.evil.clean=", "status", "--short")
	require.FileExists(t, marker, "git status ran core.fsmonitor")
}

// Settings that run git's own code, or nothing, are not programs, and
// the user's own config is not the repository's.
func TestProgramsLeavesOutGitsOwnAndTheUsers(t *testing.T) {
	repo := isolateGit(t)
	run(t, repo, "config", "core.fsmonitor", "true")
	run(t, repo, "config", "log.showSignature", "true")
	run(t, repo, "config", "protocol.ext.allow", "never")
	run(t, repo, "config", "--global", "diff.external", "my-difftool")
	run(t, repo, "config", "--global", "filter.lfs.clean", "git-lfs clean -- %f")
	hook := filepath.Join(repo, ".git", "hooks", "post-index-change")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o644))
	require.Empty(t, Programs(repo), "a hook that is not executable does not run")
	require.Empty(t, Programs(t.TempDir()), "outside a repository there is nothing")
}
