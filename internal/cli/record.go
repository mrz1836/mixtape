package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// recordFlags holds `record`'s own flags.
type recordFlags struct {
	detach, dryRun, redo, keepPartial, keepWAV, mute, allowCrossfade bool
	artwork                                                          bool
	limit, maxFailures                                               int
	match, into, capture, device                                     string
	idleTimeout                                                      time.Duration
}

// How long `record --detach` waits for the background session to start:
// a base, plus a catalog lookup per link.
const (
	detachWait        = 30 * time.Second
	spotifyLookupWait = 15 * time.Second
)

var (
	errCrossfade   = errors.New("spotify crossfade is on, so songs overlap and cannot be cut cleanly")
	errNoAfconvert = errors.New("afconvert (macOS) was not found")
	// errTapHearsNothing is a silent tap while macOS reports the audio
	// permission granted: the sound goes somewhere else.
	errTapHearsNothing = errors.New("spotify is playing, but no sound reaches mixtape (the audio permission is granted)")
)

func newRecordCmd(a *app) *cobra.Command {
	f := &recordFlags{}
	cmd := &cobra.Command{
		Use:   "record [LINK|URI...]",
		Short: "Record tracks, albums and playlists (no links: whatever plays)",
		Long: `Record Spotify tracks, albums or playlists into the library, one tagged M4A
per song. Only what is missing is recorded: songs already in the folder are
skipped, songs elsewhere in the library are copied, and an interrupted run
picks up where it stopped. Partial songs are discarded unless --keep-partial.

Links may be open.spotify.com URLs (any ?si= share link) or spotify: URIs.
Track links go to the Singles folder (or --into). With no links, mixtape
records whatever you play, and presses "next" on songs it already has.

Spotify must be running, and crossfade and automix must be off (Settings →
Playback): blended songs overlap and cannot be cut. Shuffle is turned off for the
session and restored afterwards.

On a terminal: [space] pause/resume, [n] skip the song, [q] quit (Ctrl-C
twice forces it). With -o json (or when piped) the session streams NDJSON
events on stdout: session_started, plan, track_started, track_saved,
track_copied, track_skipped, track_discarded, track_failed, warning and
session_finished.

For agents and scripts: start with --detach, which prints the session ID and
returns at once, then poll 'mixtape wait --timeout 9m' (exit 124 means still
running) or 'mixtape status -o json', and end with 'mixtape stop'.

Exit codes: 0 complete or stopped, 2 usage, 3 not ready, 4 link not found,
5 permission, 6 another session is running, 7 finished with failed songs.`,
		Example: `  mixtape record https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M
  mixtape record spotify:album:4LH4d3cOWNNsVw41Gqt2kv --dry-run
  mixtape record spotify:track:4cOdK2wGLETKBW3PvgPWqT --into "Party"
  mixtape record --match "remix"
  mixtape record --detach <link> && mixtape wait --timeout 9m`,
		GroupID: groupRecording,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runRecord(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.detach, "detach", false, "Run in the background; print the session ID and return")
	fl.BoolVar(&f.dryRun, "dry-run", false, "Print the plan (record, copy, skip) and change nothing")
	fl.BoolVar(&f.redo, "redo", false, "Record again songs that are already done")
	fl.IntVar(&f.limit, "limit", 0, "Stop after recording this many songs (0 = no limit)")
	fl.StringVar(&f.match, "match", "", "Live mode: record only songs whose title contains this text")
	fl.StringVar(&f.into, "into", "", "Folder for track links and live recordings (default Singles)")
	fl.BoolVar(&f.keepPartial, "keep-partial", false, "Keep songs not captured start to finish (flagged; replaced next run)")
	fl.BoolVar(&f.keepWAV, "keep-wav", false, "Keep each song's WAV next to the M4A work files")
	fl.BoolVar(&f.artwork, "artwork", false, "Embed album artwork (config: artwork)")
	fl.StringVar(&f.capture, "capture", "", "Capture method: auto, tap or device (config: capture)")
	fl.StringVar(&f.device, "device", "", "Capture device name for --capture device (config: device)")
	fl.BoolVar(&f.mute, "mute", false, "Tap only: silence Spotify on the speakers while recording")
	fl.DurationVar(&f.idleTimeout, "idle-timeout", 0, "End after this long with nothing recording (config: idle_timeout)")
	fl.IntVar(&f.maxFailures, "max-failures", config.DefaultMaxFailures,
		"End after this many failed attempts in a row (a song is tried twice); 0 = never (config: max_failures)")
	fl.BoolVar(&f.allowCrossfade, "allow-crossfade", false, "Record even though Spotify's crossfade or automix is on")
	return cmd
}

// overrides turns the flags that were set into config overrides.
func (f *recordFlags) overrides(cmd *cobra.Command) config.Overrides {
	var o config.Overrides
	fl := cmd.Flags()
	if fl.Changed("capture") {
		o.Capture = &f.capture
	}
	if fl.Changed("device") {
		o.Device = &f.device
	}
	if fl.Changed("artwork") {
		o.Artwork = &f.artwork
	}
	if fl.Changed("idle-timeout") {
		o.IdleTimeout = &f.idleTimeout
	}
	if fl.Changed("max-failures") {
		o.MaxFailures = &f.maxFailures
	}
	return o
}

func (a *app) runRecord(cmd *cobra.Command, args []string, f *recordFlags) error {
	cmdPath := cmd.CommandPath()
	refs, err := parseRefs(cmdPath, args)
	if err != nil {
		return err
	}
	if f.limit < 0 {
		return &usageError{cmdPath: cmdPath, err: fmt.Errorf("%w: --limit must be 0 or more", errInvalidFlag)}
	}
	lib, settings, err := a.openLibrary(f.overrides(cmd))
	if err != nil {
		return err
	}
	if f.detach && a.deps.getenv(session.EnvDetached) == "" {
		if f.dryRun {
			return &usageError{cmdPath: cmdPath, err: fmt.Errorf("%w: --detach and --dry-run cannot be combined", errInvalidFlag)}
		}
		return a.detach(cmd.Context(), childFlags(cmd), args)
	}
	ctx := cmd.Context()
	cols, err := a.resolve(ctx, refs)
	if err != nil {
		return err
	}
	plan := engine.BuildPlan(lib, cols, engine.PlanOptions{Redo: f.redo, Into: f.into})
	if f.dryRun {
		doc := newPlanDoc(plan, settings.Library)
		return a.out.Result(a.planText(doc, plan), doc)
	}
	return a.record(ctx, &recordRun{
		cmdPath: cmdPath, flags: f, settings: settings, lib: lib, plan: plan, args: args,
	})
}

// parseRefs validates every link argument of the command at cmdPath.
func parseRefs(cmdPath string, args []string) ([]spotify.Ref, error) {
	refs := make([]spotify.Ref, 0, len(args))
	for _, arg := range args {
		ref, err := spotify.ParseRef(arg)
		if err != nil {
			return nil, &usageError{cmdPath: cmdPath, err: err}
		}
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// resolve looks every link up in Spotify's catalog.
func (a *app) resolve(ctx context.Context, refs []spotify.Ref) ([]spotify.Collection, error) {
	cat := a.catalog()
	cols := make([]spotify.Collection, 0, len(refs))
	for _, ref := range refs {
		col, err := cat.Resolve(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref.URL(), err)
		}
		switch {
		case col.Source == spotify.SourceOEmbed && ref.Kind != spotify.KindTrack:
			a.out.Warn("%q: the track list is unavailable; mixtape will play it in Spotify and follow it", col.Name)
		case col.Truncated:
			a.out.Warn("%q lists more than %d songs; mixtape will follow the rest in Spotify", col.Name, len(col.Tracks))
		}
		cols = append(cols, col)
	}
	return cols, nil
}

// catalog is the command's one Spotify catalog client: links and release
// dates share its connections.
func (a *app) catalog() catalog {
	if a.cat == nil {
		a.cat = a.deps.services.catalog()
	}
	return a.cat
}

// openLibrary resolves the settings (o carries the command's flags) and
// opens the library they name.
func (a *app) openLibrary(o config.Overrides) (*library.Library, config.Settings, error) {
	s, err := a.settings(o)
	if err != nil {
		return nil, config.Settings{}, err
	}
	lib, err := library.Open(s.Library, a.libraryOptions())
	if err != nil {
		return nil, config.Settings{}, err
	}
	return lib, s, nil
}

func (a *app) libraryOptions() library.Options {
	return library.Options{
		Encoder:     a.deps.services.encoder,
		WorkDir:     filepath.Join(a.home, "work"),
		Now:         a.deps.now,
		Warn:        func(msg string) { a.out.Warn("%s", msg) },
		ReleaseDate: a.catalog().ReleaseDate,
	}
}

// planDoc is the JSON shape of `record --dry-run`.
type planDoc struct {
	Mode    string            `json:"mode"`
	Library string            `json:"library"`
	Items   []engine.PlanItem `json:"items"`
	Counts  map[string]int    `json:"counts"`
}

func newPlanDoc(plan *engine.Plan, root string) planDoc {
	doc := planDoc{Mode: plan.Mode, Library: root, Items: plan.Summary(), Counts: map[string]int{}}
	if doc.Items == nil {
		doc.Items = []engine.PlanItem{}
	}
	for _, it := range doc.Items {
		doc.Counts[it.Action]++
	}
	return doc
}

// planText renders a dry run for people.
func (a *app) planText(doc planDoc, plan *engine.Plan) string {
	if doc.Mode == engine.ModeLive {
		return "Live mode: mixtape records whatever Spotify plays into " + plan.Groups[0].Spec.Name
	}
	var b strings.Builder
	var recordMS int64
	for _, it := range doc.Items {
		if it.Action == string(engine.ActionRecord) {
			recordMS += it.DurationMS
		}
	}
	fmt.Fprintf(&b, "%d songs: %d to record (%s), %d to copy, %d already done",
		len(doc.Items), doc.Counts[string(engine.ActionRecord)], mmss(ms(recordMS)),
		doc.Counts[string(engine.ActionCopy)], doc.Counts[string(engine.ActionSkip)])
	if n := doc.Counts[string(engine.ActionEncode)]; n > 0 {
		fmt.Fprintf(&b, ", %d to re-encode", n)
	}
	group := ""
	for _, it := range doc.Items {
		if it.Collection != group {
			group = it.Collection
			fmt.Fprintf(&b, "\n%s", a.out.paint(colorBold, group))
		}
		line := fmt.Sprintf("\n  %-7s %3d  %s", it.Action, it.Position, songLabel(it.Artist, it.Title))
		if it.DurationMS > 0 {
			line += a.out.paint(colorDim, " ("+mmss(ms(it.DurationMS))+")")
		}
		if it.From != "" {
			line += a.out.paint(colorDim, "  ← "+it.From)
		}
		if it.Hint != "" {
			line += a.out.paint(colorYellow, "  ["+it.Hint+"]")
		}
		b.WriteString(line)
	}
	if doc.Mode == engine.ModeFollow {
		b.WriteString("\n" + a.out.paint(colorDim, "…and the rest of the playlist, followed in Spotify"))
	}
	return b.String()
}
