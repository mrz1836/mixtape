package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/permission"
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
				return notReady(failed(doc.Checks), "fix the failed checks above, or run 'mixtape setup'")
			}
			return nil
		},
	}
}

// diagnose runs every readiness check.
func (a *app) diagnose(ctx context.Context) doctorDoc {
	checks := []check{a.checkMacOS()}
	settings, cfgCheck := a.checkConfig()
	checks = append(checks, cfgCheck)
	spotifyChecks, snap := a.checkSpotify(ctx)
	checks = append(checks, spotifyChecks...)
	checks = append(checks, a.checkCrossfade(), volumeCheck(snap))

	method, capChecks := a.checkCapture(settings)
	checks = append(checks, capChecks...)

	if a.afconvertFound() {
		checks = append(checks, check{ID: checkIDAfconvert, Name: "afconvert", Status: checkOK, Detail: "found"})
	} else {
		checks = append(checks, check{
			ID: checkIDAfconvert, Name: "afconvert", Status: checkFail, Detail: "not found", Fix: fixAfconvert,
		})
	}
	checks = append(checks, checkLibrary(settings.Library), a.checkSession())
	return doctorDoc{Ready: failed(checks) == 0, Capture: string(method), Checks: checks}
}

// checkSpotify reports the Spotify app (installed, running) and whether
// mixtape may control it, with the snapshot taken to find out (an error
// snapshot when Spotify was not asked).
func (a *app) checkSpotify(ctx context.Context) ([]check, spotify.Snapshot) {
	svc := a.deps.services
	var checks []check
	if appPath, installed := svc.spotifyApp(a.deps.userHome); installed {
		checks = append(checks, check{ID: checkIDSpotifyApp, Name: "Spotify app", Status: checkOK, Detail: appPath})
	} else {
		checks = append(checks, check{
			ID: checkIDSpotifyApp, Name: "Spotify app", Status: checkFail, Detail: "not installed", Fix: fixInstallSpotify,
		})
	}
	snap := spotify.Snapshot{Err: spotify.ErrNotRunning}
	running := check{ID: checkIDSpotifyRunning, Name: "Spotify running", Status: checkOK, Detail: "running"}
	proc, procErr := a.spotifyStatus()
	switch proc {
	case spotifyStopped:
		running.Status, running.Detail, running.Fix = checkFail, "not running", fixOpenSpotify
	case spotifyUnknown:
		// Polling never launches Spotify, so it still tells whether it runs.
		running.Status, running.Detail = checkWarn, "could not check Spotify: "+procErr.Error()
	case spotifyRunning:
	}
	checks = append(checks, running)
	if proc == spotifyStopped {
		return append(checks, check{
			ID: checkIDAutomation, Name: "Automation permission", Status: checkSkip,
			Detail: "checked once Spotify is running",
		}), snap
	}
	// Ask macOS first: polling Spotify while Automation is undecided would
	// put a prompt on screen, and doctor never prompts.
	if c, decided := a.automationState(); decided {
		return append(checks, c), snap
	}
	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	snap = svc.player().Poll(pctx)
	cancel()
	return append(checks, automationCheck(snap)), snap
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
	v, err := osProductVersion()
	if err != nil || v == "" {
		return check{ID: checkIDMacOS, Name: "macOS", Status: checkWarn, Detail: "version unknown"}
	}
	return check{ID: checkIDMacOS, Name: "macOS", Status: checkOK, Detail: "macOS " + v}
}

// checkConfig reports the config file and returns the settings the other
// checks use: the defaults, where the file or a value is unusable.
func (a *app) checkConfig() (config.Settings, check) {
	c := check{ID: checkIDConfig, Name: "Config file", Status: checkOK, Detail: config.Path(a.home)}
	s, warnings, err := a.loadSettings(config.Overrides{})
	switch {
	case errors.Is(err, config.ErrInvalidValue):
		c.Status, c.Detail, c.Fix = checkFail, err.Error(), "fix the value with 'mixtape config set'"
	case err != nil:
		c.Status, c.Detail, c.Fix = checkFail, err.Error(), "fix the JSON, or delete the file to start over"
	case len(warnings) > 0:
		c.Status, c.Detail = checkWarn, strings.Join(warnings, "; ")
	}
	if s.Sources == nil { // nothing resolved: carry on with the defaults
		s, _ = config.Resolve(config.File{}, nil, config.Overrides{}, a.deps.userHome)
	}
	return s, c
}

// automationState reports the Automation permission when macOS gives a
// definite answer that rules out polling (undecided or denied); decided is
// false when Spotify should be polled to find out.
func (a *app) automationState() (c check, decided bool) {
	app := a.deps.services.hostApp()
	c = check{ID: checkIDAutomation, Name: "Automation permission", Fix: permission.Instructions(permission.Automation, app)}
	switch a.deps.services.permission(permission.Automation) {
	case permission.Denied:
		c.Status, c.Detail = checkFail, app.String()+" may not "+permissionVerb(permission.Automation)
		return c, true
	case permission.Undecided:
		c.Status = checkWarn
		c.Detail = "macOS has not been asked yet whether " + app.String() + " may " + permissionVerb(permission.Automation)
		c.Fix = "run 'mixtape setup': it shows the prompt"
		return c, true
	case permission.Granted, permission.Unknown:
	}
	return check{}, false
}

// permissionCheck reports a capture permission for the host app.
func (a *app) permissionCheck(k permission.Kind) check {
	app := a.deps.services.hostApp()
	c := check{ID: string(k), Name: "Audio recording", Status: checkOK}
	if k == permission.Microphone {
		c.Name = "Microphone access"
	}
	what := permissionVerb(k)
	switch a.deps.services.permission(k) {
	case permission.Granted:
		c.Detail = app.String() + " may " + what
	case permission.Undecided:
		c.Status, c.Detail = checkFail, app.String()+" has not been allowed to "+what
		c.Fix = permission.Instructions(k, app) + " (or run 'mixtape setup')"
		if k == permission.Microphone {
			c.Status, c.Fix = checkWarn, "macOS asks the first time; 'mixtape setup' shows the prompt"
		}
		if k == permission.SystemAudio && a.deps.services.permission(permission.ScreenRecording) == permission.Granted {
			c.Detail += "; it is on in the screen-recording list, which does not cover the tap"
		}
	case permission.Denied:
		c.Status, c.Detail = checkFail, app.String()+" is not allowed to "+what
		c.Fix = permission.Instructions(k, app)
	case permission.Unknown:
		c.Status, c.Detail = checkWarn, "macOS did not say whether "+app.String()+" may "+what
		c.Fix = "'mixtape setup' checks with a short test capture"
	}
	return c
}

func automationCheck(snap spotify.Snapshot) check {
	c := check{ID: checkIDAutomation, Name: "Automation permission", Status: checkOK, Detail: "mixtape can control Spotify"}
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
	c := check{ID: checkIDCrossfade, Name: "Crossfade off", Status: checkOK, Detail: "off"}
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
		c.Fix = fixCrossfade
	case blending(prefs) != "":
		c.Status, c.Detail = checkFail, blending(prefs)+": songs blend into each other and cannot be cut"
		c.Fix = blendingFix(prefs)
	}
	return c
}

func volumeCheck(snap spotify.Snapshot) check {
	c := check{ID: checkIDVolume, Name: "Spotify volume", Status: checkSkip, Detail: "checked once Spotify is reachable"}
	if snap.Err != nil {
		return c
	}
	c.Status, c.Detail = checkOK, fmt.Sprintf("%d%%", snap.Volume)
	if snap.Volume < 100 {
		c.Status = checkWarn
		c.Detail = fmt.Sprintf("%d%%: recordings are made at Spotify's volume", snap.Volume)
		c.Fix = fixVolume
	}
	return c
}

// checkCapture reports the tap, the capture device and the routing, and
// which method record would use.
func (a *app) checkCapture(s config.Settings) (capture.Method, []check) {
	svc := a.deps.services
	var checks []check
	tapSt := svc.tapStatus()
	tapCheck := check{ID: checkIDTap, Name: "Process tap", Status: checkOK, Detail: "available (records Spotify only, nothing to install)"}
	if !tapSt.Supported {
		tapCheck.Status, tapCheck.Detail = checkWarn, "unavailable: "+tapSt.Reason
	}

	method, err := capture.Select(s.Capture, tapSt)
	methodCheck := check{
		ID: checkIDCapture, Name: "Capture method", Status: checkOK,
		Detail: fmt.Sprintf("%s (setting: %s)", method, s.Capture),
	}
	if err != nil {
		methodCheck.Status, methodCheck.Detail = checkFail, err.Error()
		methodCheck.Fix = "mixtape config set capture device (BlackHole), or auto"
	}
	checks = append(checks, methodCheck, tapCheck)
	if method == capture.MethodTap {
		return method, append(checks, a.permissionCheck(capturePermission(method)))
	}

	devs, derr := svc.devices()
	dev := check{ID: checkIDDevice, Name: "Capture device", Status: checkOK}
	found := findDevice(devs, s.Device)
	switch {
	case derr != nil:
		dev.Status, dev.Detail = checkFail, derr.Error()
	case found == nil:
		dev.Status, dev.Detail = checkFail, fmt.Sprintf("no capture device matching %q", s.Device)
		dev.Fix = fixInstallDevice
	default:
		dev.Detail = found.Name
	}
	checks = append(checks, dev)
	route := check{ID: checkIDRouting, Name: "Output routing", Status: checkOK}
	out, oerr := svc.defaultOutput()
	switch {
	case oerr != nil:
		route.Status, route.Detail = checkWarn, "could not read the default output: "+oerr.Error()
	case strings.Contains(strings.ToLower(out.Name), "multi-output") ||
		(found != nil && strings.EqualFold(out.Name, found.Name)):
		route.Detail = "system output is " + out.Name
	default:
		route.Status, route.Detail = checkFail, "system output is "+out.Name+", so the capture device hears nothing"
		route.Fix = fixRouting
	}
	if method == capture.MethodDevice {
		checks = append(checks, a.permissionCheck(capturePermission(method)))
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
	c := check{ID: checkIDLibrary, Name: "Library folder", Status: checkOK, Detail: root}
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
	c := check{ID: checkIDSession, Name: "Recording session", Status: checkOK, Detail: "none running"}
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
