package tools

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/crush/internal/log"
)

var getRg = sync.OnceValue(func() string {
	if testing.Testing() {
		return ""
	}
	path, err := exec.LookPath("rg")
	if err != nil {
		if log.Initialized() {
			slog.Warn("Ripgrep (rg) not found in $PATH. Some grep features might be limited or slower.")
		}
		return ""
	}
	return path
})

func getRgCmd(ctx context.Context, globPattern string) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	// Note: we intentionally do not pass -L (follow symlinks). Following
	// symlinks lets rg escape the search root (into module caches, the nix
	// store, $HOME, etc.) and chase cycles, which pins all cores and can
	// hang. This keeps glob scoped to the tree it was pointed at, matching
	// the grep search command.
	args := []string{"--files", "--null"}
	if globPattern != "" {
		if !filepath.IsAbs(globPattern) && !strings.HasPrefix(globPattern, "/") {
			globPattern = "/" + globPattern
		}
		args = append(args, "--glob", globPattern)
	}
	return exec.CommandContext(ctx, name, args...)
}

func getRgSearchCmd(ctx context.Context, pattern, path string, opts grepOptions) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	// Use -n to show line numbers, -0 for null separation to handle Windows
	// paths. The pattern goes in with -e and the path after --, so that a
	// pattern such as "->getName" or "--force" is searched for rather than
	// read as an option.
	args := []string{"--json", "-H", "-n", "-0", "-e", pattern}
	if opts.include != "" {
		args = append(args, "--glob="+opts.include)
	}
	if opts.fileType != "" {
		args = append(args, "--type="+opts.fileType)
	}
	if opts.before > 0 {
		args = append(args, "--before-context="+strconv.Itoa(opts.before))
	}
	if opts.after > 0 {
		args = append(args, "--after-context="+strconv.Itoa(opts.after))
	}
	if opts.multiline {
		args = append(args, "--multiline", "--multiline-dotall")
	}
	// The project's ignore files, before the "--": after it ripgrep reads
	// an option as one more path to search.
	for _, ignoreFile := range []string{".gitignore", ".gnoshignore", ".klashignore", ".crushignore"} {
		ignorePath := filepath.Join(path, ignoreFile)
		if _, err := os.Stat(ignorePath); err == nil {
			args = append(args, "--ignore-file", ignorePath)
		}
	}
	args = append(args, "--", path)

	return exec.CommandContext(ctx, name, args...)
}
