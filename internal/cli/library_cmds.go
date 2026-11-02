package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
)

var errVerify = errors.New("the library has problems")

// collectionDoc summarizes one library folder.
type collectionDoc struct {
	Folder    string    `json:"folder"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	SpotifyID string    `json:"spotify_id,omitempty"`
	URL       string    `json:"url,omitempty"`
	Songs     int       `json:"songs"`
	Done      int       `json:"done"`
	Pending   int       `json:"pending"`
	Failed    int       `json:"failed"`
	UpdatedAt time.Time `json:"updated_at"`
}

type listDoc struct {
	Library     string          `json:"library"`
	Collections []collectionDoc `json:"collections"`
}

// trackDoc is one song of `list LINK`.
type trackDoc struct {
	Position   int    `json:"position"`
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist,omitempty"`
	Status     string `json:"status"`
	Action     string `json:"next_action"`
	File       string `json:"file,omitempty"`
	FailReason string `json:"fail_reason,omitempty"`
}

type listLinkDoc struct {
	Library    string         `json:"library"`
	Collection *collectionDoc `json:"collection,omitempty"`
	Name       string         `json:"name"`
	Tracks     []trackDoc     `json:"tracks"`
}

func newListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "list [LINK]",
		Aliases: []string{"ls"},
		Short:   "List library folders, or the status of each song of one link",
		Long: `Without arguments, list every collection folder in the library with how many
of its songs are done, pending and failed.

With a link, list each song of that track, album or playlist with its status
in the library (recorded, copied, pending, failed, unavailable, or "new") and
what the next 'mixtape record' would do with it (skip, copy or record).`,
		Example: `  mixtape list
  mixtape list https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M
  mixtape ls -o json`,
		GroupID: groupLibrary,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := a.openLibrary(config.Overrides{})
			if err != nil {
				return err
			}
			if len(args) == 1 {
				return a.listLink(cmd, lib, args[0])
			}
			return a.listCollections(lib)
		},
	}
}

func summarize(c *library.Collection) collectionDoc {
	m := c.Manifest
	d := collectionDoc{
		Folder: c.Folder(), Kind: string(m.Kind), Name: m.Name, SpotifyID: m.SpotifyID, URL: m.URL,
		UpdatedAt: m.UpdatedAt,
	}
	for _, e := range m.Tracks {
		switch {
		case e.Status == library.StatusRemoved:
			continue
		case e.Status.Done() && !e.Partial:
			d.Done++
		case e.Status.Failed():
			d.Failed++
		default:
			d.Pending++
		}
		d.Songs++
	}
	return d
}

func (a *app) listCollections(lib *library.Library) error {
	doc := listDoc{Library: lib.Root(), Collections: []collectionDoc{}}
	for _, c := range lib.Collections() {
		doc.Collections = append(doc.Collections, summarize(c))
	}
	return a.out.Result(a.collectionsText(doc), doc)
}

// collectionsText renders `list` for people.
func (a *app) collectionsText(doc listDoc) string {
	if len(doc.Collections) == 0 {
		return "the library at " + doc.Library + " is empty: start with 'mixtape record <link>'"
	}
	rows := make([][]string, 0, len(doc.Collections))
	for _, d := range doc.Collections {
		progress := fmt.Sprintf("%d/%d", d.Done, d.Songs)
		if d.Failed > 0 {
			progress += a.out.paint(colorYellow, fmt.Sprintf(" (%d failed)", d.Failed))
		}
		rows = append(rows, []string{d.Folder, d.Kind, progress, d.Name})
	}
	return a.out.table([]string{"FOLDER", "KIND", "DONE", "NAME"}, rows)
}

func (a *app) listLink(cmd *cobra.Command, lib *library.Library, link string) error {
	refs, err := parseRefs(cmd.CommandPath(), []string{link})
	if err != nil {
		return err
	}
	cols, err := a.resolve(cmd.Context(), refs)
	if err != nil {
		return err
	}
	// One link is one folder: the plan has a single group.
	g := engine.BuildPlan(lib, cols, engine.PlanOptions{}).Groups[0]
	doc := listLinkDoc{Library: lib.Root(), Name: cols[0].Name, Tracks: []trackDoc{}}
	if g.Collection != nil {
		d := summarize(g.Collection)
		doc.Collection = &d
	}
	for i, it := range g.Items {
		td := trackDoc{
			Position: i + 1, ID: it.Track.ID, Title: it.Track.Title, Artist: it.Track.Artist,
			Status: "new", Action: string(it.Action),
		}
		if g.Collection != nil {
			if e := g.Collection.Entry(it.Track.ID); e != nil {
				td.Status, td.File, td.FailReason = string(e.Status), e.File, e.FailReason
				if e.Partial {
					td.Status = "partial"
				}
			}
		}
		doc.Tracks = append(doc.Tracks, td)
	}
	return a.out.Result(a.linkText(doc, g.Spec.Name), doc)
}

// linkText renders `list LINK` for people, under the folder's name.
func (a *app) linkText(doc listLinkDoc, folder string) string {
	var b strings.Builder
	b.WriteString(a.out.paint(colorBold, folder))
	for _, td := range doc.Tracks {
		fmt.Fprintf(&b, "\n  %3d  %-13s %s", td.Position, a.statusMark(td.Status), songLabel(td.Artist, td.Title))
		if td.FailReason != "" {
			b.WriteString(a.out.paint(colorDim, "  ("+td.FailReason+")"))
		}
	}
	return b.String()
}

// statusMark colors a song status for the terminal.
func (a *app) statusMark(status string) string {
	pad := fmt.Sprintf("%-13s", status)
	switch st := library.Status(status); {
	case st == library.StatusRecorded || st == library.StatusCopied:
		return a.out.paint(colorGreen, pad)
	case st.Failed():
		return a.out.paint(colorRed, pad)
	}
	return a.out.paint(colorDim, pad)
}

func newVerifyCmd(a *app) *cobra.Command {
	var fix, force bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check every file's tags and audio hash against the manifests",
		Long: `Check every recorded song in the library: the file exists, its tags name the
right Spotify track, and the SHA-256 of its audio matches the manifest. Files
that moved are found by their audio hash; files no manifest mentions are
reported as orphans.

--fix repairs what it can: a file moved within its folder is re-pointed, a
missing or damaged song goes back to pending (the next record run records
it again), and every .m3u8 playlist is regenerated. It refuses to run when
the library folder is missing or more than half of the songs are missing,
which looks like an unmounted drive; --force overrides that.

Exits 0 when the library is consistent and 1 when problems remain.`,
		Example: `  mixtape verify
  mixtape verify --fix
  mixtape verify -o json`,
		GroupID: groupLibrary,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := a.openLibrary(config.Overrides{})
			if err != nil {
				return err
			}
			if fix {
				unlock, lockErr := lib.Lock()
				if lockErr != nil {
					return lockErr
				}
				defer unlock()
			}
			rep, err := lib.Verify(cmd.Context(), fix, force)
			if err != nil {
				return err
			}
			if err := a.out.Result(a.verifyText(rep, fix), rep); err != nil {
				return err
			}
			if serious := seriousProblems(rep); serious > 0 {
				hint := "run 'mixtape verify --fix'"
				if fix {
					hint = "--fix leaves these to you: move a file back into its folder, or replace or delete a file " +
						"tagged for another song (then 'mixtape record' records what is missing)"
				}
				return newError(ExitError, errVerify, hint, "%d problem(s) in the library", serious)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "Repair manifests and playlists where possible")
	cmd.Flags().BoolVar(&force, "force", false, "With --fix: proceed even if the library looks unmounted")
	return cmd
}

// seriousProblems counts unfixed problems other than orphan files.
func seriousProblems(rep library.Report) int {
	fixed := map[string]bool{}
	for _, p := range rep.Fixed {
		fixed[p.Collection+"/"+p.File+"/"+p.Kind] = true
	}
	n := 0
	for _, p := range rep.Problems {
		if p.Kind != library.ProblemOrphan && !fixed[p.Collection+"/"+p.File+"/"+p.Kind] {
			n++
		}
	}
	return n
}

func (a *app) verifyText(rep library.Report, fix bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "checked %d song(s) in %d folder(s): %d ok", rep.Files, rep.Collections, rep.OK)
	for _, p := range rep.Problems {
		where := p.Collection
		if p.File != "" {
			where += "/" + p.File
		}
		fmt.Fprintf(&b, "\n  %s %s: %s", a.out.paint(colorYellow, fmt.Sprintf("%-15s", p.Kind)), where, p.Detail)
	}
	if fix {
		fmt.Fprintf(&b, "\nfixed %d", len(rep.Fixed))
	}
	return b.String()
}
