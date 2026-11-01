package fsext

import (
	"cmp"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/charlievieth/fastwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/home"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// fastIgnoreDirs is a set of directory names that are always ignored.
// This provides O(1) lookup for common cases to avoid expensive pattern matching.
var fastIgnoreDirs = map[string]bool{
	".git":            true,
	".svn":            true,
	".hg":             true,
	".bzr":            true,
	".vscode":         true,
	".idea":           true,
	"node_modules":    true,
	"__pycache__":     true,
	".pytest_cache":   true,
	".cache":          true,
	".tmp":            true,
	".Trash":          true,
	".Spotlight-V100": true,
	".fseventsd":      true,
	".gnosh":          true,
	".klash":          true,
	".crush":          true,
	"OrbStack":        true,
	".local":          true,
	".share":          true,
}

// commonIgnorePatterns contains commonly ignored files and directories.
// Note: Exact directory names that are in fastIgnoreDirs are handled there for O(1) lookup.
// This list contains wildcard patterns and file-specific patterns.
var commonIgnorePatterns = sync.OnceValue(func() []gitignore.Pattern {
	patterns := []string{
		// IDE and editor files (wildcards)
		"*.swp",
		"*.swo",
		"*~",
		".DS_Store",
		"Thumbs.db",

		// Build artifacts (non-fastIgnoreDirs)
		"target",
		"build",
		"dist",
		"out",
		"bin",
		"obj",
		"*.o",
		"*.so",
		"*.dylib",
		"*.dll",
		"*.exe",

		// Logs and temporary files (wildcards)
		"*.log",
		"*.tmp",
		"*.temp",

		// Language-specific (wildcards and non-fastIgnoreDirs)
		"*.pyc",
		"*.pyo",
		"vendor",
		"Cargo.lock",
		"package-lock.json",
		"yarn.lock",
		"pnpm-lock.yaml",
	}
	return parsePatterns(patterns, nil)
})

// gitGlobalIgnorePatterns returns patterns from git's global excludes file
// (core.excludesFile), following git's config resolution order.
var gitGlobalIgnorePatterns = sync.OnceValue(func() []gitignore.Pattern {
	cfg, err := gitconfig.LoadConfig(gitconfig.GlobalScope)
	if err != nil {
		slog.Debug("Failed to load global git config", "error", err)
		return nil
	}

	excludesFilePath := cmp.Or(
		cfg.Raw.Section("core").Options.Get("excludesfile"),
		filepath.Join(home.Config(), "git", "ignore"),
	)
	excludesFilePath = home.Long(excludesFilePath)

	bts, err := os.ReadFile(excludesFilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("Failed to read git global excludes file", "path", excludesFilePath, "error", err)
		}
		return nil
	}

	return parsePatterns(strings.Split(string(bts), "\n"), nil)
})

// crushGlobalIgnorePatterns returns patterns from the user's global
// ignore files, ~/.config/gnosh/ignore and the klash and crush ones.
var crushGlobalIgnorePatterns = sync.OnceValue(func() []gitignore.Pattern {
	return globalIgnorePatterns(home.Config())
})

// globalIgnorePatterns reads the global ignore file under configDir by
// every name, gnosh and the klash and crush from before the renames. All
// are read, as all commands directories are: patterns add up, so one
// started under the new name drops none of an old one's.
func globalIgnorePatterns(configDir string) []gitignore.Pattern {
	var patterns []gitignore.Pattern
	for _, app := range []string{"gnosh", "klash", "crush"} {
		name := filepath.Join(configDir, app, "ignore")
		bts, err := os.ReadFile(name)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Debug("Failed to read global ignore file", "path", name, "error", err)
			}
			continue
		}
		patterns = append(patterns, parsePatterns(strings.Split(string(bts), "\n"), nil)...)
	}
	return patterns
}

// parsePatterns parses gitignore pattern strings into Pattern objects.
// domain is the path components where the patterns are defined (nil for global).
func parsePatterns(lines []string, domain []string) []gitignore.Pattern {
	var patterns []gitignore.Pattern
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, gitignore.ParsePattern(line, domain))
	}
	return patterns
}

type directoryLister struct {
	// dirPatterns caches parsed patterns from .gitignore/.crushignore for each directory.
	// This avoids re-reading files when building combined matchers.
	dirPatterns *csync.Map[string, []gitignore.Pattern]
	// combinedMatchers caches a combined matcher for each directory that includes
	// all ancestor patterns. This allows O(1) matching per file.
	combinedMatchers *csync.Map[string, gitignore.Matcher]
	rootPath         string
}

func NewDirectoryLister(rootPath string) *directoryLister {
	return &directoryLister{
		rootPath:         rootPath,
		dirPatterns:      csync.NewMap[string, []gitignore.Pattern](),
		combinedMatchers: csync.NewMap[string, gitignore.Matcher](),
	}
}

// pathToComponents splits a path into its components for gitignore matching.
func pathToComponents(path string) []string {
	path = filepath.ToSlash(path)
	if path == "" || path == "." {
		return nil
	}
	return strings.Split(path, "/")
}

// getDirPatterns returns the parsed patterns for a specific directory's
// .gitignore and .crushignore files. Results are cached.
func (dl *directoryLister) getDirPatterns(dir string) []gitignore.Pattern {
	return dl.dirPatterns.GetOrSet(dir, func() []gitignore.Pattern {
		var allPatterns []gitignore.Pattern

		relPath, _ := filepath.Rel(dl.rootPath, dir)
		var domain []string
		if relPath != "" && relPath != "." {
			domain = pathToComponents(relPath)
		}

		for _, ignoreFile := range []string{".gitignore", ".gnoshignore", ".klashignore", ".crushignore"} {
			ignPath := filepath.Join(dir, ignoreFile)
			if content, err := os.ReadFile(ignPath); err == nil {
				lines := strings.Split(string(content), "\n")
				allPatterns = append(allPatterns, parsePatterns(lines, domain)...)
			}
		}
		return allPatterns
	})
}

// getCombinedMatcher returns a matcher that combines all gitignore patterns
// from the root to the given directory, plus common patterns and home patterns.
// Results are cached per directory, and we reuse parent directory matchers.
func (dl *directoryLister) getCombinedMatcher(dir string) gitignore.Matcher {
	return dl.combinedMatchers.GetOrSet(dir, func() gitignore.Matcher {
		var allPatterns []gitignore.Pattern

		// Add common patterns first (lowest priority).
		allPatterns = append(allPatterns, commonIgnorePatterns()...)

		// Add global ignore patterns (git core.excludesFile + crush global ignore).
		allPatterns = append(allPatterns, gitGlobalIgnorePatterns()...)
		allPatterns = append(allPatterns, crushGlobalIgnorePatterns()...)

		// Collect patterns from root to this directory.
		relDir, _ := filepath.Rel(dl.rootPath, dir)
		var pathParts []string
		if relDir != "" && relDir != "." {
			pathParts = pathToComponents(relDir)
		}

		// Add patterns from each directory from root to current.
		currentPath := dl.rootPath
		allPatterns = append(allPatterns, dl.getDirPatterns(currentPath)...)

		for _, part := range pathParts {
			currentPath = filepath.Join(currentPath, part)
			allPatterns = append(allPatterns, dl.getDirPatterns(currentPath)...)
		}

		return gitignore.NewMatcher(allPatterns)
	})
}

// shouldIgnore checks if a path should be ignored based on gitignore rules.
// This uses a combined matcher that includes all ancestor patterns for O(1) matching.
func (dl *directoryLister) shouldIgnore(path string, ignorePatterns []string, isDir bool) bool {
	// The root itself is never ignored: a search asked to look in
	// ~/.local or in a .git directory looks there.
	if path == dl.rootPath {
		return false
	}

	base := filepath.Base(path)

	// Fast path: O(1) lookup for commonly ignored directories.
	if isDir && fastIgnoreDirs[base] {
		return true
	}

	// Check explicit ignore patterns.
	if len(ignorePatterns) > 0 {
		for _, pattern := range ignorePatterns {
			if matched, err := filepath.Match(pattern, base); err == nil && matched {
				return true
			}
		}
	}

	relPath, err := filepath.Rel(dl.rootPath, path)
	if err != nil {
		relPath = path
	}

	pathComponents := pathToComponents(relPath)
	if len(pathComponents) == 0 {
		return false
	}

	// Get the combined matcher for the parent directory.
	parentDir := filepath.Dir(path)
	matcher := dl.getCombinedMatcher(parentDir)

	if matcher.Match(pathComponents, isDir) {
		slog.Debug("Ignoring path", "path", relPath)
		return true
	}

	return false
}

// ListDirectory lists files and directories in the specified path. It
// follows a symlink to a directory only when the link leads to somewhere
// in the path listed: one that leads out of it (to ~/.ssh, say, from a
// downloaded repository) is listed, and what it leads to is not.
func ListDirectory(initialPath string, ignorePatterns []string, depth, limit int) ([]string, bool, error) {
	found := csync.NewSlice[string]()
	dl := NewDirectoryLister(initialPath)
	root := realAbs(initialPath)

	slog.Debug("Listing directory", "path", initialPath, "depth", depth, "limit", limit, "ignorePatterns", ignorePatterns)

	conf := fastwalk.Config{
		Follow:   true,
		ToSlash:  fastwalk.DefaultToSlash(),
		Sort:     fastwalk.SortDirsFirst,
		MaxDepth: depth,
	}

	err := fastwalk.Walk(&conf, initialPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // Skip files we don't have permission to access
		}

		isDir := d.IsDir()
		if dl.shouldIgnore(path, ignorePatterns, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}

		if path != initialPath {
			if isDir {
				path = path + string(filepath.Separator)
			}
			found.Append(path)
		}

		if limit > 0 && found.Len() >= limit {
			return filepath.SkipAll
		}
		if path != initialPath && d.Type()&os.ModeSymlink != 0 && (root == "" || !HasPrefix(realAbs(path), root)) {
			return filepath.SkipDir // a link out: not followed
		}

		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, false, err
	}

	matches, truncated := truncate(slices.Collect(found.Seq()), limit)
	return matches, truncated || errors.Is(err, filepath.SkipAll), nil
}

// realAbs is path made absolute with its symlinks resolved, "" when that
// cannot be done (a link that leads nowhere).
func realAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	real, ok := ResolvedPath(abs)
	if !ok {
		return ""
	}
	return real
}
