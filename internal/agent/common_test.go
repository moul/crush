package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/x/vcr"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"

	_ "github.com/joho/godotenv/autoload"
)

// fakeEnv is an environment for testing.
type fakeEnv struct {
	workingDir  string
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	history     history.Service
	filetracker filetracker.Service
	lspClients  *csync.Map[string, *lsp.Client]
}

type builderFunc func(t *testing.T, r *vcr.Recorder) (fantasy.LanguageModel, error)

type modelPair struct {
	name       string
	largeModel builderFunc
	smallModel builderFunc
}

func hyperBuilder(model string) builderFunc {
	return func(t *testing.T, r *vcr.Recorder) (fantasy.LanguageModel, error) {
		provider, err := openaicompat.New(
			openaicompat.WithBaseURL("https://hyper.charm.land/v1"),
			openaicompat.WithAPIKey(os.Getenv("CRUSH_HYPER_API_KEY")),
			openaicompat.WithHTTPClient(&http.Client{Transport: failFastOnStaleCassette{r}}),
		)
		if err != nil {
			return nil, err
		}
		return provider.LanguageModel(t.Context(), model)
	}
}

// failFastOnStaleCassette turns "no recorded interaction matches this
// request" into an immediate 400 instead of a transport error.
//
// The recorder reports a mismatch as an error from RoundTrip, which the
// provider stack reads as a flaky network and retries with exponential
// backoff — so a cassette that no longer matches the prompt cost about 40
// seconds per subtest, and TestCoderAgent about eight minutes, before
// failing with the same diff it could have shown at once. A 400 is not
// retried, so it fails in milliseconds. The recorder's matcher has
// already logged the diff by the time this returns.
// staleCassettes is how a mismatch reads in the error a turn returns;
// runCassette looks for it.
const staleCassettes = "the cassettes under internal/agent/testdata are stale"

type failFastOnStaleCassette struct{ next http.RoundTripper }

func (f failFastOnStaleCassette) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.next.RoundTrip(req)
	if err != nil && errors.Is(err, cassette.ErrInteractionNotFound) {
		const msg = `{"error":{"message":"no recorded interaction matches this request: ` + staleCassettes + ` and must be re-recorded against the live API"}}`
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(msg)),
			Request:    req,
		}, nil
	}
	return resp, err
}

// testEnv is the environment for a test, working in a directory of its
// own. It used a fixed directory under /tmp/crush-test, shared by every
// user and every run: once one user's run of the suite had created it,
// another user's runs of these tests failed with "permission denied" (the
// AI box's self-improvement agent hit that in every run, because the
// suite is verified there as root), and two runs at once removed each
// other's files.
func testEnv(t *testing.T) fakeEnv {
	return newTestEnv(t, t.TempDir())
}

// cassetteEnv is testEnv in the fixed directory the recorded cassettes
// were made in: the working directory is part of every recorded request,
// so it must be the same when the cassettes are replayed. Another user's
// run of the suite can own that directory (on the AI box the suite is
// verified as root, and the self-improvement agent runs it as crush). The
// test is then skipped with the reason, as for stale cassettes, since
// they can be replayed nowhere else; it failed, and each run spent calls
// finding out why.
func cassetteEnv(t *testing.T) fakeEnv {
	workingDir := filepath.Join("/tmp/crush-test/", t.Name())
	if err := os.RemoveAll(workingDir); err != nil {
		t.Skipf("cannot use the directory the cassettes were recorded in: %v", err)
	}
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Skipf("cannot use the directory the cassettes were recorded in: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(workingDir) })
	return newTestEnv(t, workingDir)
}

func newTestEnv(t *testing.T, workingDir string) fakeEnv {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	permissions := permission.NewPermissionService(workingDir, true, []string{})
	history := history.NewService(q, conn)
	filetrackerService := filetracker.NewService(q)
	lspClients := csync.NewMap[string, *lsp.Client]()

	t.Cleanup(func() { conn.Close() })

	return fakeEnv{
		workingDir,
		sessions,
		messages,
		permissions,
		history,
		filetrackerService,
		lspClients,
	}
}

func testSessionAgent(env fakeEnv, large, small fantasy.LanguageModel, systemPrompt string, tools ...fantasy.AgentTool) SessionAgent {
	largeModel := Model{
		Model: large,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
	smallModel := Model{
		Model: small,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel:   largeModel,
		SmallModel:   smallModel,
		SystemPrompt: systemPrompt,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
		Tools:        tools,
	})
	return agent
}

func coderAgent(r *vcr.Recorder, env fakeEnv, large, small fantasy.LanguageModel) (SessionAgent, error) {
	fixedTime := func() time.Time {
		t, _ := time.Parse("1/2/2006", "1/1/2025")
		return t
	}
	prompt, err := coderPrompt(
		prompt.WithTimeFunc(fixedTime),
		prompt.WithPlatform("linux"),
		prompt.WithWorkingDir(filepath.ToSlash(env.workingDir)),
	)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Init(env.workingDir, "", false, config.TrustFolderForSession())
	if err != nil {
		return nil, err
	}

	// NOTE(@andreynering): Set a fixed config to ensure cassettes match
	// independently of user config on `$HOME/.config/crush/crush.json`.
	cfg.Config().Options.Attribution = &config.Attribution{
		TrailerStyle:  "co-authored-by",
		GeneratedWith: true,
	}

	// Clear some fields to avoid issues with VCR cassette matching.
	cfg.Config().Options.SkillsPaths = nil
	cfg.Config().Options.DisabledSkills = []string{"gnosh-config"}
	cfg.Config().Options.ContextPaths = nil
	cfg.Config().Options.GlobalContextPaths = nil
	cfg.Config().LSP = nil

	systemPrompt, err := prompt.Build(context.TODO(), large.Provider(), large.Model(), cfg)
	if err != nil {
		return nil, err
	}

	// Get the model name for the bash tool
	modelName := large.Model() // fallback to ID if Name not available
	if model := cfg.Config().GetModel(large.Provider(), large.Model()); model != nil {
		modelName = model.Name
	}

	allTools := []fantasy.AgentTool{
		tools.NewBashTool(env.permissions, env.workingDir, cfg.Config().Options.DataDirectory, cfg.Config().Options.Attribution, modelName, cfg.Config().Options.GetMaxBackgroundJobs(), nil, nil, nil, nil, nil),
		tools.NewDownloadTool(env.permissions, env.workingDir, r.GetDefaultClient()),
		tools.NewEditTool(nil, env.permissions, env.history, env.filetracker, nil, env.workingDir),
		tools.NewMultiEditTool(nil, env.permissions, env.history, env.filetracker, nil, env.workingDir),
		tools.NewFetchTool(env.permissions, env.workingDir, r.GetDefaultClient()),
		tools.NewGlobTool(env.permissions, env.workingDir, cfg.Config().Tools.Glob),
		tools.NewGrepTool(env.permissions, env.workingDir, cfg.Config().Tools.Grep),
		tools.NewLsTool(env.permissions, env.workingDir, cfg.Config().Tools.Ls),
		tools.NewSourcegraphTool(r.GetDefaultClient()),
		tools.NewViewTool(nil, env.permissions, env.filetracker, nil, nil, nil, env.workingDir),
		tools.NewWriteTool(nil, env.permissions, env.history, env.filetracker, nil, env.workingDir),
	}

	return testSessionAgent(env, large, small, systemPrompt, allTools...), nil
}

// createSimpleGoProject creates a simple Go project structure in the given directory.
// It creates a go.mod file and a main.go file with a basic hello world program.
func createSimpleGoProject(t *testing.T, dir string) {
	goMod := `module example.com/testproject

go 1.23
`
	err := os.WriteFile(dir+"/go.mod", []byte(goMod), 0o644)
	require.NoError(t, err)

	mainGo := `package main

import "fmt"

func main() {
	fmt.Println("Hello, World!")
}
`
	err = os.WriteFile(dir+"/main.go", []byte(mainGo), 0o644)
	require.NoError(t, err)
}
