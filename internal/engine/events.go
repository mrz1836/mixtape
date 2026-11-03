package engine

import (
	"time"

	"github.com/mrz1836/mixtape/internal/session"
)

// Event types, the public NDJSON contract of `record -o json`.
const (
	EventSessionStarted  = "session_started"
	EventPlan            = "plan"
	EventTrackStarted    = "track_started"
	EventTrackSaved      = "track_saved"
	EventTrackCopied     = "track_copied"
	EventTrackSkipped    = "track_skipped"
	EventTrackDiscarded  = "track_discarded"
	EventTrackFailed     = "track_failed"
	EventWarning         = "warning"
	EventSessionFinished = "session_finished"
	// EventBoundary is emitted with --verbose: the measured timing of a
	// track change, for tuning.
	EventBoundary = "boundary"
)

// Session outcomes.
const (
	OutcomeComplete   = session.ResultComplete
	OutcomeIncomplete = session.ResultIncomplete
	OutcomeStopped    = session.ResultStopped
)

// Event is one line of the session's event stream. Fields that do not
// apply to a type are omitted.
type Event struct {
	Type      string    `json:"type"`
	Time      time.Time `json:"time"`
	SessionID string    `json:"session_id,omitempty"`

	// session_started
	Mode    string `json:"mode,omitempty"`
	Capture string `json:"capture,omitempty"`
	Library string `json:"library,omitempty"`
	PID     int    `json:"pid,omitempty"`
	// Codec, BitDepth and SampleRate describe what the session records:
	// the codec it encodes to, and the capture's depth and rate (absent
	// when nothing plays).
	Codec      string `json:"codec,omitempty"`
	BitDepth   int    `json:"bit_depth,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`

	// plan
	Items []PlanItem `json:"items,omitempty"`

	// track_*
	Track *EventTrack `json:"track,omitempty"`

	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`

	// session_finished (and plan, as a preview)
	Counts   *session.Counts `json:"counts,omitempty"`
	Result   string          `json:"result,omitempty"`
	ExitCode *int            `json:"exit_code,omitempty"`

	// boundary (--verbose)
	OffsetSeconds *float64 `json:"offset_seconds,omitempty"`
	DriftSeconds  *float64 `json:"drift_seconds,omitempty"`
	Runs          int      `json:"runs,omitempty"`
}

// EventTrack identifies the song an event is about.
type EventTrack struct {
	ID          string `json:"id"`
	PlayedID    string `json:"played_id,omitempty"`
	Title       string `json:"title"`
	Artist      string `json:"artist,omitempty"`
	Collection  string `json:"collection,omitempty"`
	Position    int    `json:"position,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	File        string `json:"file,omitempty"`
	RecordedMS  int64  `json:"recorded_ms,omitempty"`
	AudioSHA256 string `json:"audio_sha256,omitempty"`
	Codec       string `json:"codec,omitempty"`
	CopiedFrom  string `json:"copied_from,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	Partial     bool   `json:"partial,omitempty"`
}

// PlanItem is one planned song in the plan event and `--dry-run`.
type PlanItem struct {
	Action     string `json:"action"`
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist,omitempty"`
	Collection string `json:"collection"`
	Position   int    `json:"position"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	From       string `json:"from,omitempty"`
	Hint       string `json:"hint,omitempty"`
	Reason     string `json:"reason,omitempty"` // other_codec: in the library, but not in the codec this session records
}

// itemTrack describes a planned song for events.
func itemTrack(it *Item) *EventTrack {
	return &EventTrack{
		ID: it.Track.ID, PlayedID: it.Track.PlayedID, Title: it.Track.Title, Artist: it.Track.Artist,
		Collection: it.Group.Spec.Name, Position: it.position(), DurationMS: it.Track.DurationMS,
		Attempt: it.attempts,
	}
}
