package tools

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"image"
	_ "image/gif"  // for unseenImage
	_ "image/jpeg" // for unseenImage
	_ "image/png"  // for unseenImage
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/dustin/go-humanize"
)

//go:embed view.md.tpl
var viewDescriptionTmpl []byte

var viewDescriptionTpl = template.Must(
	template.New("viewDescription").
		Parse(string(viewDescriptionTmpl)),
)

type viewDescriptionData struct {
	DefaultReadLimit int
	MaxReadTokens    int
}

func viewDescription() string {
	return renderTemplate(viewDescriptionTpl, viewDescriptionData{
		DefaultReadLimit: DefaultReadLimit,
		MaxReadTokens:    readTokens(),
	})
}

// readTokens is how many tokens one read returns at most:
// CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS, as in Claude Code, or
// MaxReadTokens.
func readTokens() int {
	return envInt("CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS", MaxReadTokens)
}

// readSizeLimit is how many bytes of the file at path one read returns at
// most: its tokens at Claude Code's estimate (bytesPerToken).
func readSizeLimit(path string) int {
	return readTokens() * bytesPerToken(path)
}

// bytesPerToken is Claude Code's estimate of the bytes a token of the file
// at path takes: 4, or 2 for JSON, which takes more tokens a byte.
func bytesPerToken(path string) int {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".jsonl", ".jsonc":
		return 2
	}
	return 4
}

type ViewParams struct {
	FilePath string `json:"file_path" description:"The path to the file to read"`
	Offset   int    `json:"offset,omitempty" description:"The line number to start reading from, the first line being 1"`
	Limit    int    `json:"limit,omitempty" description:"The number of lines to read (defaults to 2000)"`
	// AllowLarge reads a section over the usual limit, up to what still
	// fits in the context this turn (ContextRoom), as Claude Code's Read
	// takes allow_large.
	AllowLarge bool `json:"allow_large,omitempty" description:"Set to true to read a section that is over the usual size limit, up to what still fits in your context this turn. Only when you genuinely need all of it or the user asked for the whole file; otherwise read it in parts with offset and limit, or grep it."`
}

// Skipped is how many lines come before the first one read. Offset is a
// line number, as the lines are numbered and as Claude Code's Read takes
// it, so offset 120 starts at line 120; it used to count lines to skip,
// and a model that asked for the line a search had found started at the
// line after it.
func (p ViewParams) Skipped() int {
	return max(p.Offset-1, 0)
}

type ViewPermissionsParams struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

type ViewResourceType string

const (
	ViewResourceUnset ViewResourceType = ""
	ViewResourceSkill ViewResourceType = "skill"
)

type ViewResponseMetadata struct {
	FilePath            string           `json:"file_path"`
	Content             string           `json:"content"`
	ResourceType        ViewResourceType `json:"resource_type,omitempty"`
	ResourceName        string           `json:"resource_name,omitempty"`
	ResourceDescription string           `json:"resource_description,omitempty"`
	// Unchanged is set when the lines were not given again: the
	// session's context already holds them (see ViewedFiles).
	Unchanged bool `json:"unchanged,omitempty"`
}

const (
	ViewToolName = "view"
	// MaxReadTokens is how many tokens one read returns at most, as
	// Claude Code's Read: 25,000. At 200KB, about 50,000 tokens, one
	// read of a log took 40% of a local model's 128K context.
	MaxReadTokens = 25000
	// DefaultReadLimit is how many lines a view returns when no limit is
	// given: 2000, as Claude Code's Read tool does. At 200, an agent paged
	// through any real source file in several calls, each a model round
	// trip (tens of seconds for the AI box's local model).
	DefaultReadLimit = 2000
	MaxLineLength    = 2000
)

type contentTooLargeError struct {
	Size int
	Max  int
}

func (e contentTooLargeError) Error() string {
	return fmt.Sprintf("content section is too large (%d bytes). Maximum size is %d bytes", e.Size, e.Max)
}

// SubdirectoryContext returns, for a file a session views, writes or
// edits, the instructions of the subdirectories between the working
// directory and the file that it has not been given since its last
// summary, or "".
type SubdirectoryContext func(ctx context.Context, sessionID, filePath string) string

// FileRead is a view of Limit lines of a file after Skipped, and the
// file's state when it was made.
type FileRead struct {
	Path           string
	Skipped, Limit int
	ModTime        time.Time
	Size           int64
}

// ViewedFiles keeps the reads view gave each session that its context
// still holds, as Claude Code's Read keeps them, so that a read of the
// same lines of a file that has not changed since is answered with a
// note instead of the lines again.
type ViewedFiles interface {
	// StillGiven reports whether the session's context holds read: the
	// same lines of the file, as it is now.
	StillGiven(ctx context.Context, sessionID string, read FileRead) bool
	// Give records that view gave the session read.
	Give(ctx context.Context, sessionID string, read FileRead)
}

// fileUnchanged is Claude Code's answer to a read of what the agent's
// context already holds.
const fileUnchanged = "Wasted call \u2014 file unchanged since your last view of it. Refer to that earlier tool result instead."

// NewViewTool returns the view tool. subdirectoryContext, when not nil,
// adds what it returns to the file read; viewed, when not nil, keeps the
// reads it gives (see ViewedFiles).
func NewViewTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	filetracker filetracker.Service,
	skillTracker *skills.Tracker,
	subdirectoryContext SubdirectoryContext,
	viewed ViewedFiles,
	workingDir string,
	skillsPaths ...string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ViewToolName,
		viewDescription(),
		func(ctx context.Context, params ViewParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			// Handle builtin skill files (gnosh:// prefix, or klash:// or
			// crush:// from before the renames).
			if skills.IsBuiltinPath(params.FilePath) {
				params.FilePath = skills.CanonicalBuiltinPath(params.FilePath)
				resp, err := readBuiltinFile(params, skillTracker)
				return resp, err
			}

			// Handle relative paths
			filePath := filepathext.SmartJoin(workingDir, params.FilePath)
			// A screenshot's name has another space before AM or PM. The
			// name is settled before the check below, so the file read is
			// the file checked.
			if _, err := os.Stat(filePath); os.IsNotExist(err) {
				if other := otherScreenshotSpace(filePath); other != "" {
					if _, otherErr := os.Stat(other); otherErr == nil {
						filePath = other
					}
				}
			}

			// Check if file is outside working directory and request permission if needed
			absWorkingDir, err := filepath.Abs(workingDir)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error resolving working directory: %w", err)
			}

			absFilePath, err := filepath.Abs(filePath)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error resolving file path: %w", err)
			}

			// Outside is where the read goes: a link in the project that
			// leads out of it is asked about as the file it leads to.
			isOutsideWorkDir, realFilePath := fsext.LeadsOut(absWorkingDir, absFilePath)
			isSkillFile := isInSkillsPath(absFilePath, skillsPaths)

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for accessing files outside working directory")
			}

			// Request permission for files outside working directory, unless it's a skill file.
			if isOutsideWorkDir && !isSkillFile {
				granted, permReqErr := permissions.Request(
					ctx,
					permission.CreatePermissionRequest{
						SessionID:   sessionID,
						Path:        realFilePath,
						ToolCallID:  call.ID,
						ToolName:    ViewToolName,
						Action:      "read",
						Description: outsideRequest("Read file", absWorkingDir, absFilePath, realFilePath),
						Params:      ViewPermissionsParams{FilePath: params.FilePath, Offset: params.Offset, Limit: params.Limit},
					},
				)
				if permReqErr != nil {
					return fantasy.ToolResponse{}, permReqErr
				}
				if !granted {
					return NewPermissionDeniedResponse(), nil
				}
			}

			// Check if file exists
			fileInfo, err := os.Stat(filePath)
			if err != nil {
				if os.IsNotExist(err) {
					return fantasy.NewTextErrorResponse(missingFile(absFilePath, absWorkingDir)), nil
				}
				if denied := deniedPath(absFilePath); denied != "" {
					return fantasy.NewTextErrorResponse(denied), nil
				}
				return fantasy.ToolResponse{}, fmt.Errorf("error accessing file: %w", err)
			}

			// Check if it's a directory
			if fileInfo.IsDir() {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Path is a directory, not a file: %s", filePath)), nil
			}

			// Set default limit if not provided (no limit for SKILL.md files)
			if params.Limit <= 0 {
				if isSkillFile {
					params.Limit = 1000000 // Effectively no limit for skill files
				} else {
					params.Limit = DefaultReadLimit
				}
			}

			isSupportedImage, mimeType := getImageMimeType(filePath)
			if isSupportedImage {
				if !GetSupportsImagesFromContext(ctx) {
					return fantasy.NewTextErrorResponse(unseenImage(filePath, fileInfo.Size(), GetModelNameFromContext(ctx))), nil
				}
				if fileInfo.Size() > maxImageFileBytes {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Image file is too large (%d bytes). Maximum size is %d bytes",
						fileInfo.Size(), maxImageFileBytes)), nil
				}

				imageData, readErr := os.ReadFile(filePath)
				if readErr != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("error reading image file: %w", readErr)
				}

				// Some tools save files with a mismatched extension
				// (e.g. pinchtab writes JPEG bytes to a .png file).
				// Providers like Anthropic strictly validate the
				// media type against the base64 magic bytes and 400
				// on mismatch, so prefer the sniffed type whenever
				// it identifies a supported image format.
				mimeType = sniffImageMimeType(imageData, mimeType)

				imageData, mimeType, err = fitImage(imageData, mimeType)
				if errors.Is(err, errImageTooLarge) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Image file is too large (%d bytes) and could not be scaled down "+
						"to %d pixels a side and %d bytes", fileInfo.Size(), maxImageSide, maxImageBytes)), nil
				}
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("error scaling image: %w", err)
				}
				return fantasy.NewImageResponse(imageData, mimeType), nil
			}

			// Read the file content
			maxContentSize := readSizeLimit(filePath)
			tokenLimit := readTokens()
			// With allow_large a section may run to what still fits in
			// the context this turn, as in Claude Code; largeNote says
			// why it did not.
			var room *ContextRoom
			largeNote := ""
			if params.AllowLarge {
				room = ContextRoomFrom(ctx)
				switch {
				case room == nil:
					largeNote = " allow_large is not available where this read runs; read it in parts with offset and limit."
				case room.Left() > tokenLimit:
					tokenLimit = room.Left()
					maxContentSize = tokenLimit * bytesPerToken(filePath)
				default:
					room = nil
					largeNote = " Even with allow_large, this is more than fits in the context you have left this turn (other large reads in the same turn share that room)."
				}
			}
			if isSkillFile {
				maxContentSize = 0
			}
			skipped := params.Skipped()
			read := FileRead{Path: absFilePath, Skipped: skipped, Limit: params.Limit, ModTime: fileInfo.ModTime(), Size: fileInfo.Size()}
			if viewed != nil && viewed.StillGiven(ctx, sessionID, read) {
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse(fileUnchanged+getDiagnostics(filePath, lspManager)),
					ViewResponseMetadata{FilePath: filePath, Unchanged: true},
				), nil
			}
			content, hasMore, err := readTextFile(filePath, skipped, params.Limit, maxContentSize)
			if err != nil {
				var tooLarge contentTooLargeError
				if errors.As(err, &tooLarge) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Content section is too large (%d bytes). Maximum size is %d bytes",
						tooLarge.Size, tooLarge.Max)), nil
				}
				return fantasy.ToolResponse{}, fmt.Errorf("error reading file: %w", err)
			}
			if !utf8.ValidString(content) {
				return fantasy.NewTextErrorResponse("File content is not valid UTF-8"), nil
			}
			if room != nil {
				// What this read took is no longer there for the
				// step's other large reads.
				room.Claim(len(content) / bytesPerToken(filePath))
			}

			openInLSPs(ctx, lspManager, filePath)
			waitForLSPDiagnostics(ctx, lspManager, filePath, 300*time.Millisecond)
			output := "<file>\n"
			output += addLineNumbers(content, skipped+1)

			cutShort := false
			if hasMore {
				last := skipped + len(strings.Split(content, "\n"))
				if last-skipped < params.Limit {
					// Cut short at the size limit, not at the line limit.
					cutShort = true
					cutNote := largeNote
					switch {
					case !params.AllowLarge:
						cutNote = " To read it anyway, up to what still fits in your context, call view again with allow_large: true, " +
							"only if you genuinely need all of it or the user asked for the whole file."
					case cutNote == "":
						cutNote = " That is what allow_large had room for this turn."
					}
					output += fmt.Sprintf("\n\n(Stopped after line %d: one read returns at most %d tokens. To read on, use offset %d, "+
						"or grep for a specific section. Do NOT answer from this part alone if the answer may be further in the file.%s)",
						last, tokenLimit, last+1, cutNote)
				} else {
					output += fmt.Sprintf("\n\n(File has more lines. To read on, use offset %d)", last+1)
				}
			}
			output += "\n</file>\n"
			output += getDiagnostics(filePath, lspManager)
			if subdirectoryContext != nil {
				output += subdirectoryContext(ctx, sessionID, absFilePath)
			}
			filetracker.RecordRead(ctx, sessionID, filePath)
			// A read cut short is not all the lines asked for, so it is
			// not taken for them, as Claude Code takes no partial view.
			if viewed != nil && !cutShort {
				viewed.Give(ctx, sessionID, read)
			}

			meta := ViewResponseMetadata{
				FilePath: filePath,
				Content:  content,
			}
			if isSkillFile {
				if skill, err := skills.Parse(filePath); err == nil {
					meta.ResourceType = ViewResourceSkill
					meta.ResourceName = skill.Name
					meta.ResourceDescription = skill.Description
					skillTracker.MarkLoaded(skill.Name)
				}
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output),
				meta,
			), nil
		},
	)
}

// FileAsViewed is the start of the text file at path as the view tool
// gives it: at most limit lines and maxContentSize bytes, numbered, in a
// <file> block that says where to read on when there is more. An error
// says it is not a text file that can be read.
func FileAsViewed(path string, limit, maxContentSize int) (string, error) {
	content, hasMore, err := readTextFile(path, 0, limit, maxContentSize)
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(content) {
		return "", errors.New("file content is not valid UTF-8")
	}
	text := "<file>\n" + addLineNumbers(content, 1)
	if hasMore {
		text += fmt.Sprintf("\n\n(File has more lines. To read on, use offset %d)", len(strings.Split(content, "\n"))+1)
	}
	return text + "\n</file>\n", nil
}

func addLineNumbers(content string, startLine int) string {
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")

	var result []string
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")

		lineNum := i + startLine
		numStr := fmt.Sprintf("%d", lineNum)

		if len(numStr) >= 6 {
			result = append(result, fmt.Sprintf("%s|%s", numStr, line))
		} else {
			paddedNum := fmt.Sprintf("%6s", numStr)
			result = append(result, fmt.Sprintf("%s|%s", paddedNum, line))
		}
	}

	return strings.Join(result, "\n")
}

// readTextFile reads up to limit lines of filePath, starting after the
// first offset lines. When the next line would take the section past
// maxContentSize bytes (0 for no limit), it stops there and reports
// hasMore, so a section shorter than limit with hasMore set was cut at
// the size limit. Only a first line that does not fit on its own is an
// error.
// unseenImage says, for a model that cannot see images, what the image
// at path is: its format and dimensions when they can be read, and its
// size, and how to look into it anyway. An agent on the AI box checking
// rendered video frames tried to view them nine times, told only that it
// could not, before it measured their pixels in code instead.
func unseenImage(path string, size int64, modelName string) string {
	what := "an image"
	if f, err := os.Open(path); err == nil {
		config, format, err := image.DecodeConfig(f)
		_ = f.Close()
		if err == nil {
			what = fmt.Sprintf("a %s image of %dx%d pixels", strings.ToUpper(format), config.Width, config.Height)
		}
	}
	return fmt.Sprintf("This model (%s) cannot see images. %s is %s, %s. To check what it shows, "+
		"analyze its pixels in code (with Python's PIL or numpy, say) or ask a model that sees images.",
		modelName, path, what, humanize.Bytes(uint64(size)))
}

func readTextFile(filePath string, offset, limit, maxContentSize int) (string, bool, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", false, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	skipped := 0
	for skipped < offset {
		_, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return "", false, nil
			}
			return "", false, err
		}
		skipped++
	}

	lines := make([]string, 0, min(limit, DefaultReadLimit))
	contentSize := 0

	for len(lines) < limit {
		lineText, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", false, err
		}
		lineText = strings.TrimSuffix(lineText, "\n")
		lineText = strings.TrimSuffix(lineText, "\r")
		if len(lineText) > MaxLineLength {
			// Truncate at a rune boundary to avoid splitting
			// multi-byte characters.
			lineText = strings.ToValidUTF8(lineText[:MaxLineLength], "") + "..."
		}
		projectedSize := contentSize + len(lineText)
		if len(lines) > 0 {
			projectedSize++
		}
		if maxContentSize > 0 && projectedSize > maxContentSize {
			if len(lines) == 0 {
				return "", false, contentTooLargeError{Size: projectedSize, Max: maxContentSize}
			}
			// Return what fits, and say there is more: failing the
			// whole read would leave the agent nothing to go on.
			return strings.Join(lines, "\n"), true, nil
		}
		contentSize = projectedSize
		lines = append(lines, lineText)
		if err == io.EOF {
			break
		}
	}

	// Peek one more line only when we filled the limit.
	hasMore := false
	if len(lines) == limit {
		lineText, peekErr := reader.ReadString('\n')
		hasMore = len(lineText) > 0 || peekErr == nil
	}

	return strings.Join(lines, "\n"), hasMore, nil
}

func getImageMimeType(filePath string) (bool, string) {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".jpg", ".jpeg":
		return true, "image/jpeg"
	case ".png":
		return true, "image/png"
	case ".gif":
		return true, "image/gif"
	case ".webp":
		return true, "image/webp"
	default:
		return false, ""
	}
}

// sniffImageMimeType returns the content-sniffed MIME type when it identifies
// a supported image format. Otherwise it returns the provided fallback, which
// is usually the extension-derived type. Providers that validate the image
// media type against the base64 magic bytes (e.g. Anthropic) reject mismatched
// requests with a 400, so trusting the filename alone is unsafe.
func sniffImageMimeType(data []byte, fallback string) string {
	sniffed := http.DetectContentType(data)
	// http.DetectContentType may return the MIME with a ";" parameter
	// (e.g. "image/svg+xml; charset=utf-8") although current image sniffers
	// return bare types; strip defensively.
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = strings.TrimSpace(sniffed[:i])
	}
	switch sniffed {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return sniffed
	}
	return fallback
}

// isInSkillsPath checks if filePath is within any of the configured skills
// directories. Returns true for files that can be read without permission
// prompts and without size limits.
//
// Note that symlinks are resolved to prevent path traversal attacks via
// symbolic links.
func isInSkillsPath(filePath string, skillsPaths []string) bool {
	if len(skillsPaths) == 0 {
		return false
	}

	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return false
	}

	evalFilePath, err := filepath.EvalSymlinks(absFilePath)
	if err != nil {
		return false
	}

	for _, skillsPath := range skillsPaths {
		absSkillsPath, err := filepath.Abs(skillsPath)
		if err != nil {
			continue
		}

		evalSkillsPath, err := filepath.EvalSymlinks(absSkillsPath)
		if err != nil {
			continue
		}

		relPath, err := filepath.Rel(evalSkillsPath, evalFilePath)
		if err == nil && !strings.HasPrefix(relPath, "..") {
			return true
		}
	}

	return false
}

// readBuiltinFile reads a file from the embedded builtin skills filesystem.
func readBuiltinFile(params ViewParams, skillTracker *skills.Tracker) (fantasy.ToolResponse, error) {
	embeddedPath := "builtin/" + strings.TrimPrefix(params.FilePath, skills.BuiltinPrefix)
	builtinFS := skills.BuiltinFS()

	data, err := fs.ReadFile(builtinFS, embeddedPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Builtin file not found: %s", params.FilePath)), nil
	}

	content := string(data)
	if !utf8.ValidString(content) {
		return fantasy.NewTextErrorResponse("File content is not valid UTF-8"), nil
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 1000000 // Effectively no limit for skill files.
	}

	lines := strings.Split(content, "\n")
	skipped := min(params.Skipped(), len(lines))
	lines = lines[skipped:]

	hasMore := len(lines) > limit
	if hasMore {
		lines = lines[:limit]
	}

	output := "<file>\n"
	output += addLineNumbers(strings.Join(lines, "\n"), skipped+1)
	if hasMore {
		output += fmt.Sprintf("\n\n(File has more lines. To read on, use offset %d)", skipped+len(lines)+1)
	}
	output += "\n</file>\n"

	meta := ViewResponseMetadata{
		FilePath: params.FilePath,
		Content:  strings.Join(lines, "\n"),
	}
	if skill, err := skills.ParseContent(data); err == nil {
		meta.ResourceType = ViewResourceSkill
		meta.ResourceName = skill.Name
		meta.ResourceDescription = skill.Description
		skillTracker.MarkLoaded(skill.Name)
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(output),
		meta,
	), nil
}
