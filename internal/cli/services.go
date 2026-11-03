package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/capture/tap"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// player is the Spotify control surface commands use: the engine's needs
// (which include what restoring settings after a crash takes), plus
// resuming playback for setup's test capture.
type player interface {
	engine.Player
	Play(ctx context.Context) error
}

// catalog resolves links to their track lists.
type catalog interface {
	Resolve(ctx context.Context, ref spotify.Ref) (spotify.Collection, error)
	ReleaseDate(ctx context.Context, trackID string) (string, error)
}

// captureRequest says how to open a capture source.
type captureRequest struct {
	Method capture.Method // tap or device (already selected)
	Device string         // device name substring
	Mute   bool           // tap only
	Bits   int            // bits a sample: 16 or 24 (config.Settings.Bits)

	// The output rate to run the default output at while the tap records
	// (0 leaves it alone), and what the switch needs (see tap.Options).
	OutputRate  int
	Pending     *capture.OutputRestore
	SaveRestore func(capture.OutputRestore) error
	Warn        func(msg string)
	SessionID   string
}

// services are the heavyweight collaborators commands reach through the
// app, so tests can substitute fakes. Production values come from
// newProcessServices.
type services struct {
	player      func() player
	catalog     func() catalog
	openCapture func(context.Context, captureRequest) (capture.Source, error)
	tapStatus   func() capture.TapStatus
	// output reads and sets output device rates (doctor, crash recovery).
	output         func() capture.OutputControl
	devices        func() ([]device.Info, error)
	defaultOutput  func() (device.Info, error)
	spotifyRunning func() (bool, error)
	spotifyApp     func(userHome string) (string, bool)
	prefs          func(userHome string) (spotify.Prefs, error)
	lockPath       func() (string, error)
	executable     func() (string, error)
	lookPath       func(name string) (string, error)
	// encoder stands in for afconvert: given the encoder the settings call
	// for, it returns the one to use. nil encodes with afconvert itself.
	encoder func(library.AFConvert) library.Encoder
	// afconvertHelp is afconvert's -hf listing of the formats it writes.
	afconvertHelp func(ctx context.Context) string
	// signals delivers SIGINT and SIGTERM until its stop func is called,
	// and makes the process ignore SIGHUP and SIGPIPE.
	signals   func() (<-chan os.Signal, func())
	spawn     func(exe string, args, env []string, logPath string) (*os.Process, error)
	kill      func(pid int, sig syscall.Signal) error
	sleep     func(ctx context.Context, d time.Duration) bool
	runScript func(ctx context.Context, name string, args ...string) error
	// permission checks a macOS privacy permission without prompting;
	// requestPermission shows the system prompt where macOS allows one.
	permission        func(permission.Kind) permission.State
	requestPermission func(permission.Kind) permission.State
	hostApp           func() permission.HostApp
	// keepAwake stops the Mac from idle-sleeping until release is called
	// (or the process exits).
	keepAwake func() (release func())
}

func newProcessServices() services {
	return services{
		player:         func() player { return spotify.NewPlayer(nil) },
		catalog:        func() catalog { return spotify.NewCatalog() },
		openCapture:    openCapture,
		tapStatus:      tap.Available,
		output:         tap.OutputControl,
		devices:        device.List,
		defaultOutput:  device.DefaultOutput,
		spotifyRunning: spotify.IsRunning,
		spotifyApp:     spotify.InstalledPath,
		prefs:          spotify.ReadPrefs,
		lockPath:       session.DefaultLockPath,
		executable:     os.Executable,
		lookPath:       exec.LookPath,
		afconvertHelp:  afconvertHelp,
		signals: func() (<-chan os.Signal, func()) {
			// A closed terminal or pipe must not kill a session mid-song.
			signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
			ch := make(chan os.Signal, 4)
			signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
			return ch, func() { signal.Stop(ch) }
		},
		spawn:     session.Spawn,
		kill:      syscall.Kill,
		keepAwake: caffeinate,
		sleep: func(ctx context.Context, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		},
		permission:        permission.Check,
		requestPermission: permission.Request,
		hostApp:           permission.App,
		runScript: func(ctx context.Context, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed programs (brew, open) with constant arguments
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			return cmd.Run()
		},
	}
}

// openCapture opens the real backend for a selected method.
func openCapture(ctx context.Context, r captureRequest) (capture.Source, error) {
	switch r.Method {
	case capture.MethodTap:
		return tap.Open(ctx, tap.Options{
			Mute: r.Mute, Wait: 10 * time.Second, Bits: r.Bits,
			OutputRate: r.OutputRate, Pending: r.Pending, SaveRestore: r.SaveRestore, Warn: r.Warn, SessionID: r.SessionID,
		})
	case capture.MethodDevice:
		return device.Open(r.Device, r.Bits)
	case capture.MethodAuto:
	}
	return nil, fmt.Errorf("%w: %q", capture.ErrUnknownMethod, r.Method)
}

// afconvertHelp runs afconvert -hf. It exits non-zero even when it lists
// everything, so only its output counts.
func afconvertHelp(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "afconvert", "-hf").CombinedOutput()
	return string(out)
}

// caffeinate holds macOS's idle-sleep assertion for this process with the
// system's own caffeinate tool. Its -w flag ends the assertion when this
// process exits, even if it is killed. Without the tool, nothing is held.
func caffeinate() func() {
	// Not tied to a context: it lives until release, or until this process
	// exits (-w), whichever comes first.
	cmd := exec.CommandContext(context.Background(), "caffeinate", "-i", "-w", strconv.Itoa(os.Getpid())) //nolint:gosec // G204: fixed system tool, our own PID
	if err := cmd.Start(); err != nil {
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
