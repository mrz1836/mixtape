package library

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mrz1836/mixtape/internal/fileutil"
	"github.com/mrz1836/mixtape/internal/mp4tag"
)

// Problem kinds reported by Verify.
const (
	ProblemMissing        = "missing"
	ProblemMoved          = "moved"
	ProblemHashMismatch   = "hash_mismatch"
	ProblemTagMismatch    = "tag_mismatch"
	ProblemUnreadable     = "unreadable"
	ProblemOrphan         = "orphan"
	ProblemStatusMismatch = "status_mismatch"
)

// Report is the result of Verify.
type Report struct {
	Collections int       `json:"collections"`
	Files       int       `json:"files"` // done entries checked
	OK          int       `json:"ok"`
	Problems    []Problem `json:"problems"`
	Fixed       []Problem `json:"fixed"`
}

// Problem is one inconsistency between a manifest and the files.
type Problem struct {
	Collection string `json:"collection"`
	File       string `json:"file,omitempty"`
	TrackID    string `json:"track_id,omitempty"`
	Kind       string `json:"kind"`
	Detail     string `json:"detail"`
}

// audioFile is one .m4a found while scanning the library.
type audioFile struct {
	hash    string
	codec   mp4tag.Codec // "" when it cannot be read
	tags    mp4tag.Tags
	readErr error
}

// Verify checks every done song: the file exists, its tags are readable
// and name the right track, and its audio hash matches the manifest. It
// finds files that moved (by hash) and files no manifest mentions.
//
// With fix it repairs what it can: a moved file inside its own folder is
// re-pointed, a missing or corrupt song goes back to pending so the next
// run records it, a stale manifest hash is refreshed, and every playlist
// is regenerated. Fixing requires the caller to hold Lock. It is refused
// with ErrUnsafeFix, unless force is set, when the library folder is
// missing or more than half of the done songs are missing: that looks
// like an unmounted drive, not lost files.
func (l *Library) Verify(ctx context.Context, fix, force bool) (Report, error) {
	r := Report{Collections: len(l.cols), Problems: []Problem{}, Fixed: []Problem{}}
	if _, err := os.Stat(l.root); err != nil {
		r.Problems = append(r.Problems, Problem{Kind: ProblemMissing, Detail: "library folder is missing: " + l.root})
		if fix && !force {
			return r, ErrUnsafeFix
		}
		return r, nil
	}
	files, err := l.scanAudio(ctx)
	if err != nil {
		return r, err
	}

	v := l.index(files)
	for _, c := range l.cols {
		v.checkCollection(c)
	}
	v.findOrphans()
	r.Files, r.OK = v.checked, v.ok
	for _, f := range v.found {
		r.Problems = append(r.Problems, f.p)
	}
	sortProblems(r.Problems)
	if !fix {
		return r, nil
	}
	if err = v.safeToFix(force); err != nil {
		return r, err
	}
	err = l.applyFixes(v.found, &r)
	return r, err
}

// verifier is the state of one Verify pass over the scanned files.
type verifier struct {
	l     *Library
	files map[string]audioFile // every .m4a under the root, by path
	paths []string             // the keys of files, sorted
	// referenced holds the paths manifest entries name; unreferenced
	// lists the other files by audio hash, in path order.
	referenced   map[string]bool
	unreferenced map[string][]string
	claimed      map[string]bool // unreferenced files explained as moved
	found        []finding
	// checked counts the done songs checked, ok the ones without a
	// problem and missing the ones whose file is nowhere to be found.
	checked, ok, missing int
}

// finding is a problem and the change that fixes it; apply is nil for a
// problem that is only reported.
type finding struct {
	p     Problem
	apply func()
}

// index sorts out the scanned files: which ones manifest entries name, and
// the others by audio hash, so a song whose file is not where its entry
// says can be found by its hash. Paths are taken in sorted order so that,
// when several unreferenced files share a hash, every run picks the same
// one.
func (l *Library) index(files map[string]audioFile) *verifier {
	v := &verifier{
		l: l, files: files, paths: slices.Sorted(maps.Keys(files)),
		referenced: map[string]bool{}, unreferenced: map[string][]string{}, claimed: map[string]bool{},
	}
	for _, c := range l.cols {
		for i := range c.Manifest.Tracks {
			if p := c.Path(&c.Manifest.Tracks[i]); p != "" {
				v.referenced[p] = true
			}
		}
	}
	for _, path := range v.paths {
		if f := files[path]; !v.referenced[path] && f.hash != "" {
			v.unreferenced[f.hash] = append(v.unreferenced[f.hash], path)
		}
	}
	return v
}

func (v *verifier) add(p Problem, apply func()) { v.found = append(v.found, finding{p, apply}) }

// checkCollection checks every entry of c against the files.
func (v *verifier) checkCollection(c *Collection) {
	for i := range c.Manifest.Tracks {
		e := &c.Manifest.Tracks[i]
		p := Problem{Collection: c.Folder(), File: e.File, TrackID: e.RequestedID}
		if e.Status.Done() {
			v.checkDone(c, e, p)
		} else {
			v.checkNotDone(c, e, p)
		}
	}
}

// checkNotDone finds a song the manifest does not count as done although
// its intact file is in place. Changed audio stays pending (see
// hashMismatch), as reconcile leaves it: a second --fix must not take it
// back.
func (v *verifier) checkNotDone(c *Collection, e *Entry, p Problem) {
	f, ok := v.files[c.Path(e)]
	if !ok || f.readErr != nil || e.Status == StatusRemoved || !taggedFor(f.tags, e) || !f.intact() {
		return
	}
	p.Kind, p.Detail = ProblemStatusMismatch, fmt.Sprintf("file exists but status is %s", e.Status)
	hash, codec := f.hash, f.codec
	v.add(p, func() { e.Status, e.AudioSHA256, e.Codec, e.FailReason = StatusRecorded, hash, codec, "" })
}

// checkDone checks a done song's file: there, readable, tagged for it, and
// holding the audio the manifest recorded.
func (v *verifier) checkDone(c *Collection, e *Entry, p Problem) {
	v.checked++
	f, ok := v.files[c.Path(e)]
	switch {
	case !ok:
		v.notInPlace(c, e, p)
	case f.readErr != nil:
		p.Kind, p.Detail = ProblemUnreadable, f.readErr.Error()
		v.add(p, func() { e.Status, e.FailReason = StatusPending, "unreadable file (verify)" })
	case !taggedFor(f.tags, e):
		p.Kind = ProblemTagMismatch
		p.Detail = fmt.Sprintf("file is tagged %s=%q", TagSpotifyID, f.tags.Freeform[TagSpotifyID])
		v.add(p, nil)
	case f.hash != e.AudioSHA256:
		v.hashMismatch(e, f, p)
	default:
		v.ok++
	}
}

// notInPlace handles a done song whose file is not where its entry says:
// moved, when an unreferenced file holds its audio, or else missing. A
// file found outside the song's own folder is only reported; moving it
// back is left to the owner.
func (v *verifier) notInPlace(c *Collection, e *Entry, p Problem) {
	if moved := v.unreferenced[e.AudioSHA256]; e.AudioSHA256 != "" && len(moved) > 0 {
		to := moved[0]
		v.claimed[to] = true
		p.Kind, p.Detail = ProblemMoved, "found at "+v.l.rel(to)
		if filepath.Dir(to) != c.Dir {
			p.Detail += " (outside its folder; move it back to fix)"
			v.add(p, nil)
			return
		}
		name, codec := filepath.Base(to), v.files[to].codec
		v.add(p, func() { e.File, e.Codec = name, codec })
		return
	}
	v.missing++ // a song found elsewhere proves the drive is mounted: only lost ones count
	p.Kind, p.Detail = ProblemMissing, "file not found; the next run records it again"
	v.add(p, func() {
		e.Status, e.File, e.AudioSHA256, e.Codec, e.Partial = StatusPending, "", "", "", false
		e.FailReason = "file missing (verify)"
	})
}

// hashMismatch handles a file whose audio hash is not the manifest's.
// When the manifest has no hash, or the file's own hash tag agrees with
// its audio, the manifest is what is stale (a redo whose manifest write
// was lost) and is refreshed. Otherwise the audio changed after it was
// recorded, and the song goes back to pending.
func (v *verifier) hashMismatch(e *Entry, f audioFile, p Problem) {
	p.Kind = ProblemHashMismatch
	hash, codec := f.hash, f.codec
	if e.AudioSHA256 == "" || f.tags.Freeform[TagAudioSHA256] == f.hash {
		p.Detail = "manifest hash is stale"
		v.add(p, func() { e.AudioSHA256, e.Codec = hash, codec })
		return
	}
	p.Detail = "audio changed since it was recorded; the next run records it again"
	v.add(p, func() { e.Status, e.FailReason = StatusPending, "audio changed (verify)" })
}

// findOrphans reports the files in a collection folder that no entry names
// and no song claimed as moved. Files outside every collection folder are
// not the library's, and are not reported.
func (v *verifier) findOrphans() {
	for _, path := range v.paths {
		if v.referenced[path] || v.claimed[path] {
			continue
		}
		for _, c := range v.l.cols {
			if filepath.Dir(path) == c.Dir {
				v.add(Problem{
					Collection: c.Folder(), File: filepath.Base(path), Kind: ProblemOrphan,
					Detail: "no manifest entry mentions this file",
				}, nil)
			}
		}
	}
}

// safeToFix is the safety gate of --fix: unless forced, it refuses to send
// more than half of the done songs back to pending, which looks like an
// unmounted drive, not lost files.
func (v *verifier) safeToFix(force bool) error {
	if force || v.checked == 0 || v.missing*2 <= v.checked {
		return nil
	}
	return fmt.Errorf("%w (%d of %d songs missing; pass --force if they are really gone)", ErrUnsafeFix, v.missing, v.checked)
}

// applyFixes applies every fix found, records it in r, and saves every
// collection, which also regenerates its playlist.
func (l *Library) applyFixes(found []finding, r *Report) error {
	for _, f := range found {
		if f.apply != nil {
			f.apply()
			r.Fixed = append(r.Fixed, f.p)
		}
	}
	sortProblems(r.Fixed)
	for _, c := range l.cols {
		if err := l.Save(c); err != nil {
			return err
		}
	}
	return nil
}

// intact reports whether a readable file's audio matches its own hash tag
// (a file without one, tagged by another tool, counts as intact).
func (f audioFile) intact() bool {
	tagged := f.tags.Freeform[TagAudioSHA256]
	return tagged == "" || tagged == f.hash
}

// scanAudio reads the tags and audio hash of every .m4a under the root.
func (l *Library) scanAudio(ctx context.Context) (map[string]audioFile, error) {
	files := map[string]audioFile{}
	err := filepath.WalkDir(l.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // an unreadable subtree is skipped, not fatal
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		name := d.Name()
		if d.IsDir() {
			if path != l.root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(name), m4aExt) || fileutil.IsTemp(name) {
			return nil
		}
		var f audioFile
		f.tags, f.readErr = mp4tag.ReadFile(path)
		if f.readErr == nil {
			f.hash, f.readErr = mp4tag.HashFile(path)
		}
		if f.readErr == nil {
			f.codec, _ = mp4tag.ReadCodec(path) // a file another tool made may not say
		}
		files[path] = f
		return nil
	})
	return files, err
}

func sortProblems(ps []Problem) {
	slices.SortStableFunc(ps, func(a, b Problem) int {
		return cmp.Or(cmp.Compare(a.Collection, b.Collection), cmp.Compare(a.File, b.File), cmp.Compare(a.Kind, b.Kind))
	})
}
