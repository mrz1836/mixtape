package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/mrz1836/mixtape/internal/fileutil"
)

// StateSchema is the session.json schema version.
const StateSchema = 1

// ErrNoSession reports that no session has run under this home.
var ErrNoSession = errors.New("no recording session found")

// Session phases, in order.
const (
	PhaseStarting  = "starting"
	PhaseRecording = "recording"
	PhasePaused    = "paused"
	PhaseStopping  = "stopping"
	PhaseFinished  = "finished"
)

// Session results, set when the phase is finished.
const (
	ResultComplete   = "complete"
	ResultIncomplete = "incomplete"
	ResultStopped    = "stopped"
	ResultError      = "error"
)

// State is the session.json document: everything `status` shows and
// `wait` and `stop` act on. It is rewritten atomically as the session
// progresses.
type State struct {
	Schema     int        `json:"schema"`
	ID         string     `json:"session_id"`
	PID        int        `json:"pid"`
	Phase      string     `json:"phase"`
	Mode       string     `json:"mode,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Library    string     `json:"library,omitempty"`
	Capture    string     `json:"capture,omitempty"`
	Targets    []string   `json:"targets,omitempty"`
	Current    *Current   `json:"current,omitempty"`
	Counts     Counts     `json:"counts"`
	// ETASeconds estimates the time left to record the remaining songs.
	ETASeconds int    `json:"eta_seconds,omitempty"`
	Result     string `json:"result,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Error      string `json:"error,omitempty"`
	Events     string `json:"events,omitempty"` // path of the NDJSON event log
	Log        string `json:"log,omitempty"`    // path of the text log (detached sessions)
	// Spotify holds the player settings the session changed, so they can
	// be put back even after a crash.
	Spotify *SpotifyState `json:"spotify,omitempty"`
}

// Current is the song being recorded.
type Current struct {
	TrackID    string  `json:"track_id,omitempty"`
	Title      string  `json:"title"`
	Artist     string  `json:"artist,omitempty"`
	Collection string  `json:"collection,omitempty"`
	Position   float64 `json:"position_seconds"`
	Duration   float64 `json:"duration_seconds,omitempty"`
	Recording  bool    `json:"recording"`
}

// Counts summarizes a session's songs.
type Counts struct {
	Total     int `json:"total"`
	Recorded  int `json:"recorded"`
	Copied    int `json:"copied"`
	Skipped   int `json:"skipped"`
	Failed    int `json:"failed"`
	Discarded int `json:"discarded"`
	Remaining int `json:"remaining"`
}

// Finished reports whether the session has ended.
func (s State) Finished() bool { return s.Phase == PhaseFinished }

// StatePath is session.json inside a Mixtape home.
func StatePath(home string) string { return filepath.Join(home, "session.json") }

// SessionsDir holds the per-session event logs.
func SessionsDir(home string) string { return filepath.Join(home, "sessions") }

// EventsPath is the NDJSON event log of one session.
func EventsPath(home, id string) string { return filepath.Join(SessionsDir(home), id+".ndjson") }

// LogPath is the text log of one detached session.
func LogPath(home, id string) string { return filepath.Join(SessionsDir(home), id+".log") }

// WriteState atomically replaces session.json.
func WriteState(home string, s State) error {
	s.Schema = StateSchema
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return fileutil.WriteAtomic(StatePath(home), append(data, '\n'), 0o600)
}

// ReadState reads session.json; a missing file is ErrNoSession.
func ReadState(home string) (State, error) {
	data, err := os.ReadFile(StatePath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, ErrNoSession
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("read %s: %w", StatePath(home), err)
	}
	return s, nil
}

// NewID returns a sortable, unique session ID such as
// "20261102-153045-a1b2c3".
func NewID(now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}
