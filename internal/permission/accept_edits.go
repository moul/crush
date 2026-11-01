package permission

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/fsext"
)

// acceptEditsTools are the tools whose edits acceptEdits lets run without
// asking: the file edits of Claude Code's acceptEdits. The names are
// internal/agent/tools', which imports this package. lsp_rename and
// lsp_replace_symbol, which change files too, still ask.
var acceptEditsTools = map[string]bool{"edit": true, "multiedit": true, "write": true}

// protectedDirs and protectedFiles are the paths no allowed tool, earlier
// grant for the session or acceptEdits lets a file tool's change of run
// unasked, as Claude Code's never do: what they hold runs code, sets
// things up or lets someone in (git hooks and config, a shell's startup
// files, editor and package-manager settings, ssh and gpg keys, Claude
// Code's and Gnosh's own settings), so such a change is asked about in
// every mode but yolo. A directory counts anywhere in a path;
// ".config/git" is two names in a row. Names match in any case: on a
// case-insensitive disk, as a Mac's, .GIT/hooks is .git/hooks.
var (
	protectedDirs = []string{
		".git", ".vscode", ".idea", ".husky", ".cargo", ".devcontainer", ".yarn", ".mvn",
		".claude", ".gnosh", ".klash", ".crush", ".ssh", ".gnupg",
	}
	protectedFiles = []string{
		".gitconfig", ".gitmodules", ".bashrc", ".bash_profile", ".bash_login", ".profile",
		".zshrc", ".zshenv", ".zprofile", ".zlogin", ".npmrc", ".mcp.json", ".claude.json",
		"gnosh.json", ".gnosh.json", "klash.json", ".klash.json", "crush.json", ".crush.json",
	}
)

// acceptsEdit reports whether acceptEdits lets an edit of path run
// without asking, as Claude Code's acceptEdits does: path is absolute,
// lies in the working directory or a directory added with --add-dir once
// its symlinks are resolved, and is not, as given or resolved, one of the
// protected paths or in one. Anything else is asked about as without
// acceptEdits. Before, the mode put the edit tools in the allowed tools,
// which let them write anywhere unasked: a shell's startup file, a git
// hook, Gnosh's own config.
func (s *permissionService) acceptsEdit(path string) bool {
	if !filepath.IsAbs(path) || s.protectedFile(path) {
		return false
	}
	real, _ := fsext.ResolvedPath(filepath.Clean(path))
	for _, root := range append([]string{s.workingDir}, s.dirs()...) {
		if root == "" {
			continue
		}
		if realRoot, ok := fsext.ResolvedPath(root); ok && within(realRoot, real) {
			return true
		}
	}
	return false
}

// protectedFile reports whether a change of path, a file a tool would
// change, is always asked about: path, as given or with its symlinks
// resolved, is or lies in one of the protected paths or in one of the
// service's protectedPaths, or ends in a symlink that leads nowhere. ""
// is no file.
func (s *permissionService) protectedFile(path string) bool {
	if path == "" {
		return false
	}
	real, ok := fsext.ResolvedPath(filepath.Clean(path))
	if !ok || protected(path) || protected(real) {
		return true
	}
	for _, dir := range s.protectedPaths {
		if withinFold(dir, path) || withinFold(dir, real) {
			return true
		}
	}
	return false
}

// fileIn reports whether path, a file a tool would change, lies in dir
// once both have their symlinks resolved; true for "", a request that
// names no file.
func fileIn(path, dir string) bool {
	if path == "" {
		return true
	}
	real, ok := fsext.ResolvedPath(filepath.Clean(path))
	realDir, dirOK := fsext.ResolvedPath(filepath.Clean(dir))
	return ok && dirOK && within(realDir, real)
}

// within reports whether path is root or lies in it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// withinFold is within with the names compared in any case, and root's
// symlinks resolved as well as written.
func withinFold(root, path string) bool {
	path = strings.ToLower(path)
	if within(strings.ToLower(root), path) {
		return true
	}
	real, ok := fsext.ResolvedPath(filepath.Clean(root))
	return ok && within(strings.ToLower(real), path)
}

// protected reports whether path is or lies in one of the protected
// paths.
func protected(path string) bool {
	names := strings.Split(strings.ToLower(filepath.ToSlash(path)), "/")
	for i, name := range names {
		if slices.Contains(protectedDirs, name) || (name == ".config" && i+1 < len(names) && names[i+1] == "git") {
			return true
		}
	}
	return slices.Contains(protectedFiles, names[len(names)-1])
}
