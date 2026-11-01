package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The bash tool runs a command without asking only when every part of it
// only reads, as Claude Code runs its read-only commands. The commands
// that ran unasked before, because they started with a safe name and
// held none of ";", "|", "&&", "$(" or a backquote, now ask: a second
// command on a new line or after &, a redirect to a file, a wrapper that
// runs another command, kill, and the forms of git that write.
func TestReadOnlyCommand(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	dirs := workDirs{start: project, project: project}

	for _, command := range []string{
		"ls", "ls -la", "pwd", "echo hello world", "ls *.go", "wc -l src/*.py", "echo $HOME", `echo "$HOME is home"`,
		"git status", "git log --oneline", "git diff HEAD~1", "git show HEAD:README.md", "git blame a.go",
		"git status && git diff", "git log | head -20", "ls; pwd", "ls\npwd", "! ls",
		"grep -rn foo . 2>/dev/null", "cat < README.md", "ls 2>&1", "ls >/dev/null 2>&1", "cat <<'EOF'\nhi\nEOF",
		"cat <<EOF\nhome is $HOME\nEOF", "head -20 a.go", "tail -n 5 a.go", "diff a b", "stat a.go",
		"git branch", "git branch -a", "git branch --show-current", "git tag", "git tag -l 'v1*'", "git remote -v",
		"git config --get user.name", "git config --list", "date", "date +%s", "hostname", "hostname -f",
		"ps aux", "ps -ef", "env", "printenv PATH",
	} {
		require.Truef(t, readOnlyCommand(command, dirs), "%q only reads", command)
	}

	for _, command := range []string{
		// Holes of the old check.
		"echo hi\nrm -rf ~", "ls & rm -rf ~", "ls & echo done", "echo x > ~/.zshrc", "ls > /tmp/out", "echo x >> notes",
		"timeout 5 rm -rf ~", "nice rm -rf ~", "nohup rm -rf ~", "time rm x", "env rm -rf ~", "kill 1234", "killall gnosh",
		"git branch -D main", "git branch newbranch", "git tag v1.0", "git tag -d v1", "git remote add x y",
		"git config user.name x", "git config --get x --unset y", "set -e",
		// Git that runs a program or writes a file.
		"git -c core.pager=evil log", "git log --output=/tmp/x", "git diff --ext-diff", "git grep -O vim foo",
		"git log *", "git log $REV",
		// Other commands' writing forms.
		"date -s 2020-01-01", "date --set=x", "hostname evil", "hostname -F file", "ps auxe", "ps aux -e",
		// What the shell would run or expand.
		"echo $(rm -rf ~)", `echo "$(rm -rf ~)"`, "echo `rm -rf ~`", "cat <(rm -rf ~)", "cat <<EOF\n$(rm -rf ~)\nEOF",
		"ls ${X:-$(rm -rf ~)}", "ls ${!X}", "FOO=bar ls", "GIT_EXTERNAL_DIFF=evil git diff", "$CMD", "l$X",
		"(ls)", "{ ls; }", "for f in a; do ls; done", "if true; then ls; fi", "echo hi | sh", "git log | xargs rm",
		// Writes and unknown programs.
		"ls &> out.txt", "ls >| out", "ls 2> err.txt", "rm -rf ~", "./script.sh", "/bin/ls", "touch x",
		"", "ls '", "echo " + strings.Repeat("a", 10000),
	} {
		require.Falsef(t, readOnlyCommand(command, dirs), "%q is not read-only", command)
	}
}
