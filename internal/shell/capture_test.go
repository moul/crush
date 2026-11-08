package shell

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// A bang command the shell cannot run at all, one that does not parse or
// names a script that is not there, has its reason in the error Run
// returns and nothing on stderr. The captured output must carry it, or
// the user and the model see an empty result with exit code 1.
func TestRunAndCapture_ReportsWhyTheShellCouldNotRun(t *testing.T) {
	t.Parallel()

	captures := map[string]func(context.Context, RunOptions) (CaptureResult, error){
		"RunAndCapture": RunAndCapture,
		"RunAndCaptureStream": func(ctx context.Context, opts RunOptions) (CaptureResult, error) {
			return RunAndCaptureStream(ctx, opts, nil)
		},
	}
	cases := map[string]string{
		`echo "unterminated`:  "could not parse command",
		`./does-not-exist.sh`: "no such file or directory",
	}
	for name, capture := range captures {
		for command, want := range cases {
			t.Run(name+"/"+command, func(t *testing.T) {
				t.Parallel()
				result, err := capture(t.Context(), RunOptions{
					Command: command,
					Cwd:     t.TempDir(),
				})
				require.NoError(t, err)
				require.Equal(t, 1, result.ExitCode)
				require.Contains(t, result.Output, want)
			})
		}
	}
}

// A command that ran and exited non-zero keeps its output as it wrote it:
// its own stderr already says what went wrong.
func TestRunAndCapture_LeavesAnExitStatusAlone(t *testing.T) {
	t.Parallel()

	result, err := RunAndCapture(t.Context(), RunOptions{
		Command: "echo oops >&2; exit 3",
		Cwd:     t.TempDir(),
	})
	require.NoError(t, err)
	require.Equal(t, 3, result.ExitCode)
	require.Equal(t, "oops\n", result.Output)
}
