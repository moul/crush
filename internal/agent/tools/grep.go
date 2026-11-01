package tools

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/x/ansi"
)

// regexCache provides thread-safe caching of compiled regex patterns
type regexCache struct {
	*csync.Map[string, *regexp.Regexp]
}

// newRegexCache creates a new regex cache
func newRegexCache() *regexCache {
	return &regexCache{
		Map: csync.NewMap[string, *regexp.Regexp](),
	}
}

// get retrieves a compiled regex from cache or compiles and caches it
func (rc *regexCache) get(pattern string) (*regexp.Regexp, error) {
	re, ok := rc.Get(pattern)
	if ok && re != nil {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	rc.Set(pattern, re)
	return re, nil
}

// ResetCache clears compiled regex caches to prevent unbounded growth across sessions.
func ResetCache() {
	searchRegexCache.Reset(map[string]*regexp.Regexp{})
	globRegexCache.Reset(map[string]*regexp.Regexp{})
}

// Global regex cache instances
var (
	searchRegexCache = newRegexCache()
	globRegexCache   = newRegexCache()
	// Pre-compiled regex for glob conversion (used frequently)
	globBraceRegex = regexp.MustCompile(`\{([^}]+)\}`)
)

// GrepParams are the grep tool's parameters, with Claude Code's Grep
// names beside Gnosh's own: a model used to those passed glob, -i, -C or
// output_mode, and they were dropped without a word.
type GrepParams struct {
	Pattern      string `json:"pattern" description:"The regex pattern to search for in file contents"`
	Path         string `json:"path,omitempty" description:"The file or directory to search in. Defaults to the current working directory."`
	Include      string `json:"include,omitempty" description:"File pattern to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"`
	Glob         string `json:"glob,omitempty" description:"The same as include"`
	Type         string `json:"type,omitempty" description:"File type to search, as ripgrep's --type names it: go, js, py, rust, java, ..."`
	LiteralText  bool   `json:"literal_text,omitempty" description:"If true, the pattern will be treated as literal text with special regex characters escaped. Default is false."`
	IgnoreCase   bool   `json:"ignore_case,omitempty" description:"If true, letters match regardless of case, as grep -i. Default is false."`
	CaseFold     bool   `json:"-i,omitempty" description:"The same as ignore_case"`
	OutputMode   string `json:"output_mode,omitempty" description:"content (the default): the matching lines; files_with_matches: only the files that match; count: how many lines match in each file"`
	After        int    `json:"-A,omitempty" description:"Lines to show after each match, in content mode"`
	Before       int    `json:"-B,omitempty" description:"Lines to show before each match, in content mode"`
	ContextLines int    `json:"-C,omitempty" description:"Lines to show before and after each match, in content mode"`
	HeadLimit    *int   `json:"head_limit,omitempty" description:"Limit output to the first N lines or entries, as | head -N: output lines in content mode, file paths in files_with_matches mode, count entries in count mode. Defaults to 250. Pass 0 for unlimited (use sparingly: large result sets waste context)."`
	Offset       int    `json:"offset,omitempty" description:"Skip the first N lines or entries before applying head_limit, as | tail -n +N | head -N. Defaults to 0."`
	Multiline    bool   `json:"multiline,omitempty" description:"If true, . matches newlines and a pattern can span lines. Default is false."`
}

// Grep's output modes, by Claude Code's names.
const (
	grepOutputContent = "content"
	grepOutputFiles   = "files_with_matches"
	grepOutputCount   = "count"
)

// grepOptions are a search's options beyond its pattern and path.
type grepOptions struct {
	include       string
	fileType      string
	before, after int
	multiline     bool
}

type grepMatch struct {
	path     string
	modTime  time.Time
	lineNum  int
	charNum  int
	lineText string
	// context marks a line shown around a match (-A, -B, -C), not one.
	context bool
}

type GrepResponseMetadata struct {
	NumberOfMatches int  `json:"number_of_matches"`
	Truncated       bool `json:"truncated"`
}

const (
	GrepToolName        = "grep"
	maxGrepContentWidth = 500
)

//go:embed grep.md.tpl
var grepDescriptionTmpl []byte

var grepDescriptionTpl = template.Must(
	template.New("grepDescription").
		Parse(string(grepDescriptionTmpl)),
)

type grepDescriptionData struct {
	MaxResults int
}

func grepDescription() string {
	return renderTemplate(grepDescriptionTpl, grepDescriptionData{
		MaxResults: grepDefaultLimit,
	})
}

// grepDefaultLimit is how many lines or entries grep shows when head_limit
// does not say, as Claude Code's Grep; grepSearchLimit is how many matches
// a search keeps, for head_limit and offset to page through.
const (
	grepDefaultLimit = 250
	grepSearchLimit  = 10000
)

// page is entries from offset on, at most limit of them (0: no limit),
// and Claude Code's account of the paging when it left any out: "limit:
// 250", with ", offset: 10" after skipping some.
func page[T any](entries []T, offset, limit int) ([]T, string) {
	var said []string
	entries = entries[min(offset, len(entries)):]
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
		said = append(said, fmt.Sprintf("limit: %d", limit))
	}
	if offset > 0 {
		said = append(said, fmt.Sprintf("offset: %d", offset))
	}
	return entries, strings.Join(said, ", ")
}

// escapeRegexPattern escapes special regex characters so they're treated as literal characters
func escapeRegexPattern(pattern string) string {
	specialChars := []string{"\\", ".", "+", "*", "?", "(", ")", "[", "]", "{", "}", "^", "$", "|"}
	escaped := pattern

	for _, char := range specialChars {
		escaped = strings.ReplaceAll(escaped, char, "\\"+char)
	}

	return escaped
}

// regexOperatorHint explains an empty result whose pattern holds unescaped
// brackets: the usual cause is a search for code as written, such as
// `func (s *Server)`, where the regex reads the parentheses as a group and
// matches something else. A bare "No files found" there reads as "no such
// code", and a model moves on believing it.
func regexOperatorHint(pattern string) string {
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++ // an escaped character is already literal
		case '(', ')', '[', ']', '{', '}':
			return "\n\nThe pattern is a regular expression, where ( ) [ ] { } are operators, not text. " +
				"To match them as written, escape them with a backslash or set literal_text to true."
		}
	}
	return ""
}

func NewGrepTool(permissions permission.Service, workingDir string, config config.ToolGrep) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GrepToolName,
		grepDescription(),
		func(ctx context.Context, params GrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}

			searchPattern := params.Pattern
			if params.LiteralText {
				searchPattern = escapeRegexPattern(params.Pattern)
			}
			if params.IgnoreCase || params.CaseFold {
				// An inline flag, which ripgrep and Go's regexp both read.
				searchPattern = "(?i)" + searchPattern
			}
			mode := cmp.Or(params.OutputMode, grepOutputContent)
			if mode != grepOutputContent && mode != grepOutputFiles && mode != grepOutputCount {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("output_mode %q: expected content, files_with_matches or count", params.OutputMode)), nil
			}
			opts := grepOptions{
				include:   cmp.Or(params.Include, params.Glob),
				fileType:  params.Type,
				multiline: params.Multiline,
			}
			if mode == grepOutputContent {
				opts.before = cmp.Or(params.Before, params.ContextLines)
				opts.after = cmp.Or(params.After, params.ContextLines)
			}
			// Lines and entries are shown head_limit at a time, from
			// offset, after the search, as Claude Code's Grep shows them.
			limit := grepDefaultLimit
			if params.HeadLimit != nil {
				limit = *params.HeadLimit
			}
			if limit < 0 {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("head_limit must be a whole number of 0 or more, got %d. Pass 0 for unlimited.", limit)), nil
			}
			if params.Offset < 0 {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("offset must be a whole number of 0 or more, got %d.", params.Offset)), nil
			}
			searchLimit := grepSearchLimit

			// A relative path is the tool's, as view's is, not the
			// process's: they differ for the fetch agent, whose tools work
			// in a temporary directory.
			searchPath := filepathext.SmartJoin(workingDir, params.Path)
			if resp, stop, err := askToSearch(ctx, permissions, call, GrepToolName, searchPath, workingDir, params); stop {
				return resp, err
			}
			if msg := missingSearchPath(params.Path, searchPath, workingDir); msg != "" {
				return fantasy.NewTextErrorResponse(msg), nil
			}

			searchCtx, cancel := context.WithTimeout(ctx, config.GetTimeout())
			defer cancel()

			matches, truncated, err := searchFiles(searchCtx, searchPattern, searchPath, opts, searchLimit)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error searching files: %v", err)), nil
			}

			var output strings.Builder
			var paging string
			found := countMatches(matches)
			switch {
			case found == 0:
				output.WriteString("No files found")
				if !params.LiteralText {
					output.WriteString(regexOperatorHint(params.Pattern))
				}
			case mode == grepOutputContent:
				var shown []grepMatch
				shown, paging = page(matches, params.Offset, limit)
				if len(shown) == 0 {
					output.WriteString("No entries at this offset")
				} else {
					writeGrepLines(&output, shown, found, opts.before > 0 || opts.after > 0)
				}
			default:
				paging = writeGrepFiles(&output, matches, mode == grepOutputCount, params.Offset, limit)
			}
			if paging != "" {
				fmt.Fprintf(&output, "\n[Showing results with pagination = %s]", paging)
			}
			if found > 0 && truncated {
				fmt.Fprintf(&output, "\n(The search stopped at %d matches. Consider using a more specific path or pattern.)", grepSearchLimit)
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output.String()),
				GrepResponseMetadata{
					NumberOfMatches: found,
					Truncated:       truncated,
				},
			), nil
		},
	)
}

// countMatches is how many of lines are matches, not context.
func countMatches(lines []grepMatch) int {
	n := 0
	for _, l := range lines {
		if !l.context {
			n++
		}
	}
	return n
}

// writeGrepLines writes lines, matching lines and the context lines around
// them, grouped by file, under how many matches were found in all. With
// context, "--" separates the groups of a file that are not next to each
// other, as grep's does.
func writeGrepLines(output *strings.Builder, lines []grepMatch, found int, withContext bool) {
	fmt.Fprintf(output, "Found %d matches\n", found)
	var prev *grepMatch
	for i := range lines {
		line := &lines[i]
		if prev == nil || prev.path != line.path {
			if prev != nil {
				output.WriteString("\n")
			}
			fmt.Fprintf(output, "%s:\n", filepath.ToSlash(line.path))
		} else if withContext && !follows(*prev, *line) {
			output.WriteString("  --\n")
		}
		prev = line
		if line.lineNum <= 0 {
			fmt.Fprintf(output, "  %s\n", line.path)
			continue
		}
		// A multiline match spans lines; each gets its own.
		for j, text := range strings.Split(line.lineText, "\n") {
			if ansi.StringWidth(text) > maxGrepContentWidth {
				text = ansi.Truncate(text, maxGrepContentWidth, "...")
			}
			switch {
			case line.context:
				fmt.Fprintf(output, "  Line %d- %s\n", line.lineNum+j, text)
			case j == 0 && line.charNum > 0:
				fmt.Fprintf(output, "  Line %d, Char %d: %s\n", line.lineNum, line.charNum, text)
			default:
				fmt.Fprintf(output, "  Line %d: %s\n", line.lineNum+j, text)
			}
		}
	}
}

// writeGrepFiles writes the files with matches, with how many lines in
// each match when counts is set, from offset on and at most limit of them
// (0: no limit), and returns Claude Code's account of the paging when it
// left any out (see page).
func writeGrepFiles(output *strings.Builder, lines []grepMatch, counts bool, offset, limit int) string {
	var files []string
	perFile := map[string]int{}
	for _, l := range lines {
		if l.context {
			continue
		}
		if perFile[l.path] == 0 {
			files = append(files, l.path)
		}
		perFile[l.path]++
	}
	if counts {
		fmt.Fprintf(output, "Found %d matches in %d files\n", countMatches(lines), len(perFile))
	} else {
		fmt.Fprintf(output, "Found %d files\n", len(perFile))
	}
	shown, paging := page(files, offset, limit)
	if len(shown) == 0 {
		output.WriteString("No entries at this offset\n")
	}
	for _, f := range shown {
		if counts {
			fmt.Fprintf(output, "%s: %d\n", filepath.ToSlash(f), perFile[f])
		} else {
			fmt.Fprintf(output, "%s\n", filepath.ToSlash(f))
		}
	}
	return paging
}

func searchFiles(ctx context.Context, pattern, rootPath string, opts grepOptions, limit int) ([]grepMatch, bool, error) {
	// A path that does not exist is an error, not an empty result: neither
	// engine says so plainly (ripgrep reports it as a failed search, the Go
	// walker not at all).
	if _, err := os.Stat(rootPath); err != nil {
		return nil, false, err
	}
	matches, err := searchWithRipgrep(ctx, pattern, rootPath, opts)
	if err != nil {
		// Fall back to the Go walker only when ripgrep is not installed.
		// A search that ran out of time must not continue on unbounded, and
		// other ripgrep failures (a rejected pattern, a missing path) must
		// be reported rather than retried with a slower engine that fails
		// for different reasons.
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("search timed out; try a narrower path or pattern: %w", ctx.Err())
		}
		if errors.Is(err, exec.ErrNotFound) {
			if opts.fileType != "" || opts.multiline {
				return nil, false, errors.New("type and multiline need ripgrep (rg), which is not installed; use include for the files to search")
			}
			matches, err = searchFilesWithRegex(pattern, rootPath, opts, limit)
		}
		if err != nil {
			return nil, false, err
		}
	}

	// Use a stable sort so that the multiple matches a single file can
	// contribute (all sharing the same modTime) keep their original
	// line order and stay grouped together in the rendered output.
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].modTime.After(matches[j].modTime)
	})

	matches, truncated := firstMatches(matches, limit)
	return matches, truncated, nil
}

// firstMatches keeps the first limit matches of lines and the context
// lines that follow them, and reports whether there were more.
func firstMatches(lines []grepMatch, limit int) ([]grepMatch, bool) {
	n := 0
	for i, l := range lines {
		if l.context {
			continue
		}
		if n < limit {
			n++
			continue
		}
		// Keep the lines that run on from the last match kept; context
		// after a gap is the dropped match's.
		end := i
		for end > 0 && lines[end-1].context {
			end--
		}
		for end < i && follows(lines[end-1], lines[end]) {
			end++
		}
		return lines[:end], true
	}
	return lines, false
}

// follows reports whether line comes right after prev in the same file.
func follows(prev, line grepMatch) bool {
	return prev.path == line.path && line.lineNum == prev.lineNum+strings.Count(prev.lineText, "\n")+1
}

func searchWithRipgrep(ctx context.Context, pattern, path string, opts grepOptions) ([]grepMatch, error) {
	cmd := getRgSearchCmd(ctx, pattern, path, opts)
	if cmd == nil {
		return nil, fmt.Errorf("%w: ripgrep not found in $PATH", exec.ErrNotFound)
	}

	// Only add ignore files if they exist
	for _, ignoreFile := range []string{".gitignore", ".gnoshignore", ".klashignore", ".crushignore"} {
		ignorePath := filepath.Join(path, ignoreFile)
		if _, err := os.Stat(ignorePath); err == nil {
			cmd.Args = append(cmd.Args, "--ignore-file", ignorePath)
		}
	}

	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return nil, err
		}
		// Exit 1 means no match. Exit 2 means an error occurred, which
		// includes a file that could not be read during a search that
		// otherwise ran: its output then ends with a summary, and the
		// matches it found are the answer, as they were when the Go walker,
		// which skips such files, stood in for every failure. With no
		// output at all the search never ran (a pattern it rejects, say),
		// and its message is the error.
		if exitErr.ExitCode() == 1 || (exitErr.ExitCode() == 2 && len(bytes.TrimSpace(output)) > 0) {
			return parseRipgrepMatches(output), nil
		}
		if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
			return nil, fmt.Errorf("ripgrep: %s", ansi.Truncate(msg, 1000, "…"))
		}
		return nil, err
	}
	return parseRipgrepMatches(output), nil
}

// parseRipgrepMatches reads the matches out of ripgrep's --json output,
// keeping the first match on each line, and the context lines around
// them in order.
func parseRipgrepMatches(output []byte) []grepMatch {
	var matches []grepMatch
	for line := range bytes.SplitSeq(bytes.TrimSpace(output), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var match ripgrepMatch
		if err := json.Unmarshal(line, &match); err != nil {
			continue
		}
		if match.Type == "context" {
			fi, err := os.Stat(match.Data.Path.Text)
			if err != nil {
				continue
			}
			matches = append(matches, grepMatch{
				path:     match.Data.Path.Text,
				modTime:  fi.ModTime(),
				lineNum:  match.Data.LineNumber,
				lineText: strings.TrimRight(match.Data.Lines.Text, "\r\n"),
				context:  true,
			})
			continue
		}
		if match.Type != "match" {
			continue
		}
		for _, m := range match.Data.Submatches {
			fi, err := os.Stat(match.Data.Path.Text)
			if err != nil {
				continue // Skip files we can't access
			}
			matches = append(matches, grepMatch{
				path:     match.Data.Path.Text,
				modTime:  fi.ModTime(),
				lineNum:  match.Data.LineNumber,
				charNum:  m.Start + 1, // ensure 1-based
				lineText: strings.TrimSpace(match.Data.Lines.Text),
			})
			// only get the first match of each line
			break
		}
	}
	return matches
}

type ripgrepMatch struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}

func searchFilesWithRegex(pattern, rootPath string, opts grepOptions, limit int) ([]grepMatch, error) {
	matches := []grepMatch{}
	found := 0

	// Use cached regex compilation
	regex, err := searchRegexCache.get(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	var includePattern *regexp.Regexp
	if opts.include != "" {
		regexPattern := globToRegex(opts.include)
		includePattern, err = globRegexCache.get(regexPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid include pattern: %w", err)
		}
	}

	// Create walker with gitignore and crushignore support
	walker := fsext.NewFastGlobWalker(rootPath)

	err = filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		if info.IsDir() {
			// Check if directory should be skipped
			if walker.ShouldSkip(path) {
				return filepath.SkipDir
			}
			return nil // Continue into directory
		}

		// Use walker's shouldSkip method for files
		if walker.ShouldSkip(path) {
			return nil
		}

		// A symlink met on the way is not searched, as ripgrep searches
		// none it meets: one in the project could lead to ~/.ssh. (The
		// path searched, when it is a link, was asked about as where it
		// leads.)
		if path != rootPath && info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		// Skip hidden files (starting with a dot) to match ripgrep's default behavior
		base := filepath.Base(path)
		if base != "." && strings.HasPrefix(base, ".") {
			return nil
		}

		if includePattern != nil && !includePattern.MatchString(path) {
			return nil
		}

		lineMatches, err := fileMatches(path, regex, opts.before, opts.after)
		if err != nil {
			return nil // Skip files we can't read
		}

		for _, lm := range lineMatches {
			matches = append(matches, grepMatch{
				path:     path,
				modTime:  info.ModTime(),
				lineNum:  lm.lineNum,
				charNum:  lm.charNum,
				lineText: lm.lineText,
				context:  lm.context,
			})
			if !lm.context {
				found++
			}
			if found > limit {
				return filepath.SkipAll
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return matches, nil
}

// lineMatch is a single matching line within a file: its 1-based line
// number, the 1-based column of the first match on that line, and the
// line text (with the trailing newline stripped). context marks a line
// shown around a match instead.
type lineMatch struct {
	lineNum  int
	charNum  int
	lineText string
	context  bool
}

// fileMatches returns every line in filePath that matches pattern, and
// the before and after lines around each, in order. Like ripgrep, it
// reports one entry per matching line (using the first match on the line
// for the column) instead of stopping at the first match in the file.
func fileMatches(filePath string, pattern *regexp.Regexp, before, after int) ([]lineMatch, error) {
	if pattern == nil {
		return nil, nil
	}
	// Only search text files.
	if !isTextFile(filePath) {
		return nil, nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	var found []lineMatch
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadString('\n')
		if line == "" && err == io.EOF {
			// The end of a file whose last line ends with a newline: no
			// line follows it.
			break
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		lines = append(lines, line)
		if loc := pattern.FindStringIndex(line); loc != nil {
			found = append(found, lineMatch{
				lineNum:  len(lines),
				charNum:  loc[0] + 1,
				lineText: line,
			})
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if before == 0 && after == 0 {
		return found, nil
	}

	// Each match with its context, a line shown once however many
	// matches it is near.
	var out []lineMatch
	next := 1 // the first line not yet shown
	for i, m := range found {
		for n := max(next, m.lineNum-before); n < m.lineNum; n++ {
			out = append(out, lineMatch{lineNum: n, lineText: lines[n-1], context: true})
		}
		out = append(out, m)
		next = m.lineNum + 1
		end := min(len(lines), m.lineNum+after)
		if i+1 < len(found) {
			end = min(end, found[i+1].lineNum-1)
		}
		for ; next <= end; next++ {
			out = append(out, lineMatch{lineNum: next, lineText: lines[next-1], context: true})
		}
	}
	return out, nil
}

// isTextFile checks if a file is a text file by examining its MIME type.
func isTextFile(filePath string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer file.Close()

	// Read first 512 bytes for MIME type detection.
	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return false
	}

	// Detect content type.
	contentType := http.DetectContentType(buffer[:n])

	// Check if it's a text MIME type.
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml" ||
		contentType == "application/javascript" ||
		contentType == "application/x-sh"
}

func globToRegex(glob string) string {
	regexPattern := strings.ReplaceAll(glob, ".", "\\.")
	regexPattern = strings.ReplaceAll(regexPattern, "*", ".*")
	regexPattern = strings.ReplaceAll(regexPattern, "?", ".")

	// Use pre-compiled regex instead of compiling each time
	regexPattern = globBraceRegex.ReplaceAllStringFunc(regexPattern, func(match string) string {
		inner := match[1 : len(match)-1]
		return "(" + strings.ReplaceAll(inner, ",", "|") + ")"
	})

	return regexPattern
}
