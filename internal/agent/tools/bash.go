package tools

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/shell"
)

type BashParams struct {
	Description         string `json:"description" description:"A brief description of what the command does, try to keep it under 30 characters or so"`
	Command             string `json:"command" description:"The command to execute"`
	Host                string `json:"host,omitempty" description:"Run on another declared machine, by its name from <hosts>; omit (or \"local\") for this machine. A path belongs to the machine it was seen on."`
	WorkingDir          string `json:"working_dir,omitempty" description:"The working directory to execute the command in (defaults to current directory)"`
	RunInBackground     bool   `json:"run_in_background,omitempty" description:"Set to true (boolean) to run this command in the background. Use job_output to read the output later."`
	AutoBackgroundAfter int    `json:"auto_background_after,omitempty" description:"Seconds to wait before automatically moving the command to a background job (default: 120)"`
	Timeout             int    `json:"timeout,omitempty" description:"Milliseconds to wait for the command before it moves to a background job, at most the most the tool description gives; auto_background_after wins if both are set"`
}

// maxBashTimeoutSeconds bounds a bash call's timeout unless
// BASH_MAX_TIMEOUT_MS says otherwise: Claude Code's 600000 ms.
const maxBashTimeoutSeconds = 600

// timeoutSeconds is a timeout in milliseconds, Claude Code's bash
// parameter, as the whole seconds to wait before a command moves to the
// background: rounded up, at most maxWaitSeconds, 0 for none. A model
// used to Claude Code's Bash gives a long test run timeout 600000, and
// Gnosh dropped it: the run moved to the background after a minute, to be
// read with job_output, a model round trip each time.
func timeoutSeconds(ms int) int {
	if ms <= 0 {
		return 0
	}
	return min((ms+999)/1000, maxWaitSeconds())
}

// envInt is the positive whole number the environment variable name
// holds, or def: Claude Code's BASH_* limits and its
// CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS, which a gnosh.json's env can
// set too.
func envInt(name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && n > 0 {
		return n
	}
	return def
}

// defaultWaitSeconds is how long a command runs before it moves to the
// background when its call gives no timeout: BASH_DEFAULT_TIMEOUT_MS, as
// in Claude Code, rounded up to whole seconds, or
// DefaultAutoBackgroundAfter, Claude Code's 2 minutes. A long job (a test
// suite, a render) that finishes within it costs the model no job_output
// round trips, each of which on a local model means a step over the whole
// conversation. Of the commands moved to the background on the AI box in
// a week at 60 seconds, a sixth or more would have ended within 2 minutes.
func defaultWaitSeconds() int {
	return (envInt("BASH_DEFAULT_TIMEOUT_MS", DefaultAutoBackgroundAfter*1000) + 999) / 1000
}

// maxWaitSeconds is the longest a call's timeout can ask for:
// BASH_MAX_TIMEOUT_MS, as in Claude Code (maxBashTimeoutSeconds by
// default), or the default wait when that is longer.
func maxWaitSeconds() int {
	return max((envInt("BASH_MAX_TIMEOUT_MS", maxBashTimeoutSeconds*1000)+999)/1000, defaultWaitSeconds())
}

// maxOutputLength is how many characters of a command's output the model
// reads before the middle is cut: BASH_MAX_OUTPUT_LENGTH, as in Claude
// Code, at most 150000, or MaxOutputLength.
func maxOutputLength() int {
	return min(envInt("BASH_MAX_OUTPUT_LENGTH", MaxOutputLength), 150_000)
}

type BashPermissionsParams struct {
	Description         string `json:"description"`
	Command             string `json:"command"`
	Host                string `json:"host"`
	WorkingDir          string `json:"working_dir"`
	RunInBackground     bool   `json:"run_in_background"`
	AutoBackgroundAfter int    `json:"auto_background_after"`
	Timeout             int    `json:"timeout"`
	// Note is what the question says besides: that git in this folder,
	// not trusted, would run the programs its git config names.
	Note string `json:"note,omitempty"`
}

// gitWord finds git run as a command in a command line.
var gitWord = regexp.MustCompile(`(^|[\s;&|(])git(\s|$)`)

// bashQuestion is what the permission question for params shows: the
// call, and a note when it runs git in a project whose git config would
// have git run programs, in a folder not trusted.
func bashQuestion(params BashParams, gitPrograms []string) BashPermissionsParams {
	q := BashPermissionsParams{
		Description: params.Description, Command: params.Command, Host: params.Host, WorkingDir: params.WorkingDir,
		RunInBackground: params.RunInBackground, AutoBackgroundAfter: params.AutoBackgroundAfter, Timeout: params.Timeout,
	}
	if params.Host == "" && len(gitPrograms) > 0 && gitWord.MatchString(params.Command) {
		q.Note = "This folder is not trusted, and its git config has git run programs as you: " + strings.Join(gitPrograms, ", ")
	}
	return q
}

type BashResponseMetadata struct {
	StartTime        int64  `json:"start_time"`
	EndTime          int64  `json:"end_time"`
	Output           string `json:"output"`
	Description      string `json:"description"`
	WorkingDirectory string `json:"working_directory"`
	Background       bool   `json:"background,omitempty"`
	ShellID          string `json:"shell_id,omitempty"`
	// Host names the machine the command ran on ("local" for this one)
	// and Machine identifies it (user@hostname, or the host's ssh
	// destination), so the stored call is the whole tuple: where, in
	// what directory, what, and how it ended.
	Host    string `json:"host,omitempty"`
	Machine string `json:"machine,omitempty"`
	// ExitCode is the command's exit status once it has finished; the
	// output text says it too ("Exit code N"), but readers such as the
	// --verbose trace should not have to parse that.
	ExitCode int `json:"exit_code,omitempty"`
}

const (
	BashToolName = "bash"

	DefaultAutoBackgroundAfter = 120 // Commands taking longer automatically become background jobs, after Claude Code's 2 minutes
	MaxOutputLength            = 30000
	BashNoOutput               = "no output"
)

//go:embed bash.md.tpl
var bashDescriptionTmpl []byte

var bashDescriptionTpl = template.Must(
	template.New("bashDescription").
		Parse(string(bashDescriptionTmpl)),
)

type bashDescriptionData struct {
	BannedCommands  string
	DenyRules       string
	MaxOutputLength int
	DefaultWait     int
	MaxTimeoutMs    int
	Attribution     config.Attribution
	ModelID         string
	RgAvailable     bool
	GhAvailable     bool
	LocalHost       string
	Hosts           []config.Host
	// Notified is set when the agent is told a background command's end,
	// and so may not start a command with a long sleep (BlockedSleep).
	Notified     bool
	BlockedSleep int
}

var bannedCommands = []string{
	// Network/Download tools
	"alias",
	"aria2c",
	"axel",
	"chrome",
	"curl",
	"curlie",
	"firefox",
	"http-prompt",
	"httpie",
	"links",
	"lynx",
	"nc",
	"safari",
	"scp",
	"ssh",
	"telnet",
	"w3m",
	"wget",
	"xh",

	// System administration
	"doas",
	"su",
	"sudo",

	// Package managers
	"apk",
	"apt",
	"apt-cache",
	"apt-get",
	"dnf",
	"dpkg",
	"emerge",
	"home-manager",
	"makepkg",
	"opkg",
	"pacman",
	"paru",
	"pkg",
	"pkg_add",
	"pkg_delete",
	"portage",
	"rpm",
	"yay",
	"yum",
	"zypper",

	// System modification
	"at",
	"batch",
	"chkconfig",
	"crontab",
	"fdisk",
	"mkfs",
	"mount",
	"parted",
	"service",
	"systemctl",
	"umount",

	// Network configuration
	"firewall-cmd",
	"ifconfig",
	"ip",
	"iptables",
	"netstat",
	"pfctl",
	"route",
	"ufw",
}

func bashDescription(attribution *config.Attribution, modelID string, denyRules []string, notified bool, hosts config.Hosts) string {
	bannedCommandsStr := strings.Join(bannedCommands, ", ")
	var out bytes.Buffer
	if err := bashDescriptionTpl.Execute(&out, bashDescriptionData{
		BannedCommands:  bannedCommandsStr,
		DenyRules:       strings.Join(denyRules, ", "),
		MaxOutputLength: maxOutputLength(),
		DefaultWait:     defaultWaitSeconds(),
		MaxTimeoutMs:    maxWaitSeconds() * 1000,
		Attribution:     *attribution,
		ModelID:         modelID,
		RgAvailable:     getRg() != "",
		GhAvailable:     ghAvailable,
		LocalHost:       config.LocalMachine(),
		Hosts:           hosts.Sorted(),
		Notified:        notified,
		BlockedSleep:    blockedSleep,
	}); err != nil {
		// this should never happen.
		panic("failed to execute bash description template: " + err.Error())
	}
	return out.String()
}

// blockFuncs are the commands the shell refuses to run: the banned ones,
// and those the Bash(pattern) deny rules of the permissions match.
func blockFuncs(denyRules []string) []shell.BlockFunc {
	funcs := []shell.BlockFunc{
		shell.CommandsBlocker(bannedCommands),

		// System package managers
		shell.ArgumentsBlocker("apk", []string{"add"}, nil),
		shell.ArgumentsBlocker("apt", []string{"install"}, nil),
		shell.ArgumentsBlocker("apt-get", []string{"install"}, nil),
		shell.ArgumentsBlocker("dnf", []string{"install"}, nil),
		shell.ArgumentsBlocker("pacman", nil, []string{"-S"}),
		shell.ArgumentsBlocker("pkg", []string{"install"}, nil),
		shell.ArgumentsBlocker("yum", []string{"install"}, nil),
		shell.ArgumentsBlocker("zypper", []string{"install"}, nil),

		// Language-specific package managers
		shell.ArgumentsBlocker("brew", []string{"install"}, nil),
		shell.ArgumentsBlocker("cargo", []string{"install"}, nil),
		shell.ArgumentsBlocker("gem", []string{"install"}, nil),
		shell.ArgumentsBlocker("go", []string{"install"}, nil),
		shell.ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
		shell.ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
		shell.ArgumentsBlocker("pip", []string{"install"}, []string{"--user"}),
		shell.ArgumentsBlocker("pip3", []string{"install"}, []string{"--user"}),
		shell.ArgumentsBlocker("pnpm", []string{"add"}, []string{"--global"}),
		shell.ArgumentsBlocker("pnpm", []string{"add"}, []string{"-g"}),
		shell.ArgumentsBlocker("yarn", []string{"global", "add"}, nil),

		// `go test -exec` can run arbitrary commands
		shell.ArgumentsBlocker("go", []string{"test"}, []string{"-exec"}),

		// A gno.land key's password belongs in the gnokey tool's masked
		// prompt, never on a command line the model wrote (see
		// gnokey.go). gnokey reads it from standard input only with this
		// flag, so refusing the flag here closes the shell route.
		shell.ArgumentsBlocker("gnokey", nil, []string{"-insecure-password-stdin"}),
		shell.ArgumentsBlocker("gnokey", nil, []string{"--insecure-password-stdin"}),
	}
	for _, pattern := range denyRules {
		funcs = append(funcs, shell.CommandRuleBlocker(pattern))
	}
	return funcs
}

// NewBashTool returns the bash tool. denyRules are the patterns of the
// Bash(pattern) deny rules of the permissions: the shell refuses every
// command one matches, wherever in a command line it is. allowRules are
// the patterns of the Bash(pattern) allow rules: a command line runs
// without asking when each command in it matches one or only reads.
// BackgroundWatcher is told of each command that becomes a background
// job, with the context of the bash call that started it, so the agent
// can be told when the job ends.
type BackgroundWatcher func(ctx context.Context, job *shell.BackgroundShell)

// NewBashTool runs shell commands. onBackground, when not nil, is told of
// each command that becomes a background job, and the result then says
// the agent will be told when it ends.
func NewBashTool(permissions permission.Service, workingDir, spillDir string, attribution *config.Attribution, modelID string, maxBackgroundJobs int, denyRules, allowRules []string, onBackground BackgroundWatcher, hosts config.Hosts, gitPrograms []string) fantasy.AgentTool {
	allowed := commandRules(allowRules)
	return fantasy.NewAgentTool(
		BashToolName,
		string(bashDescription(attribution, modelID, denyRules, onBackground != nil, hosts)),
		func(ctx context.Context, params BashParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Command == "" {
				return fantasy.NewTextErrorResponse("missing command"), nil
			}
			// Where the agent is told when a background command ends, a
			// long sleep in the foreground only holds it up, as Claude
			// Code has it.
			if onBackground != nil && !params.RunInBackground {
				if sleep := foregroundSleep(params.Command); sleep != "" {
					return fantasy.NewTextErrorResponse(fmt.Sprintf(sleepBlocked, sleep)), nil
				}
			}

			host, remote, err := resolveHost(hosts, params.Host)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			// Where the command runs: this machine's shell, in the working
			// directory, or ssh to the host. The local shell then runs
			// only the ssh line built here, so the block list (ssh
			// included) does not apply to it; what runs on the other
			// machine is gated by the user's deny rules, their declaration
			// of the host, and a permission prompt that names it, never by
			// the read-only shortcut or an allow rule, which are this
			// machine's.
			execWorkingDir := cmp.Or(params.WorkingDir, workingDir)
			command := params.Command
			blocks := blockFuncs(denyRules)
			hostName, machine := config.LocalHostName, config.LocalMachine()
			jobDescription := params.Description
			if remote {
				if rule := hostDenied(params.Command, denyRules); rule != "" {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("command is not allowed for security reasons: a Bash(%s) deny rule matches it", rule)), nil
				}
				command, err = hostCommand(host.SSH, params.WorkingDir, params.Command)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				execWorkingDir = workingDir
				blocks = nil
				hostName, machine = params.Host, host.SSH
				// The job tools show a job's description, not its host.
				jobDescription = params.Host + ": " + cmp.Or(params.Description, params.Command)
			}
			// Where the command ended up: this machine's shell keeps its
			// directory across cd; another machine's is known only when
			// the call set it.
			cwdOf := func(job *shell.BackgroundShell) string {
				if remote {
					return params.WorkingDir
				}
				return job.WorkingDir
			}

			dirs := workDirs{start: execWorkingDir, project: workingDir, gitPrograms: gitPrograms}
			isSafeReadOnly := !remote && (readOnlyCommand(params.Command, dirs) || allowedCommand(params.Command, dirs, allowed))

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for executing shell command")
			}
			if !isSafeReadOnly {
				description := fmt.Sprintf("Execute command: %s", params.Command)
				permissionPath := execWorkingDir
				if remote {
					description = fmt.Sprintf("Execute on %s (%s): %s", params.Host, host.SSH, params.Command)
					permissionPath = hostPermissionPath(host.SSH, params.WorkingDir)
				}
				p, err := permissions.Request(
					ctx,
					permission.CreatePermissionRequest{
						SessionID:   sessionID,
						Path:        permissionPath,
						ToolCallID:  call.ID,
						ToolName:    BashToolName,
						Action:      "execute",
						Description: description,
						Params:      bashQuestion(params, gitPrograms),
					},
				)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if !p {
					return NewPermissionDeniedResponse(), nil
				}
			}

			// If explicitly requested as background, start immediately with detached context
			if params.RunInBackground {
				startTime := time.Now()
				bgManager := shell.GetBackgroundShellManager()
				// Use background context so it continues after tool returns
				bgShell, err := bgManager.StartAfter(context.Background(), execWorkingDir, blocks, sessionEnvSetup(ctx), command, jobDescription, maxBackgroundJobs)
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("error starting background shell: %w", err)
				}

				// Wait a short time to detect fast failures (blocked commands, syntax errors, etc.)
				time.Sleep(1 * time.Second)
				stdout, stderr, done, execErr := bgShell.GetOutput()

				if done {
					// Command failed or completed very quickly
					bgManager.Remove(bgShell.ID)

					interrupted := shell.IsInterrupt(execErr)
					exitCode := shell.ExitCode(execErr)
					if exitCode == 0 && !interrupted && execErr != nil {
						return fantasy.ToolResponse{}, fmt.Errorf("[Job %s] error executing command: %w", bgShell.ID, execErr)
					}

					stdout = formatOutput(stdout, stderr, execErr, spillDir)

					metadata := BashResponseMetadata{
						StartTime:        startTime.UnixMilli(),
						EndTime:          time.Now().UnixMilli(),
						Output:           stdout,
						Description:      params.Description,
						Background:       params.RunInBackground,
						WorkingDirectory: cwdOf(bgShell),
						Host:             hostName,
						Machine:          machine,
						ExitCode:         exitCode,
					}
					tags := resultTags(hostName, machine, cwdOf(bgShell))
					if stdout == "" {
						return fantasy.WithResponseMetadata(fantasy.NewTextResponse(BashNoOutput+tags), metadata), nil
					}
					stdout += tags
					return fantasy.WithResponseMetadata(fantasy.NewTextResponse(stdout), metadata), nil
				}

				// Still running after fast-failure check - return as background job
				metadata := BashResponseMetadata{
					StartTime:        startTime.UnixMilli(),
					EndTime:          time.Now().UnixMilli(),
					Description:      params.Description,
					WorkingDirectory: cwdOf(bgShell),
					Background:       true,
					ShellID:          bgShell.ID,
					Host:             hostName,
					Machine:          machine,
				}
				response := fmt.Sprintf("Background shell started with ID: %s\n\n%s", bgShell.ID, backgrounded(ctx, bgShell, onBackground))
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
			}

			// Start synchronous execution with auto-background support
			startTime := time.Now()

			// Start with detached context so it can survive if moved to background
			bgManager := shell.GetBackgroundShellManager()
			bgShell, err := bgManager.StartAfter(context.Background(), execWorkingDir, blocks, sessionEnvSetup(ctx), command, jobDescription, maxBackgroundJobs)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error starting shell: %w", err)
			}

			// Wait for either completion, auto-background threshold, or context cancellation
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			autoBackgroundAfter := cmp.Or(params.AutoBackgroundAfter, timeoutSeconds(params.Timeout), defaultWaitSeconds())
			autoBackgroundThreshold := time.Duration(autoBackgroundAfter) * time.Second
			timeout := time.After(autoBackgroundThreshold)

			var stdout, stderr string
			var done bool
			var execErr error

		waitLoop:
			for {
				select {
				case <-ticker.C:
					// Only ask whether the job has finished. GetOutput
					// copies the whole accumulated output, so reading it
					// on every tick costs one full copy of the command's
					// output ten times a second for as long as the
					// command runs, and the values are thrown away on
					// every tick but the last.
					if !bgShell.IsDone() {
						continue
					}
					stdout, stderr, done, execErr = bgShell.GetOutput()
					break waitLoop
				case <-timeout:
					stdout, stderr, done, execErr = bgShell.GetOutput()
					break waitLoop
				case <-ctx.Done():
					// Incoming context was cancelled before we moved to background
					// Kill the shell and return error
					bgManager.Kill(bgShell.ID)
					return fantasy.ToolResponse{}, ctx.Err()
				}
			}

			if done {
				// Command completed within threshold - return synchronously
				// Remove from background manager since we're returning directly
				// Don't call Kill() as it cancels the context and corrupts the exit code
				bgManager.Remove(bgShell.ID)

				interrupted := shell.IsInterrupt(execErr)
				exitCode := shell.ExitCode(execErr)
				if exitCode == 0 && !interrupted && execErr != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("[Job %s] error executing command: %w", bgShell.ID, execErr)
				}

				stdout = formatOutput(stdout, stderr, execErr, spillDir)

				metadata := BashResponseMetadata{
					StartTime:        startTime.UnixMilli(),
					EndTime:          time.Now().UnixMilli(),
					Output:           stdout,
					Description:      params.Description,
					Background:       params.RunInBackground,
					WorkingDirectory: cwdOf(bgShell),
					Host:             hostName,
					Machine:          machine,
					ExitCode:         exitCode,
				}
				tags := resultTags(hostName, machine, cwdOf(bgShell))
				if stdout == "" {
					return fantasy.WithResponseMetadata(fantasy.NewTextResponse(BashNoOutput+tags), metadata), nil
				}
				stdout += tags
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(stdout), metadata), nil
			}

			// Still running - keep as background job
			metadata := BashResponseMetadata{
				StartTime:        startTime.UnixMilli(),
				EndTime:          time.Now().UnixMilli(),
				Description:      params.Description,
				WorkingDirectory: cwdOf(bgShell),
				Background:       true,
				ShellID:          bgShell.ID,
				Host:             hostName,
				Machine:          machine,
			}

			// In Claude Code's words, with how long it waited, so the model
			// knows what timeout to ask for to keep such a command in the
			// foreground.
			response := fmt.Sprintf("Command did not complete within its %ds timeout and was moved to the background (ID: %s). %s", autoBackgroundAfter, bgShell.ID, backgrounded(ctx, bgShell, onBackground))
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
		},
	)
}

// backgrounded hands job, a command that became a background job, to
// onBackground, and says what the agent can do with it: with a watcher,
// it is told when the job ends, as Claude Code tells it, so it need not
// poll.
func backgrounded(ctx context.Context, job *shell.BackgroundShell, onBackground BackgroundWatcher) string {
	if onBackground == nil {
		return "Call the job_output tool with this ID to read its output, or the job_kill tool to stop it."
	}
	onBackground(ctx, job)
	return "You will be told when it ends; there is no need to poll it. Call the job_output tool with this ID to read its output, or the job_kill tool to stop it."
}

// sessionEnvSetup is the shell code a bash command runs first: the env
// file of its session (WithSessionEnvFile), which the SessionStart hooks
// write export lines to (Claude Code's CLAUDE_ENV_FILE), when it holds
// anything, as Claude Code runs it before each Bash command. "" when
// there is none.
func sessionEnvSetup(ctx context.Context) string {
	path := SessionEnvFileFromContext(ctx)
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		return ""
	}
	return ". '" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// formatOutput formats the output of a completed command with error handling
func formatOutput(stdout, stderr string, execErr error, spillDir string) string {
	interrupted := shell.IsInterrupt(execErr)
	exitCode := shell.ExitCode(execErr)

	stdout = truncateOutput(stdout, spillDir)
	stderr = truncateOutput(stderr, spillDir)

	errorMessage := stderr
	if errorMessage == "" && execErr != nil {
		errorMessage = execErr.Error()
	}

	if interrupted {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += "Command was aborted before completion"
	} else if exitCode != 0 {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += fmt.Sprintf("Exit code %d", exitCode)
	}

	hasBothOutputs := stdout != "" && stderr != ""

	if hasBothOutputs {
		stdout += "\n"
	}

	if errorMessage != "" {
		stdout += "\n" + errorMessage
	}

	return stdout
}

// TruncateOutput keeps the head and tail of oversized tool output. The
// dropped middle is written in full to a file under spillDir so the agent
// can read it back rather than losing it for good.
func TruncateOutput(content, spillDir string) string {
	limit := maxOutputLength()
	runeCount := len([]rune(content))
	if ansi.StringWidth(content) <= limit && runeCount <= limit {
		return content
	}

	halfLength := limit / 2
	var start, end string
	if ansi.StringWidth(content) > limit {
		start = ansi.Truncate(content, halfLength, "")
		end = ansi.TruncateLeft(content, ansi.StringWidth(content)-halfLength, "")
	} else {
		// Oversized by raw length rather than display width: output full of
		// zero-width bytes (a binary printed by cat, say) fills the context
		// window while measuring near nothing on screen, so the width check
		// alone lets it through. Cut on rune boundaries instead.
		r := []rune(content)
		start = string(r[:halfLength])
		end = string(r[len(r)-halfLength:])
	}

	truncatedLinesCount := max(strings.Count(content, "\n")-strings.Count(start, "\n")-strings.Count(end, "\n"), 0)
	marker := fmt.Sprintf("... [%d lines truncated] ...", truncatedLinesCount)
	if path, err := shell.SpillOutput(ansi.Strip(content), spillDir); err == nil {
		marker = fmt.Sprintf("... [%d lines truncated, full output: %s] ...", truncatedLinesCount, path)
	} else {
		slog.Debug("Could not spill tool output", "dir", spillDir, "error", err)
	}
	return fmt.Sprintf("%s\n\n%s\n\n%s", start, marker, end)
}

func truncateOutput(content, spillDir string) string {
	return TruncateOutput(content, spillDir)
}

func normalizeWorkingDir(path string) string {
	if runtime.GOOS == "windows" {
		path = strings.ReplaceAll(path, fsext.WindowsWorkingDirDrive(), "")
	}
	return filepath.ToSlash(path)
}
