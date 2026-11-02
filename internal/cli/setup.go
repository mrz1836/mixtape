package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/spotify"
)

var errProbeSilent = errors.New("captured only silence")

type devicesDoc struct {
	Tap           capture.TapStatus `json:"tap"`
	DefaultOutput string            `json:"default_output,omitempty"`
	Devices       []deviceDoc       `json:"devices"`
}

type deviceDoc struct {
	Name          string `json:"name"`
	Input         bool   `json:"input"`
	Output        bool   `json:"output"`
	DefaultInput  bool   `json:"default_input,omitempty"`
	DefaultOutput bool   `json:"default_output,omitempty"`
}

func newDevicesCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "List audio devices and whether the process tap is available",
		Long: `List the Mac's audio devices (capture inputs such as BlackHole 2ch, and
outputs such as a Multi-Output Device), mark the defaults, and report
whether the Core Audio process tap can be used on this Mac (macOS 14.2+).`,
		Example: `  mixtape devices
  mixtape devices -o json`,
		GroupID: groupSetup,
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			svc := a.deps.services
			doc := devicesDoc{Tap: svc.tapStatus(), Devices: []deviceDoc{}}
			devs, err := svc.devices()
			if err != nil {
				return err
			}
			var b strings.Builder
			tap := "available"
			if !doc.Tap.Supported {
				tap = "unavailable: " + doc.Tap.Reason
			}
			fmt.Fprintf(&b, "%s %s", a.out.paint(colorBold, "process tap:"), tap)
			for _, d := range devs {
				dd := deviceDoc{Name: d.Name, Input: d.Input, Output: d.Output, DefaultInput: d.DefaultInput, DefaultOutput: d.DefaultOutput}
				doc.Devices = append(doc.Devices, dd)
				if d.DefaultOutput {
					doc.DefaultOutput = d.Name
				}
				var tags []string
				if d.Input {
					tags = append(tags, "input")
				}
				if d.Output {
					tags = append(tags, "output")
				}
				if d.DefaultInput {
					tags = append(tags, "default input")
				}
				if d.DefaultOutput {
					tags = append(tags, "default output")
				}
				fmt.Fprintf(&b, "\n  %-32s %s", d.Name, a.out.paint(colorDim, strings.Join(tags, ", ")))
			}
			return a.out.Result(b.String(), doc)
		},
	}
}

func newSetupCmd(a *app) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Guided first-time setup: capture method, permissions, library",
		Long: `Walk through everything recording needs, fixing what it can:

  1. Capture method. The process tap (macOS 14.2+) records Spotify alone,
     with nothing to install; macOS asks once for audio-recording permission
     for your terminal app. The alternative is BlackHole 2ch plus a
     Multi-Output Device; setup can install BlackHole with Homebrew and
     opens Audio MIDI Setup with the steps.
  2. A short test capture while Spotify plays proves the method works.
  3. The Spotify Automation prompt, crossfade and volume.
  4. The library folder.

Only a verified capture method is saved to the config. --yes accepts every
default. Without a terminal, setup prints the readiness checks and the steps
instead.

The tap permission belongs to the app that runs mixtape (Terminal, iTerm, an
IDE), so it is not available over ssh or from launchd; use the device
method there.`,
		Example: `  mixtape setup
  mixtape setup --yes`,
		GroupID: groupSetup,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.out.JSON() || !a.deps.stdinTTY || !a.deps.stdoutTTY {
				return a.setupNonInteractive(cmd.Context())
			}
			return (&setupFlow{a: a, yes: yes, in: bufio.NewReader(a.deps.stdin)}).run(cmd.Context())
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Accept every default without asking")
	return cmd
}

// setupNonInteractive prints the checks and the manual steps.
func (a *app) setupNonInteractive(ctx context.Context) error {
	doc := a.diagnose(ctx)
	text := a.doctorText(doc) + "\n\n" + setupSteps
	if err := a.out.Result(text, doc); err != nil {
		return err
	}
	if !doc.Ready {
		return newError(ExitNotReady, errNotReady, "run 'mixtape setup' in a terminal, or follow the steps above",
			"not ready: %d check(s) failed", failed(doc.Checks))
	}
	return nil
}

const setupSteps = `Manual setup:
  1. Spotify: Settings → Playback → turn off "Crossfade songs"; set Spotify's volume to the maximum.
  2. Capture, either:
     a. Process tap (macOS 14.2+): mixtape config set capture tap — the first recording asks
        macOS for audio-recording permission for your terminal app.
     b. BlackHole: brew install blackhole-2ch; in Audio MIDI Setup create a Multi-Output Device
        with your speakers and BlackHole 2ch, select it as the sound output; then
        mixtape config set capture device
  3. mixtape config set library ~/Music/Mixtape   (optional)
  4. mixtape doctor`

// setupFlow is one interactive setup.
type setupFlow struct {
	a   *app
	yes bool
	in  *bufio.Reader
}

func (s *setupFlow) say(format string, args ...any) { s.a.out.Println(format, args...) }

// ask asks a yes/no question; def is the answer for Enter and --yes.
func (s *setupFlow) ask(question string, def bool) bool {
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	if s.yes {
		s.say("%s %s %s", question, hint, map[bool]string{true: "y", false: "n"}[def])
		return def
	}
	_, _ = fmt.Fprintf(s.a.deps.stdout, "%s %s ", question, hint)
	line, err := s.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes":
		return true
	}
	return false
}

// prompt asks for a value with a default.
func (s *setupFlow) prompt(question, def string) string {
	if s.yes {
		s.say("%s [%s] %s", question, def, def)
		return def
	}
	_, _ = fmt.Fprintf(s.a.deps.stdout, "%s [%s] ", question, def)
	line, _ := s.in.ReadString('\n')
	if v := strings.TrimSpace(line); v != "" {
		return v
	}
	return def
}

// pause waits for Enter.
func (s *setupFlow) pause(message string) {
	if s.yes {
		s.say("%s", message)
		return
	}
	_, _ = fmt.Fprintf(s.a.deps.stdout, "%s [Enter] ", message)
	_, _ = s.in.ReadString('\n')
}

func (s *setupFlow) run(ctx context.Context) error {
	a := s.a
	svc := a.deps.services
	s.say("%s", a.out.paint(colorBold, "Mixtape setup"))

	if _, ok := svc.spotifyApp(a.deps.userHome); !ok {
		return spotify.ErrNotInstalled
	}
	if running, _ := svc.spotifyRunning(); !running {
		s.pause("Open Spotify, then press Enter.")
	}
	s.say("Checking that mixtape can control Spotify (macOS may ask you to allow it)…")
	if snap := svc.player().Poll(ctx); snap.Err != nil {
		return snap.Err
	}

	settings, err := a.settings(config.Overrides{})
	if err != nil {
		return err
	}
	method, err := s.chooseCapture(ctx, settings)
	if err != nil {
		return err
	}
	library := s.prompt("Library folder for recordings?", settings.Library)

	if c := a.checkCrossfade(); c.Status == checkFail {
		s.say("%s %s\n  %s", a.out.paint(colorRed, "✗"), c.Detail, c.Fix)
		s.pause("Turn crossfade off, then press Enter.")
	}

	path := config.Path(a.home)
	if err := config.Set(path, config.KeyCapture, string(method), a.deps.userHome); err != nil {
		return err
	}
	if err := config.Set(path, config.KeyLibrary, library, a.deps.userHome); err != nil {
		return err
	}
	s.say("saved capture = %s and library = %s to %s", method, library, path)

	doc := a.diagnose(ctx)
	s.say("\n%s", a.doctorText(doc))
	if !doc.Ready {
		return newError(ExitNotReady, errNotReady, "fix the failed checks above and run 'mixtape doctor'",
			"setup finished, but %d check(s) still fail", failed(doc.Checks))
	}
	s.say("\nTry it: mixtape record <a Spotify track link>")
	return nil
}

// chooseCapture picks the capture method and proves it with a test
// capture. Only a verified method is returned.
func (s *setupFlow) chooseCapture(ctx context.Context, settings config.Settings) (capture.Method, error) {
	a := s.a
	tapSt := a.deps.services.tapStatus()
	if tapSt.Supported && s.ask("Use the process tap (records Spotify only, nothing to install)?", true) {
		if err := s.verify(ctx, captureRequest{Method: capture.MethodTap}); err == nil {
			return capture.MethodTap, nil
		} else {
			s.say("%s the tap test failed: %v", a.out.paint(colorRed, "✗"), err)
			s.say("  allow your terminal app in System Settings → Privacy & Security → Screen & System Audio Recording.")
			if s.ask("Try the tap again?", true) {
				if err := s.verify(ctx, captureRequest{Method: capture.MethodTap}); err == nil {
					return capture.MethodTap, nil
				}
			}
		}
	} else if !tapSt.Supported {
		s.say("The process tap is unavailable here (%s); using BlackHole.", tapSt.Reason)
	}
	return s.setupDevice(ctx, settings)
}

// setupDevice installs and routes BlackHole, then verifies it.
func (s *setupFlow) setupDevice(ctx context.Context, settings config.Settings) (capture.Method, error) {
	a := s.a
	svc := a.deps.services
	devs, _ := svc.devices()
	if findDevice(devs, settings.Device) == nil {
		if !s.ask("BlackHole 2ch is not installed. Install it with Homebrew now (brew install blackhole-2ch)?", true) {
			return "", fmt.Errorf("%w: no capture device", errNotReady)
		}
		if err := svc.runScript(ctx, "brew", "install", "blackhole-2ch"); err != nil {
			return "", fmt.Errorf("brew install blackhole-2ch: %w", err)
		}
	}
	if c := a.routingCheck(settings); c.Status != checkOK {
		s.say("Route Spotify's audio through BlackHole:\n" +
			"  1. In Audio MIDI Setup click +, then Create Multi-Output Device.\n" +
			"  2. Tick your speakers or headphones and BlackHole 2ch (speakers first, as the clock source).\n" +
			"  3. In System Settings → Sound, select the Multi-Output Device as the output.")
		_ = svc.runScript(ctx, "open", "-a", "Audio MIDI Setup")
		s.pause("Press Enter when the Multi-Output Device is the sound output.")
	}
	if err := s.verify(ctx, captureRequest{Method: capture.MethodDevice, Device: settings.Device}); err != nil {
		return "", fmt.Errorf("the BlackHole test failed: %w", err)
	}
	return capture.MethodDevice, nil
}

// routingCheck is the output-routing check alone.
func (a *app) routingCheck(s config.Settings) check {
	s.Capture = string(capture.MethodDevice)
	_, checks := a.checkCapture(s)
	for _, c := range checks {
		if c.ID == "routing" {
			return c
		}
	}
	return check{Status: checkWarn}
}

// verify captures a few seconds while Spotify plays and requires audio.
func (s *setupFlow) verify(ctx context.Context, r captureRequest) error {
	s.pause("Start playing a song in Spotify (any song), then press Enter to test the capture.")
	s.say("Listening for 4 seconds…")
	src, err := s.a.deps.services.openCapture(r)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	peak, err := probe(ctx, src, 4*time.Second, s.a.deps.services.sleep)
	if err != nil {
		return err
	}
	if peak <= 64 {
		return errProbeSilent
	}
	s.say("%s heard audio (peak level %d)", s.a.out.paint(colorGreen, "✓"), peak)
	return nil
}

// probe captures for d and returns the loudest sample seen.
func probe(ctx context.Context, src capture.Source, d time.Duration, sleep func(context.Context, time.Duration) bool) (int, error) {
	var mu sync.Mutex
	loudest := 0
	err := src.Start(ctx, func(b capture.Block) {
		p := pcmPeak(b.PCM[:min(len(b.PCM), b.Frames*src.Format().BytesPerFrame())])
		mu.Lock()
		loudest = max(loudest, p)
		mu.Unlock()
	})
	if err != nil {
		return 0, err
	}
	sleep(ctx, d)
	mu.Lock()
	defer mu.Unlock()
	return loudest, nil
}

// pcmPeak is the largest sample magnitude in PCM16LE data.
func pcmPeak(pcm []byte) int {
	m := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(pcm[i]) | int(pcm[i+1])<<8
		if v >= 1<<15 {
			v = 1<<16 - v
		}
		m = max(m, v)
	}
	return m
}
