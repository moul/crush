package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A command line runs without asking under Claude Code's Bash allow rules
// only when each command in it matches a rule on its own or only reads,
// and when nothing in it could run something else: as for read-only
// commands, a substitution, an assignment, a background job, a redirect
// to a file or a line that does not parse is asked about.
func TestAllowedCommand(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	dirs := workDirs{start: project, project: project}

	rules := commandRules([]string{"git diff *", "npm run *", "go test:*", "make", "find build -type f -delete", "watch -n1 ls"})
	for _, command := range []string{
		"git diff",
		"git diff --stat HEAD~1",
		"npm run build && npm run test",
		"go test ./... | tail -5",
		"make",
		"ls && git diff",
		"timeout 30 go test ./...",
		"nice -n 5 npm run lint",
		"git diff 2>&1 | head -20",
		"git diff > /dev/null",
		"git diff $BASE",
		"git diff '*.go'",
		"find build -type f -delete",
		"watch -n1 ls",
		"xargs go test < pkgs.txt",
	} {
		require.True(t, allowedCommand(command, dirs, rules), command)
	}
	for _, command := range []string{
		"git diff && rm -rf ~",
		"git commit -m wip",
		"make install",
		"npm test",
		"git diff > out.txt",
		"git diff $(rm -rf ~)",
		"git diff `rm -rf ~`",
		"GIT_EXTERNAL_DIFF=evil git diff",
		"git diff &",
		"npm run build &&",
		"(git diff)",
		"if true; then git diff; fi",
		"$GIT diff",
		"timeout 5 $CMD",
		"xargs -n1 go test",
		"find build -type f -name '*.o' -delete",
		"find build -type f -delete -name x",
		"watch -n2 ls",
		"git diff " + strings.Repeat("a", 10000),
	} {
		require.False(t, allowedCommand(command, dirs, rules), command)
	}
	require.False(t, allowedCommand("git diff", workDirs{}, nil), "no rules, no allowing")

	finds := commandRules([]string{"find *"})
	require.True(t, allowedCommand("find . -name x.go", workDirs{}, finds))
	require.False(t, allowedCommand("find . -name x.go -delete", workDirs{}, finds), "a prefix rule does not approve a find that deletes")
	require.False(t, allowedCommand("find . -name *.go", workDirs{}, finds), "an unquoted glob could expand to -delete")
	require.False(t, allowedCommand("watch ls", workDirs{}, commandRules([]string{"watch *"})), "nor an exec wrapper")

	versions := commandRules([]string{"* --version"})
	require.True(t, allowedCommand("node --version", workDirs{}, versions))
	require.True(t, allowedCommand("timeout 5 node --version", workDirs{}, versions))
	require.False(t, allowedCommand("$X --version", workDirs{}, versions), "the program must be named in plain text")
	require.False(t, allowedCommand("timeout 5 $X --version", workDirs{}, versions), "past its wrappers too")
}
