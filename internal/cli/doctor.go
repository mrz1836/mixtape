package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// Check outcomes.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

// check is one readiness check.
type check struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type doctorDoc struct {
	Ready   bool    `json:"ready"`
	Capture string  `json:"capture"`
	Checks  []check `json:"checks"`
}

func newDoctorCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that everything needed to record is in place",
		Long: `Check every prerequisite for recording and say how to fix what is missing:
macOS version, the Spotify app (installed, running, Automation permission),
crossfade (must be off), volume, the capture method (process tap, or
BlackHole with the output routed through it), afconvert, the library folder,
the config file, and any running or interrupted session.

Exits 0 when ready and 3 when a check fails. Warnings do not block.`,
		Example: `  mixtape doctor
  mixtape doctor -o json`,
		GroupID: groupSetup,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc := a.diagnose(cmd.Context())
			if err := a.out.Result(a.doctorText(doc), doc); err != nil {
				return err
			}
			if !doc.Ready {
				return newError(ExitNotReady, errNotReady, "fix the failed checks above, or run 'mixtape setup'",
					"not ready: %d check(s) failed", failed(doc.Checks))
			}
			return nil
		},
	}
}

// diagnose runs every readiness check.
func (a *app) diagnose(ctx context.Context) doctorDoc {
	svc := a.deps.services
	var checks []check
	add := func(c check) { checks = append(checks, c) }

	add(a.checkMacOS())
	settings, cfgCheck := a.checkConfig()
	add(cfgCheck)

	appPath, installed := svc.spotifyApp(a.deps.userHome)
	if installed {
		add(check{ID: "spotify_installed", Name: "Spotify app", Status: checkOK, Detail: appPath})
	} else {
		add(check{
			ID: "spotify_installed", Name: "Spotify app", Status: checkFail, Detail: "not installed",
			Fix: "install Spotify from https://www.spotify.com/download",
		})
	}
	running, _ := svc.spotifyRunning()
	snap := spotify.Snapshot{Err: spotify.ErrNotRunning}
	if running {
		add(check{ID: "spotify_running", Name: "Spotify running", Status: checkOK, Detail: "running"})
		pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		snap = svc.player().Poll(pctx)
		cancel()
		add(automationCheck(snap))
	} else {
		add(check{
			ID: "spotify_running", Name: "Spotify running", Status: checkFail, Detail: "not running",
			Fix: "open Spotify (open -a Spotify)",
		})
		add(check{
			ID: "automation", Name: "Automation permission", Status: checkSkip,
			Detail: "checked once Spotify is running",
		})
	}
	add(a.checkCrossfade())
	add(volumeCheck(snap))

	method, capChecks := a.checkCapture(settings)
	checks = append(checks, capChecks...)

	if _, err := svc.lookPath("afconvert"); err == nil || svc.encoder != nil {
		add(check{ID: "afconvert", Name: "afconvert", Status: checkOK, Detail: "found"})
	} else {
		add(check{
			ID: "afconvert", Name: "afconvert", Status: checkFail, Detail: "not found",
			Fix: "afconvert ships with macOS; check /usr/bin/afconvert",
		})
	}
	add(checkLibrary(settings.Library))
	add(a.checkSession())

	return doctorDoc{Ready: failed(checks) == 0, Capture: string(method), Checks: checks}
}

// failed counts the checks that failed.
func failed(checks []check) int {
	n := 0
	for _, c := range checks {
		if c.Status == checkFail {
			n++
		}
	}
	return n
}

func (a *app) checkMacOS() check {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil || v == "" {
		return check{ID: "macos", Name: "macOS", Status: checkWarn, Detail: "version unknown"}
	}
	return check{ID: "macos", Name: "macOS", Status: checkOK, Detail: "macOS " + v}
}

func (a *app) checkConfig() (config.Settings, check) {
	c := check{ID: "config", Name: "Config file", Status: checkOK, Detail: config.Path(a.home)}
	file, warnings, err := config.Load(config.Path(a.home))
	if err != nil {
		c.Status, c.Detail, c.Fix = checkFail, err.Error(), "fix the JSON, or delete the file to start over"
		file = config.File{}
	} else if len(warnings) > 0 {
		c.Status, c.Detail = checkWarn, strings.Join(warnings, "; ")
	}
	o := config.Overrides{}
	if a.flags.librarySet {
		o.Library = &a.flags.library
	}
	s, err := config.Resolve(file, a.deps.getenv, o, a.deps.userHome)
	if err != nil {
		c.Status, c.Detail, c.Fix = checkFail, err.Error(), "fix the value with 'mixtape config set'"
		s, _ = config.Resolve(config.File{}, func(string) string { return "" }, config.Overrides{}, a.deps.userHome)
	}
	return s, c
}

func automationCheck(snap spotify.Snapshot) check {
	c := check{ID: "automation", Name: "Automation permission", Status: checkOK, Detail: "mixtape can control Spotify"}
	switch {
	case snap.Err == nil:
	case errors.Is(snap.Err, spotify.ErrNotAuthorized):
		c.Status, c.Detail = checkFail, "macOS denied control of Spotify"
		c.Fix = "System Settings → Privacy & Security → Automation → (your terminal) → enable Spotify"
	case errors.Is(snap.Err, spotify.ErrPromptPending):
		c.Status, c.Detail, c.Fix = checkWarn, "macOS is asking for permission", "approve the Automation prompt on screen"
	case errors.Is(snap.Err, spotify.ErrAppleEventsBlocked):
		c.Status, c.Detail = checkFail, "Apple Events to Spotify are blocked (a sandbox?)"
		c.Fix = "run mixtape from a normal terminal session"
	default:
		c.Status, c.Detail = checkWarn, snap.Err.Error()
	}
	return c
}

func (a *app) checkCrossfade() check {
	c := check{ID: "crossfade", Name: "Crossfade off", Status: checkOK, Detail: "off"}
	prefs, err := a.deps.services.prefs(a.deps.userHome)
	switch {
	case err != nil:
		c.Status, c.Detail = checkWarn, "could not read Spotify's preferences: "+err.Error()
	case prefs.Path == "":
		c.Status, c.Detail = checkWarn, "Spotify's preferences were not found; make sure crossfade is off"
	case prefs.Stale:
		c.Status = checkWarn
		c.Detail = "this Spotify version keeps crossfade where mixtape cannot read it; make sure it is off " +
			"(recording stops if songs keep ending early)"
		c.Fix = "Spotify → Settings → Playback → \"Crossfade songs\" off"
	case prefs.Crossfade:
		c.Status, c.Detail = checkFail, fmt.Sprintf("crossfade is on (%.1fs): songs overlap and cannot be cut", prefs.CrossfadeSeconds)
		c.Fix = "Spotify → Settings → Playback → turn off \"Crossfade songs\""
	case prefs.Automix:
		c.Status, c.Detail = checkFail, "automix is on: songs blend into each other"
		c.Fix = "Spotify → Settings → Playback → turn off \"Automix\""
	}
	return c
}

func volumeCheck(snap spotify.Snapshot) check {
	c := check{ID: "volume", Name: "Spotify volume", Status: checkSkip, Detail: "checked once Spotify is reachable"}
	if snap.Err != nil {
		return c
	}
	c.Status, c.Detail = checkOK, fmt.Sprintf("%d%%", snap.Volume)
	if snap.Volume < 100 {
		c.Status = checkWarn
		c.Detail = fmt.Sprintf("%d%%: recordings are made at Spotify's volume", snap.Volume)
		c.Fix = "set Spotify's volume slider to the maximum (the Mac's own volume does not matter)"
	}
	return c
}

// checkCapture reports the tap, the capture device and the routing, and
// which method record would use.
func (a *app) checkCapture(s config.Settings) (capture.Method, []check) {
	svc := a.deps.services
	var checks []check
	tapSt := svc.tapStatus()
	tapCheck := check{ID: "tap", Name: "Process tap", Status: checkOK, Detail: "available (records Spotify only, nothing to install)"}
	if !tapSt.Supported {
		tapCheck.Status, tapCheck.Detail = checkWarn, "unavailable: "+tapSt.Reason
	}

	requested, _ := capture.ParseMethod(s.Capture)
	method, err := capture.Select(requested, tapSt)
	methodCheck := check{
		ID: "capture", Name: "Capture method", Status: checkOK,
		Detail: fmt.Sprintf("%s (setting: %s)", method, s.Capture),
	}
	if err != nil {
		methodCheck.Status, methodCheck.Detail = checkFail, err.Error()
		methodCheck.Fix = "mixtape config set capture device (BlackHole), or auto"
	}
	checks = append(checks, methodCheck, tapCheck)
	if method == capture.MethodTap {
		tapCheck.Fix = "the first recording asks macOS for audio-recording permission for your terminal app"
		checks[1] = tapCheck
		return method, checks
	}

	devs, derr := svc.devices()
	dev := check{ID: "device", Name: "Capture device", Status: checkOK}
	found := findDevice(devs, s.Device)
	switch {
	case derr != nil:
		dev.Status, dev.Detail = checkFail, derr.Error()
	case found == nil:
		dev.Status, dev.Detail = checkFail, fmt.Sprintf("no capture device matching %q", s.Device)
		dev.Fix = "brew install blackhole-2ch, then 'mixtape setup'"
	default:
		dev.Detail = found.Name
	}
	checks = append(checks, dev)
	route := check{ID: "routing", Name: "Output routing", Status: checkOK}
	out, oerr := svc.defaultOutput()
	switch {
	case oerr != nil:
		route.Status, route.Detail = checkWarn, "could not read the default output: "+oerr.Error()
	case strings.Contains(strings.ToLower(out.Name), "multi-output") ||
		(found != nil && strings.EqualFold(out.Name, found.Name)):
		route.Detail = "system output is " + out.Name
	default:
		route.Status, route.Detail = checkFail, "system output is "+out.Name+", so the capture device hears nothing"
		route.Fix = "create a Multi-Output Device (speakers + BlackHole) in Audio MIDI Setup and select it as the output"
	}
	return method, append(checks, route)
}

func findDevice(devs []device.Info, match string) *device.Info {
	for i := range devs {
		if devs[i].Input && strings.Contains(strings.ToLower(devs[i].Name), strings.ToLower(match)) {
			return &devs[i]
		}
	}
	return nil
}

// checkLibrary checks that the library folder is writable without
// creating it: a missing folder is fine when its parent is writable.
func checkLibrary(root string) check {
	c := check{ID: "library", Name: "Library folder", Status: checkOK, Detail: root}
	dir := root
	for {
		if info, err := os.Stat(dir); err == nil {
			if !info.IsDir() {
				c.Status, c.Detail, c.Fix = checkFail, dir+" is not a folder", "choose another: mixtape config set library <path>"
				return c
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if dir != root {
		c.Detail = root + " (created on first use)"
	}
	f, err := os.CreateTemp(dir, ".mixtape-doctor-*")
	if err != nil {
		c.Status, c.Detail, c.Fix = checkFail, dir+" is not writable: "+err.Error(), "fix its permissions or choose another folder"
		return c
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return c
}

func (a *app) checkSession() check {
	c := check{ID: "session", Name: "Recording session", Status: checkOK, Detail: "none running"}
	st, running, err := a.current()
	switch {
	case err != nil && !errors.Is(err, session.ErrNoSession):
		c.Status, c.Detail = checkWarn, err.Error()
	case running:
		c.Status, c.Detail = checkWarn, fmt.Sprintf("session %s is recording (pid %d)", st.ID, st.PID)
		c.Fix = "'mixtape stop' ends it; a new session cannot start meanwhile"
	case err == nil && !st.Finished():
		c.Status, c.Detail = checkWarn, "session "+st.ID+" was interrupted"
		if st.Spotify.NeedsRestore() {
			c.Detail += "; Spotify settings it changed are restored by the next 'mixtape record'"
		}
	}
	return c
}

func (a *app) doctorText(doc doctorDoc) string {
	var b strings.Builder
	for i, c := range doc.Checks {
		if i > 0 {
			b.WriteByte('\n')
		}
		var mark string
		switch c.Status {
		case checkOK:
			mark = a.out.paint(colorGreen, "✓")
		case checkWarn:
			mark = a.out.paint(colorYellow, "!")
		case checkFail:
			mark = a.out.paint(colorRed, "✗")
		default:
			mark = a.out.paint(colorDim, "·")
		}
		fmt.Fprintf(&b, "%s %-22s %s", mark, c.Name, c.Detail)
		if c.Fix != "" && c.Status != checkOK {
			fmt.Fprintf(&b, "\n    %s %s", a.out.paint(colorDim, "fix:"), c.Fix)
		}
	}
	if doc.Ready {
		fmt.Fprintf(&b, "\n%s", a.out.paint(colorGreen, "ready to record ("+doc.Capture+" capture)"))
	} else {
		fmt.Fprintf(&b, "\n%s", a.out.paint(colorRed, fmt.Sprintf("not ready: %d check(s) failed", failed(doc.Checks))))
	}
	return b.String()
}
