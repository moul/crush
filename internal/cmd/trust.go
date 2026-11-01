package cmd

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/home"
	ui "github.com/charmbracelet/crush/internal/ui/model"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"
)

// trustCmd is gnosh trust: record that a folder's project settings may be
// used, list the trusted folders, or forget one.
var trustCmd = &cobra.Command{
	Use:   "trust [folder]",
	Short: "Trust a folder's project settings (its gnoshrc and gnosh.json)",
	Long: `Trust a folder, so that Gnosh uses its project settings when it starts
there: the folder's gnoshrc, a shell script that runs as Gnosh starts, and in
its gnosh.json the hooks, MCP and LSP servers, tools allowed without asking,
model providers, environment variables and the rest.

Until a folder is trusted, Gnosh runs none of that, so a repository you cloned
cannot run its code the moment you start Gnosh in it. The TUI asks first; gnosh
run leaves the settings out and says so on stderr. Settings that only take
something away from the agent (permissions.deny, options.disabled_tools and
options.disabled_skills in a gnosh.json) apply either way.

Trust is kept for the git repository the folder is in, and its worktrees share
it; outside a repository, for the folder and the folders under it that are not
repositories of their own. The home directory is trusted for one session at a
time: Gnosh asks each time it starts there. gnosh run --trust-folder trusts a
folder for one run without recording it.`,
	Example: `
# Trust the current folder's project settings
gnosh trust

# Trust another folder
gnosh trust ~/src/myproject

# List the trusted folders
gnosh trust --list

# Stop trusting a folder
gnosh trust --forget ~/src/myproject
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		if list, _ := cmd.Flags().GetBool("list"); list {
			if len(args) > 0 {
				return errors.New("--list takes no folder")
			}
			return listTrusted(out)
		}
		dir, err := trustDir(cmd, args)
		if err != nil {
			return err
		}
		if forget, _ := cmd.Flags().GetBool("forget"); forget {
			return forgetTrusted(out, dir)
		}
		folder, err := config.TrustFolder(dir)
		if errors.Is(err, config.ErrHomeTrust) {
			return fmt.Errorf("%s is your home directory, which %s trusts for one session at a time: answer yes when it asks there, or start it with --trust-folder", config.ShowPath(folder), version.Name)
		}
		if err != nil {
			return fmt.Errorf("could not record %s as trusted: %w", config.ShowPath(folder), err)
		}
		settings, _, err := config.ScanProject(dir, "")
		if err != nil {
			return err
		}
		what := "this folder and the folders under it that are not repositories of their own"
		if settings.InRepo {
			what = "this git repository and its worktrees"
		}
		fmt.Fprintf(out, "Trusted %s: %s uses the project settings of %s.\n", config.ShowPath(folder), version.Name, what)
		if !settings.Holds() {
			fmt.Fprintln(out, "It has no settings that wait for trust yet.")
			return nil
		}
		fmt.Fprintln(out, "They now:")
		for _, line := range settings.Lines() {
			fmt.Fprintln(out, "  - "+line)
		}
		return nil
	},
}

func init() {
	trustCmd.Flags().Bool("list", false, "List the trusted folders")
	trustCmd.Flags().Bool("forget", false, "Stop trusting the folder")
	trustCmd.MarkFlagsMutuallyExclusive("list", "forget")
}

// trustDir is the folder gnosh trust acts on: its argument, or the working
// directory.
func trustDir(cmd *cobra.Command, args []string) (string, error) {
	if len(args) == 0 {
		return ResolveCwd(cmd)
	}
	dir := home.Long(args[0])
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a folder", args[0])
	}
	return dir, nil
}

// listTrusted prints the trusted folders, one a line, with when each was
// trusted.
func listTrusted(out io.Writer) error {
	folders, err := config.TrustedFolders()
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		fmt.Fprintf(out, "No folder is trusted yet (%s).\n", config.ShowPath(config.TrustFile()))
		return nil
	}
	for _, folder := range slices.Sorted(maps.Keys(folders)) {
		when := folders[folder]
		if day, _, ok := strings.Cut(when, "T"); ok {
			when = day
		}
		fmt.Fprintf(out, "%s  (trusted %s)\n", config.ShowPath(folder), when)
	}
	return nil
}

// forgetTrusted stops trusting dir's folder.
func forgetTrusted(out io.Writer, dir string) error {
	folder, removed, err := config.ForgetFolder(dir)
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintf(out, "No longer trusted: %s.\n", config.ShowPath(folder))
	} else {
		fmt.Fprintf(out, "%s was not recorded as trusted.\n", config.ShowPath(folder))
	}
	if config.FolderTrusted(dir) {
		fmt.Fprintln(out, "It is still trusted through a trusted folder above it; gnosh trust --list shows them.")
	}
	return nil
}

// addTrustFolderFlag adds --trust-folder to cmd.
func addTrustFolderFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("trust-folder", false, "Use this folder's project settings (its gnoshrc, hooks, MCP servers and the rest) for this session, as if trusted, without recording it; gnosh trust records it")
}

// trustFolderOptions are the config load options cmd's --trust-folder asks
// for.
func trustFolderOptions(cmd *cobra.Command) []config.LoadOption {
	if on, _ := cmd.Flags().GetBool("trust-folder"); on {
		return []config.LoadOption{config.TrustFolderForSession()}
	}
	return nil
}

// trustFolderFlag reports whether cmd has --trust-folder set.
func trustFolderFlag(cmd *cobra.Command) bool {
	on, _ := cmd.Flags().GetBool("trust-folder")
	return on
}

// errTrustQuit is quitting at the folder trust question: gnosh exits as a
// program stopped with ctrl+c does, without a word.
var errTrustQuit = exitStatus(130)

// askFolderTrust asks, before the TUI loads the config, whether to trust
// the folder's project settings, when it has some that wait for trust and
// the folder is not trusted yet. Yes records the trust (the home directory
// is trusted for this session only) and sets --trust-folder, so this
// session uses them whatever the record says; no leaves them out. With no
// terminal to ask on, nothing is asked and they stay out.
func askFolderTrust(cmd *cobra.Command) error {
	if trustFolderFlag(cmd) || !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		return nil
	}
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}
	dataDir, _ := cmd.Flags().GetString("data-dir")
	settings, trusted, err := config.ScanProject(cwd, dataDir)
	if err != nil || trusted || !settings.Holds() {
		// The load says what is wrong, if anything.
		return nil
	}
	answer, err := ui.AskFolderTrust(cmd.Context(), settings, savedTheme())
	if err != nil {
		return err
	}
	switch answer {
	case ui.TrustQuit:
		return errTrustQuit
	case ui.TrustNo:
		return nil
	}
	if !settings.Home {
		if _, err := config.TrustFolder(cwd); err != nil {
			slog.Warn("Could not record the folder as trusted; it is trusted for this session", "folder", settings.Folder, "error", err)
		}
	}
	return cmd.Flags().Set("trust-folder", "true")
}

// savedTheme is the theme the user picked last, from the global data
// config, read without loading any config: the trust question is asked
// before one loads.
func savedTheme() string {
	data, err := os.ReadFile(config.GlobalConfigData())
	if err != nil {
		return ""
	}
	return gjson.GetBytes(data, "options.tui.active_theme").String()
}

// noteUntrusted prints on cmd's stderr, for a command that does not ask,
// one line saying that the folder's project settings were left out because
// it is not trusted, what they would do and how to trust it; nothing when
// none were.
func noteUntrusted(cmd *cobra.Command, held *config.ProjectSettings) {
	if !held.Holds() {
		return
	}
	once := ""
	if cmd.Flags().Lookup("trust-folder") != nil {
		once = ", or --trust-folder for one run"
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s is not trusted, so its project settings are off (%s). To use them: %s%s.\n",
		config.ShowPath(held.Folder), held.Brief(), config.TrustCommand(held.Folder), once)
}

// scanUntrusted is what the project settings of a gnosh started as cmd
// holds back, read without loading them, for a command whose config loads
// in a server: nil when the folder is trusted or --trust-folder is set.
func scanUntrusted(cmd *cobra.Command) *config.ProjectSettings {
	if trustFolderFlag(cmd) {
		return nil
	}
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil
	}
	dataDir, _ := cmd.Flags().GetString("data-dir")
	settings, trusted, err := config.ScanProject(cwd, dataDir)
	if err != nil || trusted {
		return nil
	}
	return settings
}
