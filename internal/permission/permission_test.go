package permission

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHostFromPath(t *testing.T) {
	t.Parallel()
	tests := map[string]HostApp{
		"/Applications/iTerm.app/Contents/MacOS/iTerm2": {Name: "iTerm", Path: "/Applications/iTerm.app"},
		"/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal": {
			Name: "Terminal", Path: "/System/Applications/Utilities/Terminal.app",
		},
		"/Applications/Visual Studio Code.app/Contents/Frameworks/Code Helper.app/Contents/MacOS/Code Helper": {
			Name: "Visual Studio Code", Path: "/Applications/Visual Studio Code.app",
		},
		"/usr/sbin/sshd": {Name: "sshd", Path: "/usr/sbin/sshd"},
		"":               {},
	}
	for path, want := range tests {
		assert.Equal(t, want, hostFromPath(path), path)
	}
}

func TestSettingsAndInstructions(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "x-apple.systempreferences:com.apple.preference.security?Privacy_AudioCapture", SettingsURL(SystemAudio))
	assert.Contains(t, SettingsURL(Microphone), "Privacy_Microphone")
	assert.Contains(t, SettingsURL(Automation), "Privacy_Automation")

	iterm := HostApp{Name: "iTerm"}
	assert.Contains(t, Instructions(SystemAudio, iterm), "add iTerm under")
	assert.Contains(t, Instructions(SystemAudio, iterm), "System Audio Recording Only")
	assert.Contains(t, Instructions(Microphone, HostApp{}), "the app that runs mixtape")
	assert.Contains(t, Instructions(Automation, iterm), "control Spotify")
}

func TestFromCode(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Granted, fromCode(0))
	assert.Equal(t, Denied, fromCode(1))
	assert.Equal(t, Undecided, fromCode(2))
	assert.Equal(t, Unknown, fromCode(-1))
}

// TestChecksAnswer exercises the real checks: they must return a valid
// state without prompting, whatever this machine allows.
func TestChecksAnswer(t *testing.T) {
	t.Parallel()
	valid := []State{Granted, Denied, Undecided, Unknown}
	for _, k := range []Kind{SystemAudio, Microphone, Automation, ScreenRecording} {
		st := Check(k)
		t.Logf("%s: %s", k, st)
		assert.Contains(t, valid, st, k)
	}
	t.Logf("host app: %+v", App())
}
