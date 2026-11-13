package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// setRipgrepForTest swaps the cached ripgrep path for the duration of the
// test. It must only be used from tests that are not marked t.Parallel(),
// since the variable is shared package-wide. An empty path simulates
// ripgrep being absent from $PATH.
func setRipgrepForTest(t *testing.T, path string) {
	t.Helper()
	orig := getRg
	t.Cleanup(func() { getRg = orig })
	if path == "" {
		getRg = func() string { return "" }
	} else {
		getRg = func() string { return path }
	}
}

// fakeRipgrep writes an executable stand-in for rg that prints stdout and
// stderr as given and exits with code.
func fakeRipgrep(t *testing.T, stdout, stderr string, code int) string {
	t.Helper()
	dir := t.TempDir()
	out, errOut := filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
	require.NoError(t, os.WriteFile(out, []byte(stdout), 0o644))
	require.NoError(t, os.WriteFile(errOut, []byte(stderr), 0o644))
	path := filepath.Join(dir, "rg")
	script := fmt.Sprintf("#!/bin/sh\ncat %q\ncat %q >&2\nexit %d\n", out, errOut, code)
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestRegexCache(t *testing.T) {
	cache := newRegexCache()

	// Test basic caching
	pattern := "test.*pattern"
	regex1, err := cache.get(pattern)
	if err != nil {
		t.Fatalf("Failed to compile regex: %v", err)
	}

	regex2, err := cache.get(pattern)
	if err != nil {
		t.Fatalf("Failed to get cached regex: %v", err)
	}

	// Should be the same instance (cached)
	if regex1 != regex2 {
		t.Error("Expected cached regex to be the same instance")
	}

	// Test that it actually works
	if !regex1.MatchString("test123pattern") {
		t.Error("Regex should match test string")
	}
}

func TestGlobToRegexCaching(t *testing.T) {
	// Test that globToRegex uses pre-compiled regex
	pattern1 := globToRegex("*.{js,ts}")

	// Should not panic and should work correctly
	regex1, err := regexp.Compile(pattern1)
	if err != nil {
		t.Fatalf("Failed to compile glob regex: %v", err)
	}

	if !regex1.MatchString("test.js") {
		t.Error("Glob regex should match .js files")
	}
	if !regex1.MatchString("test.ts") {
		t.Error("Glob regex should match .ts files")
	}
	if regex1.MatchString("test.go") {
		t.Error("Glob regex should not match .go files")
	}
}

func TestGrepWithIgnoreFiles(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// Create test files
	testFiles := map[string]string{
		"file1.txt":           "hello world",
		"file2.txt":           "hello world",
		"ignored/file3.txt":   "hello world",
		"node_modules/lib.js": "hello world",
		"secret.key":          "hello world",
	}

	for path, content := range testFiles {
		fullPath := filepath.Join(tempDir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
	}

	// Create .gitignore file
	gitignoreContent := "ignored/\n*.key\n"
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".gitignore"), []byte(gitignoreContent), 0o644))

	// Create .crushignore file
	crushignoreContent := "node_modules/\n"
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".crushignore"), []byte(crushignoreContent), 0o644))

	// Test both implementations
	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(pattern, path, grepOptions{include: include}, 200)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			return searchWithRipgrep(t.Context(), pattern, path, grepOptions{include: include})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("hello world", tempDir, "")
			require.NoError(t, err)

			// Convert matches to a set of file paths for easier testing
			foundFiles := make(map[string]bool)
			for _, match := range matches {
				foundFiles[filepath.Base(match.path)] = true
			}

			// Should find file1.txt and file2.txt
			require.True(t, foundFiles["file1.txt"], "Should find file1.txt")
			require.True(t, foundFiles["file2.txt"], "Should find file2.txt")

			// Should NOT find ignored files
			require.False(t, foundFiles["file3.txt"], "Should not find file3.txt (ignored by .gitignore)")
			require.False(t, foundFiles["lib.js"], "Should not find lib.js (ignored by .crushignore)")
			require.False(t, foundFiles["secret.key"], "Should not find secret.key (ignored by .gitignore)")

			// Should find exactly 2 matches
			require.Equal(t, 2, len(matches), "Should find exactly 2 matches")
		})
	}
}

func TestSearchImplementations(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	for path, content := range map[string]string{
		"file1.go":         "package main\nfunc main() {\n\tfmt.Println(\"hello world\")\n}",
		"file2.js":         "console.log('hello world');",
		"file3.txt":        "hello world from text file",
		"binary.exe":       "\x00\x01\x02\x03",
		"empty.txt":        "",
		"subdir/nested.go": "package nested\n// hello world comment",
		".hidden.txt":      "hello world in hidden file",
		"file4.txt":        "hello world from a banana",
		"file5.txt":        "hello world from a grape",
	} {
		fullPath := filepath.Join(tempDir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
	}

	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".gitignore"), []byte("file4.txt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".crushignore"), []byte("file5.txt\n"), 0o644))

	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(pattern, path, grepOptions{include: include}, 200)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			return searchWithRipgrep(t.Context(), pattern, path, grepOptions{include: include})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("hello world", tempDir, "")
			require.NoError(t, err)

			require.Equal(t, len(matches), 4)
			for _, match := range matches {
				require.NotEmpty(t, match.path)
				require.NotZero(t, match.lineNum)
				require.NotEmpty(t, match.lineText)
				require.NotZero(t, match.modTime)
				require.NotContains(t, match.path, ".hidden.txt")
				require.NotContains(t, match.path, "file4.txt")
				require.NotContains(t, match.path, "file5.txt")
				require.NotContains(t, match.path, "binary.exe")
			}
		})
	}
}

// Benchmark to show performance improvement
func BenchmarkRegexCacheVsCompile(b *testing.B) {
	cache := newRegexCache()
	pattern := "test.*pattern.*[0-9]+"

	b.Run("WithCache", func(b *testing.B) {
		for b.Loop() {
			_, err := cache.get(pattern)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("WithoutCache", func(b *testing.B) {
		for b.Loop() {
			_, err := regexp.Compile(pattern)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestIsTextFile(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		filename string
		content  []byte
		wantText bool
	}{
		{
			name:     "go file",
			filename: "test.go",
			content:  []byte("package main\n\nfunc main() {}\n"),
			wantText: true,
		},
		{
			name:     "yaml file",
			filename: "config.yaml",
			content:  []byte("key: value\nlist:\n  - item1\n  - item2\n"),
			wantText: true,
		},
		{
			name:     "yml file",
			filename: "config.yml",
			content:  []byte("key: value\n"),
			wantText: true,
		},
		{
			name:     "json file",
			filename: "data.json",
			content:  []byte(`{"key": "value"}`),
			wantText: true,
		},
		{
			name:     "javascript file",
			filename: "script.js",
			content:  []byte("console.log('hello');\n"),
			wantText: true,
		},
		{
			name:     "typescript file",
			filename: "script.ts",
			content:  []byte("const x: string = 'hello';\n"),
			wantText: true,
		},
		{
			name:     "markdown file",
			filename: "README.md",
			content:  []byte("# Title\n\nSome content\n"),
			wantText: true,
		},
		{
			name:     "shell script",
			filename: "script.sh",
			content:  []byte("#!/bin/bash\necho 'hello'\n"),
			wantText: true,
		},
		{
			name:     "python file",
			filename: "script.py",
			content:  []byte("print('hello')\n"),
			wantText: true,
		},
		{
			name:     "xml file",
			filename: "data.xml",
			content:  []byte("<?xml version=\"1.0\"?>\n<root></root>\n"),
			wantText: true,
		},
		{
			name:     "plain text",
			filename: "file.txt",
			content:  []byte("plain text content\n"),
			wantText: true,
		},
		{
			name:     "css file",
			filename: "style.css",
			content:  []byte("body { color: red; }\n"),
			wantText: true,
		},
		{
			name:     "scss file",
			filename: "style.scss",
			content:  []byte("$primary: blue;\nbody { color: $primary; }\n"),
			wantText: true,
		},
		{
			name:     "sass file",
			filename: "style.sass",
			content:  []byte("$primary: blue\nbody\n  color: $primary\n"),
			wantText: true,
		},
		{
			name:     "rust file",
			filename: "main.rs",
			content:  []byte("fn main() {\n    println!(\"Hello, world!\");\n}\n"),
			wantText: true,
		},
		{
			name:     "zig file",
			filename: "main.zig",
			content:  []byte("const std = @import(\"std\");\npub fn main() void {}\n"),
			wantText: true,
		},
		{
			name:     "java file",
			filename: "Main.java",
			content:  []byte("public class Main {\n    public static void main(String[] args) {}\n}\n"),
			wantText: true,
		},
		{
			name:     "c file",
			filename: "main.c",
			content:  []byte("#include <stdio.h>\nint main() { return 0; }\n"),
			wantText: true,
		},
		{
			name:     "cpp file",
			filename: "main.cpp",
			content:  []byte("#include <iostream>\nint main() { return 0; }\n"),
			wantText: true,
		},
		{
			name:     "fish shell",
			filename: "script.fish",
			content:  []byte("#!/usr/bin/env fish\necho 'hello'\n"),
			wantText: true,
		},
		{
			name:     "powershell file",
			filename: "script.ps1",
			content:  []byte("Write-Host 'Hello, World!'\n"),
			wantText: true,
		},
		{
			name:     "cmd batch file",
			filename: "script.bat",
			content:  []byte("@echo off\necho Hello, World!\n"),
			wantText: true,
		},
		{
			name:     "cmd file",
			filename: "script.cmd",
			content:  []byte("@echo off\necho Hello, World!\n"),
			wantText: true,
		},
		{
			name:     "binary exe",
			filename: "binary.exe",
			content:  []byte{0x4D, 0x5A, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00},
			wantText: false,
		},
		{
			name:     "png image",
			filename: "image.png",
			content:  []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
			wantText: false,
		},
		{
			name:     "jpeg image",
			filename: "image.jpg",
			content:  []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46},
			wantText: false,
		},
		{
			name:     "zip archive",
			filename: "archive.zip",
			content:  []byte{0x50, 0x4B, 0x03, 0x04, 0x14, 0x00, 0x00, 0x00},
			wantText: false,
		},
		{
			name:     "pdf file",
			filename: "document.pdf",
			content:  []byte("%PDF-1.4\n%âãÏÓ\n"),
			wantText: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			filePath := filepath.Join(tempDir, tt.filename)
			require.NoError(t, os.WriteFile(filePath, tt.content, 0o644))

			got := isTextFile(filePath)
			require.Equal(t, tt.wantText, got, "isTextFile(%s) = %v, want %v", tt.filename, got, tt.wantText)
		})
	}
}

func TestMultipleMatchesPerFile(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// "world" appears on lines 1 and 3, but not line 2. Both grep
	// implementations must report every matching line, not just the first.
	content := "Hello world.\nHello.\nHello world.\n"
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "file.txt"), []byte(content), 0o644))

	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(pattern, path, grepOptions{include: include}, 200)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			return searchWithRipgrep(t.Context(), pattern, path, grepOptions{include: include})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("world", tempDir, "")
			require.NoError(t, err)
			require.Len(t, matches, 2, "should report both matching lines")

			lines := make([]int, len(matches))
			for i, match := range matches {
				lines[i] = match.lineNum
				require.Equal(t, 7, match.charNum)
				require.Equal(t, "Hello world.", match.lineText)
			}
			require.ElementsMatch(t, []int{1, 3}, lines)
		})
	}
}

func TestColumnMatch(t *testing.T) {
	t.Parallel()

	// Test both implementations
	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(pattern, path, grepOptions{include: include}, 200)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			return searchWithRipgrep(t.Context(), pattern, path, grepOptions{include: include})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("THIS", "./testdata/", "")
			require.NoError(t, err)
			require.Len(t, matches, 1)
			match := matches[0]
			require.Equal(t, 2, match.lineNum)
			require.Equal(t, 14, match.charNum)
			require.Equal(t, "I wanna grep THIS particular word", match.lineText)
			require.Equal(t, "testdata/grep.txt", filepath.ToSlash(filepath.Clean(match.path)))
		})
	}
}

// A search for code as written, with its brackets unescaped, finds nothing;
// the result must say why instead of reading as "no such code".
func TestGrepExplainsRegexOperatorsOnNoMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := "package server\n\nfunc (s *Server) Start() {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.go"), []byte(src), 0o644))
	tool := NewGrepTool(&mockPermissionService{}, dir, config.ToolGrep{})

	grep := func(t *testing.T, params GrepParams) string {
		t.Helper()
		input, err := json.Marshal(params)
		require.NoError(t, err)
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "grep-call", Name: GrepToolName, Input: string(input)})
		require.NoError(t, err)
		require.False(t, resp.IsError, resp.Content)
		return resp.Content
	}
	const hint = "( ) [ ] { } are operators"

	for _, tc := range []struct {
		name     string
		params   GrepParams
		found    bool
		wantHint bool
	}{
		{"unescaped parentheses miss, with the reason", GrepParams{Pattern: `func (s \*Server)`}, false, true},
		{"escaped, the same search finds it", GrepParams{Pattern: `func \(s \*Server\)`}, true, false},
		{"as literal text, it finds it", GrepParams{Pattern: `func (s *Server)`, LiteralText: true}, true, false},
		{"a plain miss has no hint", GrepParams{Pattern: `NoSuchThing`}, false, false},
		{"an escaped miss has no hint", GrepParams{Pattern: `func \(s \*Client\)`}, false, false},
		{"a literal miss has no hint", GrepParams{Pattern: `func (c *Client)`, LiteralText: true}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := grep(t, tc.params)
			if tc.found {
				require.Contains(t, out, "Found 1 matches")
			} else {
				require.True(t, strings.HasPrefix(out, "No files found"), out)
			}
			if tc.wantHint {
				require.Contains(t, out, hint)
			} else {
				require.NotContains(t, out, hint)
			}
		})
	}
}

// A search that runs out of time must stop and say so; it must not fall
// back to the Go walker, which takes no context and would search on
// unbounded.
func TestGrepTimeoutStopsAndReports(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644))

	// A fake rg that never returns: the timeout must fire, and the tool
	// must report it rather than continuing with the Go walker.
	path := filepath.Join(t.TempDir(), "rg")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))
	setRipgrepForTest(t, path)

	timeout := 200 * time.Millisecond
	tool := NewGrepTool(&mockPermissionService{}, dir, config.ToolGrep{Timeout: &timeout})
	input, err := json.Marshal(GrepParams{Pattern: "hello"})
	require.NoError(t, err)
	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "grep-call", Name: GrepToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "timed out")
	require.Contains(t, resp.Content, "narrower path or pattern")
}

// When ripgrep is not installed, searchFiles falls back to the Go walker.
func TestSearchFilesFallsBackToRegexWhenRipgrepMissing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644))

	setRipgrepForTest(t, "")

	matches, _, err := searchFiles(t.Context(), "hello", dir, grepOptions{}, 100)
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

// Other ripgrep failures, such as a pattern it rejects, must be reported as
// the tool's error, not retried with the Go walker.
func TestSearchFilesReportsRipgrepFailure(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644))

	setRipgrepForTest(t, fakeRipgrep(t, "", "", 2))

	_, _, err := searchFiles(t.Context(), "hello", dir, grepOptions{}, 100)
	require.Error(t, err)
}

// A path that does not exist is a real error: report it, do not fall back.
func TestSearchFilesReportsMissingPath(t *testing.T) {
	dir := t.TempDir()
	setRipgrepForTest(t, "")

	missing := filepath.Join(dir, "no-such-dir")
	_, _, err := searchFiles(t.Context(), "hello", missing, grepOptions{}, 100)
	require.Error(t, err)
}

// ripgrep exits 2 when it could not read some file, even when the search
// otherwise ran and found matches; those matches are still the answer.
func TestSearchFilesKeepsMatchesWhenRipgrepCouldNotReadAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("hello world\n"), 0o644))
	stdout := fmt.Sprintf(`{"type":"match","data":{"path":{"text":%q},"lines":{"text":"hello world\n"},"line_number":1,"submatches":[{"match":{"text":"hello"},"start":0,"end":5}]}}`, file) +
		"\n" + `{"type":"summary","data":{}}` + "\n"
	setRipgrepForTest(t, fakeRipgrep(t, stdout, "rg: "+dir+"/locked: Permission denied (os error 13)\n", 2))

	matches, _, err := searchFiles(t.Context(), "hello", dir, grepOptions{}, 100)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Equal(t, file, matches[0].path)
}

// A search ripgrep never ran, such as one for a pattern it rejects, fails
// with ripgrep's own message, not a bare exit status.
func TestSearchFilesReportsRipgrepsMessage(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644))
	setRipgrepForTest(t, fakeRipgrep(t, "", "rg: regex parse error:\n    (?:func (s)\n    ^\nerror: unclosed group\n", 2))

	_, _, err := searchFiles(t.Context(), "func (s", dir, grepOptions{}, 100)
	require.ErrorContains(t, err, "unclosed group")
}

// The same against the real ripgrep, where it is installed: the package's
// other tests run without it.
func TestSearchFilesWithTheRealRipgrep(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg is not installed")
	}
	setRipgrepForTest(t, rg)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("func (s *Server) Start()\n"), 0o644))

	_, _, err = searchFiles(t.Context(), "func (s", dir, grepOptions{}, 100)
	require.ErrorContains(t, err, "unclosed group", "a pattern ripgrep rejects is reported with its message")

	matches, _, err := searchFiles(t.Context(), `func \(s`, dir, grepOptions{}, 100)
	require.NoError(t, err)
	require.Len(t, matches, 1)

	matches, _, err = searchFiles(t.Context(), `(?i)FUNC \(S`, dir, grepOptions{}, 100)
	require.NoError(t, err)
	require.Len(t, matches, 1, "the inline flag that ignore_case adds works in ripgrep too")

	_, _, err = searchFiles(t.Context(), "x", filepath.Join(dir, "missing"), grepOptions{}, 100)
	require.Error(t, err, "a missing path is an error, not an empty result")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.php"), []byte("$user->getName();\nrun --force\n"), 0o644))
	for _, pattern := range []string{"->getName", "--force"} {
		matches, _, err = searchFiles(t.Context(), pattern, dir, grepOptions{}, 100)
		require.NoError(t, err, "a pattern starting with a dash is searched for, not read as an option")
		require.Len(t, matches, 1, pattern)
	}
	matches, _, err = searchFiles(t.Context(), "Start|getName", dir, grepOptions{include: "*.php"}, 100)
	require.NoError(t, err)
	require.Len(t, matches, 1, "include still filters: a.txt's Start is left out")
}

// A project's ignore files keep their files out of a ripgrep search, and
// the ignore file itself is not searched: both only hold when
// --ignore-file goes in before the "--" that ends the options.
func TestRipgrepHonoursTheProjectIgnoreFiles(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg is not installed")
	}
	setRipgrepForTest(t, rg)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("needle\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendored.txt"), []byte("needle\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gnoshignore"), []byte("vendored.txt\n# needle\n"), 0o644))

	matches, _, err := searchFiles(t.Context(), "needle", dir, grepOptions{}, 100)
	require.NoError(t, err)
	var found []string
	for _, m := range matches {
		found = append(found, filepath.Base(m.path))
	}
	require.ElementsMatch(t, []string{"kept.txt"}, found)
}

// ignore_case matches regardless of case, as grep -i does, so a model has
// no reason to search in the shell for that.
func TestGrepIgnoreCase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("// Hello, World\n"), 0o644))
	tool := NewGrepTool(&mockPermissionService{}, dir, config.ToolGrep{})
	grep := func(params GrepParams) string {
		input, err := json.Marshal(params)
		require.NoError(t, err)
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "grep-call", Name: GrepToolName, Input: string(input)})
		require.NoError(t, err)
		require.False(t, resp.IsError, resp.Content)
		return resp.Content
	}

	require.True(t, strings.HasPrefix(grep(GrepParams{Pattern: "hello world"}), "No files found"))
	require.Contains(t, grep(GrepParams{Pattern: "hello, world", IgnoreCase: true}), "Found 1 matches")
	require.Contains(t, grep(GrepParams{Pattern: "HELLO, (world)", IgnoreCase: true, LiteralText: true}), "No files found",
		"literal text stays literal: the parentheses are not in the file")
	require.Contains(t, grep(GrepParams{Pattern: "HELLO, WORLD", IgnoreCase: true, LiteralText: true}), "Found 1 matches")
}

// grepRaw runs the grep tool on dir with the raw JSON a model sends,
// Claude Code's parameter names and all.
func grepRaw(t *testing.T, dir, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := NewGrepTool(&mockPermissionService{}, dir, config.ToolGrep{}).Run(t.Context(), fantasy.ToolCall{ID: "grep-call", Name: GrepToolName, Input: input})
	require.NoError(t, err)
	return resp
}

// claudeCodeGrepDir writes the files the tests of Claude Code's grep
// parameters search.
func claudeCodeGrepDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("one\ntwo\nthree\nfour\nfive\nsix three\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("Three\n"), 0o644))
	return dir
}

// grepContent runs the grep tool on dir with input and returns what it
// answered, which must not be an error.
func grepContent(t *testing.T, dir, input string) string {
	t.Helper()
	resp := grepRaw(t, dir, input)
	require.False(t, resp.IsError, resp.Content)
	return resp.Content
}

// checkClaudeCodeGrep checks Claude Code's Grep parameters on a
// claudeCodeGrepDir, whichever engine searches. A model used to them
// sends -A, -B and -C for the lines around a match, glob and -i for
// include and ignore_case, output_mode to list the files or count the
// matching lines, and head_limit to cap the results. They were dropped
// without a word.
func checkClaudeCodeGrep(t *testing.T, dir string) {
	t.Helper()
	content := func(input string) string { return grepContent(t, dir, input) }

	require.Contains(t, content(`{"pattern": "three", "glob": "*.go", "-C": 1}`),
		"  Line 2- two\n  Line 3, Char 1: three\n  Line 4- four\n  Line 5- five\n  Line 6, Char 5: six three\n")
	after := content(`{"pattern": "^three", "glob": "*.go", "-A": 1}`)
	require.Contains(t, after, "  Line 3, Char 1: three\n  Line 4- four\n")
	require.NotContains(t, after, "Line 2-")
	require.Contains(t, content(`{"pattern": "one|five", "glob": "*.go", "-A": 1}`),
		"  Line 1, Char 1: one\n  Line 2- two\n  --\n  Line 5, Char 1: five\n  Line 6- six three\n")
	require.Contains(t, content(`{"pattern": "four", "glob": "*.go", "-B": 2}`),
		"  Line 2- two\n  Line 3- three\n  Line 4, Char 1: four\n")

	require.Contains(t, content(`{"pattern": "three", "-i": true}`), "Found 3 matches")
	require.Contains(t, content(`{"pattern": "three", "-i": true, "glob": "*.txt"}`), "Found 1 matches")
	files := content(`{"pattern": "three", "-i": true, "output_mode": "files_with_matches"}`)
	require.Contains(t, files, "Found 2 files\n")
	require.Contains(t, files, filepath.ToSlash(filepath.Join(dir, "b.txt"))+"\n")
	require.NotContains(t, files, "Line ")
	counts := content(`{"pattern": "three", "-i": true, "output_mode": "count"}`)
	require.Contains(t, counts, "Found 3 matches in 2 files\n")
	require.Contains(t, counts, filepath.ToSlash(filepath.Join(dir, "a.go"))+": 2\n")

	limited := content(`{"pattern": "three", "glob": "*.go", "head_limit": 1}`)
	require.Contains(t, limited, "Found 2 matches\n")
	require.Contains(t, limited, "  Line 3, Char 1: three\n")
	require.NotContains(t, limited, "six three")
	require.True(t, strings.HasSuffix(limited, "\n[Showing results with pagination = limit: 1]"), limited)
	next := content(`{"pattern": "three", "glob": "*.go", "head_limit": 1, "offset": 1}`)
	require.Contains(t, next, "  Line 6, Char 5: six three\n")
	require.NotContains(t, next, "Line 3")
	require.True(t, strings.HasSuffix(next, "\n[Showing results with pagination = offset: 1]"), next)
	require.Contains(t, content(`{"pattern": "three", "glob": "*.go", "offset": 5}`), "No entries at this offset\n[Showing results with pagination = offset: 5]")
	require.NotContains(t, content(`{"pattern": "three", "glob": "*.go", "head_limit": 0}`), "pagination")
	pagedFiles := content(`{"pattern": "three", "-i": true, "output_mode": "files_with_matches", "head_limit": 1}`)
	require.Contains(t, pagedFiles, "Found 2 files\n")
	require.Equal(t, 1, strings.Count(pagedFiles, filepath.ToSlash(dir)), pagedFiles)
	require.True(t, strings.HasSuffix(pagedFiles, "\n[Showing results with pagination = limit: 1]"), pagedFiles)
	require.Contains(t, grepRaw(t, dir, `{"pattern": "three", "head_limit": -1}`).Content, "head_limit must be a whole number of 0 or more, got -1. Pass 0 for unlimited.")
	require.Contains(t, grepRaw(t, dir, `{"pattern": "three", "offset": -2}`).Content, "offset must be a whole number of 0 or more, got -2.")

	bad := grepRaw(t, dir, `{"pattern": "three", "output_mode": "lines"}`)
	require.True(t, bad.IsError)
	require.Contains(t, bad.Content, `output_mode "lines"`)
}

// Without ripgrep, as in tests, the Go walker takes Claude Code's grep
// parameters, and says type needs ripgrep.
func TestGrepTakesClaudeCodesParameters(t *testing.T) {
	t.Parallel()

	dir := claudeCodeGrepDir(t)
	checkClaudeCodeGrep(t, dir)
	require.Contains(t, grepRaw(t, dir, `{"pattern": "three", "type": "go"}`).Content, "need ripgrep")
}

// With ripgrep the same holds, the context is the walker's line for line,
// and type and multiline, which only ripgrep does, work.
func TestGrepTakesClaudeCodesParametersWithTheRealRipgrep(t *testing.T) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg is not installed")
	}
	setRipgrepForTest(t, rg)
	dir := claudeCodeGrepDir(t)
	checkClaudeCodeGrep(t, dir)

	opts := grepOptions{before: 1, after: 1}
	fromRipgrep, err := searchWithRipgrep(t.Context(), "three", dir, opts)
	require.NoError(t, err)
	fromWalker, err := searchFilesWithRegex("three", dir, opts, 100)
	require.NoError(t, err)
	require.Equal(t, lineKinds(fromWalker), lineKinds(fromRipgrep))

	require.Contains(t, grepContent(t, dir, `{"pattern": "three", "type": "go", "-i": true}`), "Found 2 matches")
	require.Contains(t, grepContent(t, dir, `{"pattern": "two\\nthree", "multiline": true, "glob": "*.go"}`),
		"  Line 2, Char 1: two\n  Line 3: three\n")
}

// lineKinds is each line's number and whether it matched, to compare
// two searches.
func lineKinds(lines []grepMatch) []string {
	var kinds []string
	for _, l := range lines {
		kind := "match"
		if l.context {
			kind = "context"
		}
		kinds = append(kinds, fmt.Sprintf("%s:%d %s", filepath.Base(l.path), l.lineNum, kind))
	}
	return kinds
}

// The walker shows each context line once, however many matches it is
// near, and no line past the end of a file.
func TestSearchFilesWithRegexContext(t *testing.T) {
	t.Parallel()

	dir := claudeCodeGrepDir(t)
	lines, err := searchFilesWithRegex("three", dir, grepOptions{before: 1, after: 1}, 100)
	require.NoError(t, err)
	require.Equal(t, []string{"a.go:2 context", "a.go:3 match", "a.go:4 context", "a.go:5 context", "a.go:6 match"}, lineKinds(lines))
}

// Cut at head_limit, a kept match keeps the context that runs on from
// it, and the context of the first match left out goes with it.
func TestFirstMatchesKeepsContextWithItsMatch(t *testing.T) {
	t.Parallel()

	line := func(n int, context bool) grepMatch {
		return grepMatch{path: "a.go", lineNum: n, lineText: "x", context: context}
	}
	lines := []grepMatch{line(2, true), line(3, false), line(4, true), line(9, true), line(10, false), line(11, true)}
	kept, truncated := firstMatches(lines, 1)
	require.True(t, truncated)
	require.Equal(t, lines[:3], kept)
	kept, truncated = firstMatches(lines, 2)
	require.False(t, truncated)
	require.Equal(t, lines, kept)
}

// The tool's schema offers Claude Code's names, so a model sees them.
func TestGrepSchemaHasClaudeCodesNames(t *testing.T) {
	t.Parallel()

	params := NewGrepTool(&mockPermissionService{}, t.TempDir(), config.ToolGrep{}).Info().Parameters
	for _, name := range []string{"-A", "-B", "-C", "-i", "glob", "type", "output_mode", "head_limit", "offset", "multiline"} {
		require.Contains(t, params, name)
	}
	require.Equal(t, "integer", params["head_limit"].(map[string]any)["type"], "head_limit is a number, though it may be left out")
}

// Grep shows the first 250 lines when head_limit does not say, as Claude
// Code's Grep does, and says it left the rest out.
func TestGrepShowsTwoHundredFiftyLinesByDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var b strings.Builder
	for i := range 300 {
		fmt.Fprintf(&b, "match %d\n", i)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o644))
	out := grepContent(t, dir, `{"pattern": "match"}`)
	require.Contains(t, out, "Found 300 matches\n")
	require.Equal(t, 250, strings.Count(out, "  Line "))
	require.True(t, strings.HasSuffix(out, "\n[Showing results with pagination = limit: 250]"), out[len(out)-100:])
}

// Without ripgrep, include picks the files ripgrep's --glob picks: a glob
// with no slash is matched against the whole file name, and one with a
// slash against the path from the search root, where * stops at a slash.
func TestSearchFilesWithRegexIncludeMatchesLikeRipgrep(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{"a.ts", "b.tsx", "main.go", "main.golden", "domain/x.txt", "src/a.go", "src/sub/b.go"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("hello\n"), 0o644))
	}

	for _, tc := range []struct {
		include string
		want    []string
	}{
		{"*.ts", []string{"a.ts"}},
		{"*.go", []string{"main.go", "src/a.go", "src/sub/b.go"}},
		{"main.*", []string{"main.go", "main.golden"}},
		{"src/*.go", []string{"src/a.go"}},
		{"**/*.go", []string{"main.go", "src/a.go", "src/sub/b.go"}},
		{"*.{ts,go}", []string{"a.ts", "main.go", "src/a.go", "src/sub/b.go"}},
	} {
		matches, err := searchFilesWithRegex("hello", dir, grepOptions{include: tc.include}, 100)
		require.NoError(t, err)
		var got []string
		for _, m := range matches {
			rel, err := filepath.Rel(dir, m.path)
			require.NoError(t, err)
			got = append(got, filepath.ToSlash(rel))
		}
		require.ElementsMatch(t, tc.want, got, "include %q", tc.include)
	}

	matches, err := searchFilesWithRegex("hello", filepath.Join(dir, "main.go"), grepOptions{include: "src/*.go"}, 100)
	require.NoError(t, err)
	require.Len(t, matches, 1)
}
