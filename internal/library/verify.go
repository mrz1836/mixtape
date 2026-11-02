package library

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	referenced := map[string]bool{}
	for _, c := range l.cols {
		for i := range c.Manifest.Tracks {
			if p := c.Path(&c.Manifest.Tracks[i]); p != "" {
				referenced[p] = true
			}
		}
	}
	unreferencedByHash := map[string][]string{}
	for path, f := range files {
		if !referenced[path] && f.hash != "" {
			unreferencedByHash[f.hash] = append(unreferencedByHash[f.hash], path)
		}
	}

	type pending struct {
		p     Problem
		apply func()
	}
	var found []pending
	claimed := map[string]bool{} // unreferenced files explained as moved
	done, missing := 0, 0
	for _, c := range l.cols {
		for i := range c.Manifest.Tracks {
			e := &c.Manifest.Tracks[i]
			p := Problem{Collection: c.Folder(), File: e.File, TrackID: e.RequestedID}
			if !e.Status.Done() {
				if f, ok := files[c.Path(e)]; ok && f.readErr == nil && e.Status != StatusRemoved && taggedFor(f.tags, e) {
					p.Kind, p.Detail = ProblemStatusMismatch, fmt.Sprintf("file exists but status is %s", e.Status)
					hash := f.hash
					found = append(found, pending{p, func() { e.Status, e.AudioSHA256, e.FailReason = StatusRecorded, hash, "" }})
				}
				continue
			}
			done++
			r.Files++
			f, ok := files[c.Path(e)]
			if !ok {
				missing++
				if moved := unreferencedByHash[e.AudioSHA256]; e.AudioSHA256 != "" && len(moved) > 0 {
					to := moved[0]
					claimed[to] = true
					p.Kind, p.Detail = ProblemMoved, "found at "+l.rel(to)
					if filepath.Dir(to) == c.Dir {
						name := filepath.Base(to)
						found = append(found, pending{p, func() { e.File = name }})
					} else {
						p.Detail += " (outside its folder; move it back to fix)"
						found = append(found, pending{p, nil})
					}
					continue
				}
				p.Kind, p.Detail = ProblemMissing, "file not found; the next run records it again"
				found = append(found, pending{p, func() {
					e.Status, e.File, e.AudioSHA256, e.Partial = StatusPending, "", "", false
					e.FailReason = "file missing (verify)"
				}})
				continue
			}
			switch {
			case f.readErr != nil:
				p.Kind, p.Detail = ProblemUnreadable, f.readErr.Error()
				found = append(found, pending{p, func() { e.Status, e.FailReason = StatusPending, "unreadable file (verify)" }})
			case !taggedFor(f.tags, e):
				p.Kind = ProblemTagMismatch
				p.Detail = fmt.Sprintf("file is tagged %s=%q", TagSpotifyID, f.tags.Freeform[TagSpotifyID])
				found = append(found, pending{p, nil})
			case f.hash != e.AudioSHA256:
				p.Kind = ProblemHashMismatch
				hash := f.hash
				if e.AudioSHA256 == "" || f.tags.Freeform[TagAudioSHA256] == f.hash {
					p.Detail = "manifest hash is stale"
					found = append(found, pending{p, func() { e.AudioSHA256 = hash }})
				} else {
					p.Detail = "audio changed since it was recorded; the next run records it again"
					found = append(found, pending{p, func() { e.Status, e.FailReason = StatusPending, "audio changed (verify)" }})
				}
			default:
				r.OK++
			}
		}
	}

	for path := range files {
		if referenced[path] || claimed[path] {
			continue
		}
		for _, c := range l.cols {
			if filepath.Dir(path) == c.Dir {
				found = append(found, pending{Problem{
					Collection: c.Folder(), File: filepath.Base(path), Kind: ProblemOrphan,
					Detail: "no manifest entry mentions this file",
				}, nil})
			}
		}
	}

	for _, f := range found {
		r.Problems = append(r.Problems, f.p)
	}
	sortProblems(r.Problems)
	if !fix {
		return r, nil
	}
	if !force && done > 0 && missing*2 > done {
		return r, fmt.Errorf("%w (%d of %d songs missing; pass --force if they are really gone)", ErrUnsafeFix, missing, done)
	}
	for _, f := range found {
		if f.apply != nil {
			f.apply()
			r.Fixed = append(r.Fixed, f.p)
		}
	}
	sortProblems(r.Fixed)
	for _, c := range l.cols {
		if err := l.Save(c); err != nil {
			return r, err
		}
	}
	return r, nil
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
		if !strings.EqualFold(filepath.Ext(name), m4aExt) || isTempName(name) {
			return nil
		}
		var f audioFile
		f.tags, f.readErr = mp4tag.ReadFile(path)
		if f.readErr == nil {
			f.hash, f.readErr = mp4tag.HashFile(path)
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
