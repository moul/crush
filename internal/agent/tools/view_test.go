package tools

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/dustin/go-humanize"
	"github.com/stretchr/testify/require"
)

func TestReadTextFileBoundaryCases(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.txt")

	var allLines []string
	for i := range 5 {
		allLines = append(allLines, fmt.Sprintf("line %d", i+1))
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(allLines, "\n")), 0o644))

	tests := []struct {
		name        string
		offset      int
		limit       int
		wantContent string
		wantHasMore bool
	}{
		{
			name:        "exactly limit lines remaining",
			offset:      0,
			limit:       5,
			wantContent: "line 1\nline 2\nline 3\nline 4\nline 5",
			wantHasMore: false,
		},
		{
			name:        "limit plus one line remaining",
			offset:      0,
			limit:       4,
			wantContent: "line 1\nline 2\nline 3\nline 4",
			wantHasMore: true,
		},
		{
			name:        "offset at last line",
			offset:      4,
			limit:       3,
			wantContent: "line 5",
			wantHasMore: false,
		},
		{
			name:        "offset beyond eof",
			offset:      10,
			limit:       3,
			wantContent: "",
			wantHasMore: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotContent, gotHasMore, err := readTextFile(filePath, tt.offset, tt.limit, 0)
			require.NoError(t, err)
			require.Equal(t, tt.wantContent, gotContent)
			require.Equal(t, tt.wantHasMore, gotHasMore)
		})
	}
}

func TestReadTextFileTruncatesLongLines(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "longline.txt")

	longLine := strings.Repeat("a", MaxLineLength+10)
	require.NoError(t, os.WriteFile(filePath, []byte(longLine), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 1, 0)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Equal(t, strings.Repeat("a", MaxLineLength)+"...", content)
}

func TestReadTextFileLineExceeding1MB(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "huge_line.txt")

	hugeLine := strings.Repeat("A", 2*1024*1024) // 2MB — exceeds bufio.Scanner max
	require.NoError(t, os.WriteFile(filePath, []byte(hugeLine), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 1, 0)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Equal(t, strings.Repeat("A", MaxLineLength)+"...", content)
}

func TestViewToolAllowsSmallSectionsOfLargeFiles(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "large.txt")
	lines := []string{strings.Repeat("a", readSizeLimit(filePath)+1), "target line", "after target"}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{
		FilePath: filePath,
		Offset:   2,
		Limit:    1,
	})

	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "     2|target line")
	require.NotContains(t, resp.Content, "File is too large")

	var meta ViewResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "target line", meta.Content)
}

// A section past the size limit is cut there, not refused: with the
// 2000-line default a log of wide lines passes 25,000 tokens long before
// its last line, and failing the whole read left the agent nothing.
func TestViewToolCutsOversizedSectionsAtTheSizeLimit(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "large-section.txt")
	lines := make([]string, DefaultReadLimit)
	for i := range lines {
		lines[i] = strings.Repeat("a", MaxLineLength)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{
		FilePath: filePath,
	})

	require.False(t, resp.IsError, resp.Content)
	var meta ViewResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.LessOrEqual(t, len(meta.Content), readSizeLimit(filePath))
	shown := strings.Count(meta.Content, "\n") + 1
	require.Less(t, shown, DefaultReadLimit)
	require.Contains(t, resp.Content, fmt.Sprintf("Stopped after line %d", shown))
	require.Contains(t, resp.Content, fmt.Sprintf("To read on, use offset %d", shown+1))

	// Reading on from there picks up the next line.
	resp = runViewTool(t, tool, ctx, ViewParams{FilePath: filePath, Offset: shown + 1, Limit: 1})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, fmt.Sprintf("%6d|aaa", shown+1))
}

// Offset is the number of the line to start at, as Claude Code's Read
// takes it: a model that viewed from the line a search had found got the
// line after it. 0 and 1 both start at the top, and the hint at the end
// names the offset that reads on.
func TestViewToolOffsetIsALineNumber(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "lines.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("one\ntwo\nthree\nfour\n"), 0o644))
	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath, Offset: 3, Limit: 1})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "     3|three")
	require.NotContains(t, resp.Content, "two")
	require.Contains(t, resp.Content, "To read on, use offset 4")

	resp = runViewTool(t, tool, ctx, ViewParams{FilePath: filePath, Offset: 4, Limit: 1})
	require.Contains(t, resp.Content, "     4|four")

	for _, offset := range []int{0, 1} {
		resp = runViewTool(t, tool, ctx, ViewParams{FilePath: filePath, Offset: offset, Limit: 1})
		require.Contains(t, resp.Content, "     1|one", "offset %d", offset)
	}
}

// A file that ends with a newline has no line after that newline. The
// read used to hand the model one more line than the file has, numbered
// and empty, so a model counting a file's lines, or appending to its
// end, was one line out.
func TestViewToolNumbersNoLineAfterAFinalNewline(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "lines.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("one\ntwo\nthree\n"), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "     3|three")
	require.NotContains(t, resp.Content, "     4|")

	// The file still comes back as its bytes, final newline included.
	var meta ViewResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "one\ntwo\nthree\n", meta.Content)
}

// A file whose last line ends exactly at the read's byte limit is read
// whole. The empty line a final newline used to add took the section one
// byte past the limit, so the read was reported as cut short and the
// model was told to read on from a line that is not there.
func TestViewToolReadsAFileThatEndsAtTheSizeLimitWhole(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "exact.txt")

	const lines = 1000
	section := make([]string, lines)
	for i := range section {
		section[i] = strings.Repeat("a", 99)
	}
	// The section the read builds is the lines plus one newline between
	// each; pad the last line so that it is exactly the byte limit.
	size := (len(section[0])+1)*lines - 1
	section[lines-1] += strings.Repeat("a", readSizeLimit(filePath)-size)
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(section, "\n")+"\n"), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, fmt.Sprintf("%6d|aaa", lines))
	require.Falsef(t, strings.Contains(resp.Content, "Stopped after line"),
		"the whole file was read, but the read reports it cut short")
	require.Falsef(t, strings.Contains(resp.Content, "To read on, use offset"),
		"the whole file was read, but the read sends the model past its end")
}

// An image that cannot be scaled down to fit, as one gnosh cannot
// decode, is refused when it is over what goes to the model; a file too
// big to be read at all is refused unread.
func TestViewToolBlocksOversizedImages(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "large.png")
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Repeat("a", maxImageBytes+1)), 0o644))
	huge := filepath.Join(workingDir, "huge.png")
	f, err := os.Create(huge)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxImageFileBytes+1))
	require.NoError(t, f.Close())

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	ctx = context.WithValue(ctx, SupportsImagesContextKey, true)
	resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath})
	require.True(t, resp.IsError)
	require.Equal(t, fmt.Sprintf("Image file is too large (%d bytes) and could not be scaled down to 2000 pixels a side and %d bytes", maxImageBytes+1, maxImageBytes), resp.Content)

	resp = runViewTool(t, tool, ctx, ViewParams{FilePath: huge})
	require.True(t, resp.IsError)
	require.Equal(t, fmt.Sprintf("Image file is too large (%d bytes). Maximum size is %d bytes", maxImageFileBytes+1, maxImageFileBytes), resp.Content)
}

func TestReadTextFileEnforcesMaxContentSize(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "oversized.txt")
	lines := []string{
		strings.Repeat("a", MaxLineLength),
		strings.Repeat("b", MaxLineLength),
		"target line",
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	// Room for the first line only: the section stops after it, and
	// says there is more.
	content, hasMore, err := readTextFile(filePath, 0, len(lines), MaxLineLength)
	require.NoError(t, err)
	require.Equal(t, lines[0], content)
	require.True(t, hasMore)

	// A first line that does not fit on its own is the only error.
	content, hasMore, err = readTextFile(filePath, 0, len(lines), MaxLineLength-1)
	require.ErrorAs(t, err, &contentTooLargeError{})
	require.Empty(t, content)
	require.False(t, hasMore)

	content, hasMore, err = readTextFile(filePath, 2, 1, MaxLineLength)
	require.NoError(t, err)
	require.Equal(t, "target line", content)
	require.False(t, hasMore)
}

func TestReadTextFileAllowsExactMaxContentSize(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "exact-size.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("abcd\nefgh"), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 2, len("abcd\nefgh"))
	require.NoError(t, err)
	require.Equal(t, "abcd\nefgh", content)
	require.False(t, hasMore)
}

type mockViewPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
}

func (m *mockViewPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockViewPermissionService) Grant(req permission.PermissionRequest) bool { return true }

func (m *mockViewPermissionService) Deny(req permission.PermissionRequest) bool { return true }

func (m *mockViewPermissionService) GrantPersistent(req permission.PermissionRequest) bool {
	return true
}

func (m *mockViewPermissionService) AutoApproveSession(sessionID string) {}

func (m *mockViewPermissionService) IsSessionAutoApproved(string) bool { return false }

func (m *mockViewPermissionService) ActiveRequest() (permission.PermissionRequest, bool) {
	return permission.PermissionRequest{}, false
}

func (m *mockViewPermissionService) SetSkipRequests(skip bool) {}

func (m *mockViewPermissionService) AddDir(string) {}

func (m *mockViewPermissionService) SkipRequests() bool {
	return false
}

func (m *mockViewPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

type mockFileTracker struct{}

func (m mockFileTracker) RecordRead(ctx context.Context, sessionID, path string) {}

func (m mockFileTracker) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return time.Time{}
}

func (m mockFileTracker) UnchangedSinceRead(ctx context.Context, sessionID, path string) bool {
	return false
}

func (m mockFileTracker) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return nil, nil
}

func (m mockFileTracker) CopyReads(ctx context.Context, from, to string) error {
	return nil
}

func (m mockFileTracker) ForgetReadsSince(ctx context.Context, sessionID string, since int64) error {
	return nil
}

func (m mockFileTracker) ForgetReads(ctx context.Context, sessionID string, paths []string) error {
	return nil
}

func newViewToolForTest(workingDir string) fantasy.AgentTool {
	permissions := &mockViewPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	return NewViewTool(nil, permissions, mockFileTracker{}, nil, nil, nil, workingDir)
}

func runViewTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params ViewParams) fantasy.ToolResponse {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  ViewToolName,
		Input: string(input),
	}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err)
	return resp
}

var _ filetracker.Service = mockFileTracker{}

func TestReadBuiltinFile(t *testing.T) {
	t.Parallel()

	t.Run("reads a builtin skill under its name from before the rename", func(t *testing.T) {
		t.Parallel()

		resp := runViewTool(t, newViewToolForTest(t.TempDir()), context.WithValue(context.Background(), SessionIDContextKey, "test-session"), ViewParams{
			FilePath: "crush://skills/crush-config/SKILL.md",
		})
		require.False(t, resp.IsError, resp.Content)
		require.Contains(t, resp.Content, "Gnosh Configuration")
	})

	t.Run("reads gnosh-config skill", func(t *testing.T) {
		t.Parallel()

		resp, err := readBuiltinFile(ViewParams{
			FilePath: "gnosh://skills/gnosh-config/SKILL.md",
		}, nil)
		require.NoError(t, err)
		require.NotEmpty(t, resp.Content)
		require.Contains(t, resp.Content, "Gnosh Configuration")
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		resp, err := readBuiltinFile(ViewParams{
			FilePath: "gnosh://skills/nonexistent/SKILL.md",
		}, nil)
		require.NoError(t, err)
		require.True(t, resp.IsError)
	})

	t.Run("metadata has skill info", func(t *testing.T) {
		t.Parallel()

		resp, err := readBuiltinFile(ViewParams{
			FilePath: "gnosh://skills/gnosh-config/SKILL.md",
		}, nil)
		require.NoError(t, err)

		var meta ViewResponseMetadata
		require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
		require.Equal(t, ViewResourceSkill, meta.ResourceType)
		require.Equal(t, "gnosh-config", meta.ResourceName)
		require.NotEmpty(t, meta.ResourceDescription)
	})

	t.Run("respects offset", func(t *testing.T) {
		t.Parallel()

		resp, err := readBuiltinFile(ViewParams{
			FilePath: "gnosh://skills/gnosh-config/SKILL.md",
			Offset:   5,
			Limit:    2,
		}, nil)
		require.NoError(t, err)
		require.NotContains(t, resp.Content, "     4|")
		require.Contains(t, resp.Content, "     5|")
		require.Contains(t, resp.Content, "To read on, use offset 7")
	})
}

func TestSniffImageMimeType(t *testing.T) {
	t.Parallel()

	jpegMagic := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	gifMagic := []byte("GIF89a")
	// Minimal RIFF/WEBP header.
	webpMagic := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	random := []byte("not an image at all, just text")

	cases := []struct {
		name     string
		data     []byte
		fallback string
		want     string
	}{
		{"jpeg bytes in .png file uses sniffed", jpegMagic, "image/png", "image/jpeg"},
		{"png bytes in .jpg file uses sniffed", pngMagic, "image/jpeg", "image/png"},
		{"gif bytes uses sniffed", gifMagic, "image/png", "image/gif"},
		{"webp bytes uses sniffed", webpMagic, "image/png", "image/webp"},
		{"matching extension and content keeps sniffed", pngMagic, "image/png", "image/png"},
		{"unsniffable content falls back", random, "image/png", "image/png"},
		{"empty content falls back", nil, "image/jpeg", "image/jpeg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, sniffImageMimeType(tc.data, tc.fallback))
		})
	}
}

// A source file of ordinary length comes back whole from one view, as from
// Claude Code's Read, instead of in pages.
func TestViewToolReturnsAnOrdinaryFileWhole(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "main.go")
	lines := make([]string, 600)
	for i := range lines {
		lines[i] = fmt.Sprintf("// line %d", i+1)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "// line 600")
	require.NotContains(t, resp.Content, "File has more lines")
}

// What the subdirectory context gives for a file goes after the file read.
func TestViewToolAddsTheSubdirectoryContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	require.NoError(t, os.WriteFile(path, []byte("package a\n"), 0o644))
	var asked string
	permissions := &mockViewPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	tool := NewViewTool(nil, permissions, mockFileTracker{}, nil, func(_ context.Context, sessionID, filePath string) string {
		asked = sessionID + " " + filePath
		return "THE SUB RULES"
	}, nil, dir)

	resp := runViewTool(t, tool, context.WithValue(t.Context(), SessionIDContextKey, "s1"), ViewParams{FilePath: path})
	require.Contains(t, resp.Content, "package a")
	require.True(t, strings.HasSuffix(strings.TrimSpace(resp.Content), "THE SUB RULES"), resp.Content)
	require.Equal(t, "s1 "+path, asked)
}

// A model that cannot see images is told what the image is, its format,
// dimensions and size, and how to look into it, rather than only that
// it cannot; a large image too, which no size would let it see.
func TestViewTellsAModelThatCannotSeeImagesWhatAnImageIs(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	write := func(name string, encode func(io.Writer, image.Image) error, w, h int) string {
		path := filepath.Join(workingDir, name)
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, encode(f, image.NewRGBA(image.Rect(0, 0, w, h))))
		require.NoError(t, f.Close())
		return path
	}
	frame := write("frame.png", png.Encode, 32, 18)
	photo := write("photo.jpg", func(w io.Writer, m image.Image) error { return jpeg.Encode(w, m, nil) }, 9, 16)
	large := filepath.Join(workingDir, "large.png")
	require.NoError(t, os.WriteFile(large, []byte(strings.Repeat("a", 200*1024+1)), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	ctx = context.WithValue(ctx, SupportsImagesContextKey, false)
	ctx = context.WithValue(ctx, ModelNameContextKey, "qwen3.8-27b")
	for path, what := range map[string]string{
		frame: "a PNG image of 32x18 pixels",
		photo: "a JPEG image of 9x16 pixels",
		large: "an image",
	} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		resp := runViewTool(t, tool, ctx, ViewParams{FilePath: path})
		require.True(t, resp.IsError)
		require.Equal(t, "This model (qwen3.8-27b) cannot see images. "+path+" is "+what+", "+humanize.Bytes(uint64(info.Size()))+
			". To check what it shows, analyze its pixels in code (with Python's PIL or numpy, say) or ask a model that sees images.", resp.Content)
	}
}

// noise is a w by h image of random pixels, which compresses badly.
func noise(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(rng.Uint32())
	}
	return img
}

// An image goes to a model that sees images as Claude Code sends it: as
// it is when it is at most 2000 pixels a side and 3.75 MB, where gnosh
// refused everything over 200 KB, most screenshots; scaled down to fit
// when it is larger, kept a PNG when that fits, else made a JPEG.
func TestViewFitsImagesAsClaudeCodeDoes(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	write := func(name string, img image.Image, encode func(io.Writer, image.Image) error) string {
		path := filepath.Join(workingDir, name)
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, encode(f, img))
		require.NoError(t, f.Close())
		return path
	}
	jpg := func(w io.Writer, m image.Image) error { return jpeg.Encode(w, m, &jpeg.Options{Quality: 95}) }
	screenshot := write("screenshot.png", noise(300, 200), png.Encode)
	wide := write("wide.png", image.NewRGBA(image.Rect(0, 0, 3000, 1000)), png.Encode)
	photo := write("photo.jpg", noise(2600, 2600), jpg)
	grainy := write("grainy.png", noise(1200, 1200), png.Encode)

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	ctx = context.WithValue(ctx, SupportsImagesContextKey, true)
	view := func(path string) (image.Config, string, fantasy.ToolResponse) {
		resp := runViewTool(t, tool, ctx, ViewParams{FilePath: path})
		require.False(t, resp.IsError, resp.Content)
		require.Equal(t, "image", resp.Type)
		require.LessOrEqual(t, len(resp.Data), maxImageBytes)
		config, format, err := image.DecodeConfig(bytes.NewReader(resp.Data))
		require.NoError(t, err)
		return config, format, resp
	}

	data, err := os.ReadFile(screenshot)
	require.NoError(t, err)
	require.Greater(t, len(data), 200*1024, "over the 200 KB that was refused")
	_, _, resp := view(screenshot)
	require.Equal(t, data, resp.Data, "sent as it is")
	require.Equal(t, "image/png", resp.MediaType)

	config, format, resp := view(wide)
	require.Equal(t, [2]int{2000, 666}, [2]int{config.Width, config.Height})
	require.Equal(t, "png", format)
	require.Equal(t, "image/png", resp.MediaType)

	config, format, resp = view(photo)
	require.Equal(t, [2]int{2000, 2000}, [2]int{config.Width, config.Height})
	require.Equal(t, "jpeg", format)
	require.Equal(t, "image/jpeg", resp.MediaType)

	config, format, resp = view(grainy)
	require.Equal(t, [2]int{1200, 1200}, [2]int{config.Width, config.Height}, "small enough, too many bytes")
	require.Equal(t, "jpeg", format)
	require.Equal(t, "image/jpeg", resp.MediaType)
}

// vastPNG is a valid w by h grayscale PNG of zeros, small to store and
// large to decode.
func vastPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var idat bytes.Buffer
	zw := zlib.NewWriter(&idat)
	row := make([]byte, 1+w) // filter byte, then w gray pixels
	for range h {
		_, err := zw.Write(row)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	chunk := func(kind string, data []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
		out = append(out, kind...)
		out = append(out, data...)
		return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(kind), data...)))
	}
	ihdr := binary.BigEndian.AppendUint32(nil, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, 8, 0, 0, 0, 0) // 8-bit gray
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, chunk("IHDR", ihdr)...)
	out = append(out, chunk("IDAT", idat.Bytes())...)
	return append(out, chunk("IEND", nil)...)
}

// An image of more pixels than gnosh decodes to scale is refused from its
// header, undecoded; one gnosh cannot decode goes as it is when it is
// small enough.
func TestFitImageRefusesWhatItCannotScale(t *testing.T) {
	t.Parallel()

	vast := vastPNG(t, 8000, 8000)
	config, err := png.DecodeConfig(bytes.NewReader(vast))
	require.NoError(t, err)
	require.Greater(t, config.Width*config.Height, maxImagePixels)
	_, _, err = fitImage(vast, "image/png")
	require.ErrorIs(t, err, errImageTooLarge)

	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 1000)...)
	data, mimeType, err := fitImage(webp, "image/webp")
	require.NoError(t, err)
	require.Equal(t, webp, data)
	require.Equal(t, "image/webp", mimeType)
}
