// Package cli implements the mixtape command-line interface.
//
// The command tree is built fresh on every Execute call from an *app that
// carries every process-level dependency (streams, environment, clock,
// Spotify, audio capture), so there is no package-level state and tests
// drive the real tree against fakes.
package cli

import (
	"context"
	"io"
	"os"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mrz1836/mixtape/internal/config"
)

const rootName = "mixtape"

// Command groups, in help order.
const (
	groupRecording = "recording"
	groupLibrary   = "library"
	groupSetup     = "setup"
	groupOther     = "other"
)

// BuildInfo identifies the running binary; main fills it from -ldflags.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// resolved fills what -ldflags left empty: from the Go build info (a `go
// install …@v0.1.0` build knows its version and commit), else "dev" and
// "unknown".
func (b BuildInfo) resolved() BuildInfo {
	if bi, ok := debug.ReadBuildInfo(); ok {
		b = b.withBuildInfo(bi)
	}
	if b.Version == "" {
		b.Version = "dev"
	}
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	if b.Date == "" {
		b.Date = "unknown"
	}
	return b
}

// withBuildInfo fills empty fields from the module's build information.
func (b BuildInfo) withBuildInfo(bi *debug.BuildInfo) BuildInfo {
	if v := bi.Main.Version; b.Version == "" && releaseVersion(v) {
		b.Version = strings.TrimPrefix(v, "v")
	}
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision" && b.Commit == "":
			b.Commit = s.Value
		case s.Key == "vcs.time" && b.Date == "":
			b.Date = s.Value
		}
	}
	return b
}

// pseudoVersion matches the versions Go stamps on untagged builds
// (v0.0.0-20261102181358-dd1387f547b6).
var pseudoVersion = regexp.MustCompile(`-(\d+\.)?\d{14}-[0-9a-f]{12}`)

// releaseVersion reports a module version that names a release: not empty,
// not "(devel)", not a pseudo-version, not a dirty tree.
func releaseVersion(v string) bool {
	return strings.HasPrefix(v, "v") && !strings.Contains(v, "+") && !pseudoVersion.MatchString(v)
}

// Execute runs mixtape with the process's arguments and streams and
// returns the exit code. It never calls os.Exit itself.
func Execute(ctx context.Context, info BuildInfo) int {
	return run(ctx, newProcessDeps(info), os.Args[1:])
}

// deps are the process-level dependencies. Production fills them from
// the real process (newProcessDeps); tests build their own.
type deps struct {
	info      BuildInfo
	stdin     io.Reader
	stdinFile *os.File // the terminal for raw-mode keys; nil when absent
	stdout    io.Writer
	stderr    io.Writer
	stdinTTY  bool
	stdoutTTY bool
	stderrTTY bool
	getenv    func(string) string
	userHome  string
	now       func() time.Time
	services  services
}

func newProcessDeps(info BuildInfo) *deps {
	home, _ := os.UserHomeDir()
	return &deps{
		info:      info.resolved(),
		stdin:     os.Stdin,
		stdinFile: os.Stdin,
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		stdinTTY:  isTerminal(os.Stdin),
		stdoutTTY: isTerminal(os.Stdout),
		stderrTTY: isTerminal(os.Stderr),
		getenv:    os.Getenv,
		userHome:  home,
		now:       time.Now,
		services:  newProcessServices(),
	}
}

func isTerminal(f *os.File) bool { return f != nil && term.IsTerminal(int(f.Fd())) }

// app is one invocation: its dependencies, the parsed global flags, and the
// state resolved before the command runs.
type app struct {
	deps  *deps
	flags globalFlags
	out   *Output
	home  string
}

// run builds the tree, executes args, reports any error, and returns the
// exit code.
func run(ctx context.Context, d *deps, args []string) int {
	a := &app{deps: d}
	root := newRootCmd(a) //nolint:contextcheck // ctx reaches every command through ExecuteContextC below

	root.SetArgs(args)
	root.SetIn(d.stdin)
	root.SetOut(d.stdout)
	root.SetErr(d.stderr)

	cmd, err := root.ExecuteContextC(ctx)
	e := mapErr(err)
	if e == nil {
		return ExitOK
	}
	label := ""
	if cmd != nil {
		label = commandLabel(cmd.CommandPath())
	}
	if u := asUsageError(err); u != nil {
		label = commandLabel(u.cmdPath)
	}
	a.output(args).Fail(label, e)
	return e.Exit
}

// output returns the resolved Output, or, when the failure happened before
// the global flags were applied (a flag parse error), a best-effort one that
// honors -o/--output and --no-color as written in args.
func (a *app) output(args []string) *Output {
	if a.out != nil {
		return a.out
	}
	format, noColor := formatAuto, a.noColorEnv()
	for i, arg := range args {
		switch {
		case arg == "--"+flagNoColor:
			noColor = true
		case arg == "-o" || arg == "--output":
			if i+1 < len(args) {
				format = args[i+1]
			}
		case strings.HasPrefix(arg, "--output="):
			format = strings.TrimPrefix(arg, "--output=")
		case strings.HasPrefix(arg, "-o") && len(arg) > 2:
			format = arg[2:]
		}
	}
	if !slices.Contains([]string{formatAuto, formatText, formatJSON}, format) {
		format = formatAuto
	}
	out, _ := newOutput(outputOptions{
		format:    format,
		noColor:   noColor,
		stdout:    a.deps.stdout,
		stderr:    a.deps.stderr,
		stdoutTTY: a.deps.stdoutTTY,
		stderrTTY: a.deps.stderrTTY,
	})
	return out
}

func newRootCmd(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   rootName,
		Short: "Record Spotify tracks, albums and playlists into tagged M4A files",
		Long: `Mixtape records what Spotify plays on this Mac into one tagged M4A file per song.

Give it track, album or playlist links and it records only what is missing
from your library, copying songs it already has. With no links it records
whatever you play. Progress is tracked on disk, so a stopped session picks up
where it left off.

Humans get a live status line and single-key controls. Scripts and agents get
NDJSON events (-o json), detached sessions, and stable exit codes.

Start with 'mixtape setup', then 'mixtape doctor' to confirm you are ready.`,
		Example: `  mixtape setup
  mixtape record https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M
  mixtape record spotify:track:4cOdK2wGLETKBW3PvgPWqT --dry-run
  mixtape record --detach <link> && mixtape wait --timeout 9m`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       a.deps.info.Version,
	}
	root.Flags().Bool("version", false, "Print the version (as 'mixtape version' does)")
	root.SetVersionTemplate("mixtape {{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true
	a.addGlobalFlags(root)
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error { return a.prepare(cmd) }

	root.AddGroup(
		&cobra.Group{ID: groupRecording, Title: "Recording:"},
		&cobra.Group{ID: groupLibrary, Title: "Library:"},
		&cobra.Group{ID: groupSetup, Title: "Setup:"},
		&cobra.Group{ID: groupOther, Title: "Other:"},
	)
	root.SetHelpCommandGroupID(groupOther)

	root.AddCommand(
		newRecordCmd(a),
		newStatusCmd(a),
		newWaitCmd(a),
		newStopCmd(a),
		newListCmd(a),
		newVerifyCmd(a),
		newSetupCmd(a),
		newDoctorCmd(a),
		newDevicesCmd(a),
		newConfigCmd(a),
		newVersionCmd(a),
		newCompletionCmd(a),
		newDebugCmd(a),
	)
	// Registered after the persistent hooks are set: the update banner
	// chains onto them.
	attachUpdate(a, root)

	wireUsageErrors(root)
	return root
}

// prepare runs before every command: it validates the global flags and
// resolves the Output and the Mixtape home directory.
func (a *app) prepare(_ *cobra.Command) error {
	f := a.flags
	if f.verbose && f.quiet {
		return errFlagConflict
	}
	format := f.output
	if format == "" {
		format = a.deps.getenv(envOutput)
	}
	out, err := newOutput(outputOptions{
		format:    format,
		quiet:     f.quiet,
		verbose:   f.verbose,
		noColor:   f.noColor || a.noColorEnv(),
		stdout:    a.deps.stdout,
		stderr:    a.deps.stderr,
		stdoutTTY: a.deps.stdoutTTY,
		stderrTTY: a.deps.stderrTTY,
	})
	if err != nil {
		return err
	}
	a.out = out

	home := f.home
	if home == "" {
		home = a.deps.getenv(config.EnvHome)
	}
	if home == "" {
		home = config.DefaultHome(a.deps.userHome)
	}
	a.home = config.ExpandHome(home, a.deps.userHome)
	return nil
}

// noColorEnv honors the NO_COLOR convention (https://no-color.org) and
// dumb terminals.
func (a *app) noColorEnv() bool {
	return a.deps.getenv("NO_COLOR") != "" || a.deps.getenv("TERM") == "dumb"
}

// settings loads the config file and resolves every setting, with o
// carrying the command's own flag overrides (the global --library is
// applied here). Config warnings are printed once.
func (a *app) settings(o config.Overrides) (config.Settings, error) {
	file, warnings, err := config.Load(config.Path(a.home))
	if err != nil {
		return config.Settings{}, err
	}
	for _, w := range warnings {
		a.out.Warn("%s", w)
	}
	if a.flags.librarySet {
		lib := a.flags.library
		o.Library = &lib
	}
	return config.Resolve(file, a.deps.getenv, o, a.deps.userHome)
}
