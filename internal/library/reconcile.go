package library

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/mrz1836/mixtape/internal/fileutil"
	"github.com/mrz1836/mixtape/internal/mp4tag"
)

// taggingTempPrefix starts the temp files mp4tag.Write creates.
const taggingTempPrefix = ".tagging-"

// reconcile adopts files that exist for entries the manifest does not yet
// count as done: the crash window between a file's rename into place and
// the manifest write. A file is adopted only when its own SPOTIFY_ID (or
// SPOTIFY_REQUESTED_ID) tag names the entry. It changes c in memory and
// reports whether anything changed; Lock persists it.
func (l *Library) reconcile(c *Collection) bool {
	var open []int
	referenced := map[string]bool{}
	for i, e := range c.Manifest.Tracks {
		switch {
		case e.Status.Done():
			if e.File != "" {
				referenced[strings.ToLower(e.File)] = true
			}
		case e.Status != StatusRemoved:
			open = append(open, i)
		}
	}
	if len(open) == 0 {
		return false
	}

	changed := false
	adopt := func(i int, name string, tags mp4tag.Tags) {
		e := &c.Manifest.Tracks[i]
		hash := tags.Freeform[TagAudioSHA256]
		if hash == "" {
			var err error
			if hash, err = mp4tag.HashFile(filepath.Join(c.Dir, name)); err != nil {
				return
			}
		}
		if e.PendingWAV != "" {
			_ = os.Remove(e.PendingWAV)
		}
		e.Status, e.File, e.AudioSHA256 = StatusRecorded, name, hash
		e.Partial = tags.Freeform[TagPartial] == "1"
		e.FailReason, e.PendingWAV = "", ""
		referenced[strings.ToLower(name)] = true
		changed = true
		l.warn("%s: found %s on disk; marked it recorded", c.Folder(), name)
	}

	// First the name each entry has or would get.
	var rest []int
	for _, i := range open {
		e := &c.Manifest.Tracks[i]
		name := e.File
		if name == "" {
			name = trackFileName(c.Manifest.Kind, e.Position, e.Artist, e.Title)
		}
		if tags, err := mp4tag.ReadFile(filepath.Join(c.Dir, name)); err == nil && taggedFor(tags, e) {
			adopt(i, name, tags)
			continue
		}
		rest = append(rest, i)
	}
	if len(rest) == 0 {
		return changed
	}

	// Then any unreferenced file whose tags claim one of the rest.
	files, err := os.ReadDir(c.Dir)
	if err != nil {
		return changed
	}
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.EqualFold(filepath.Ext(name), m4aExt) || referenced[strings.ToLower(name)] || isTempName(name) {
			continue
		}
		tags, readErr := mp4tag.ReadFile(filepath.Join(c.Dir, name))
		if readErr != nil {
			continue
		}
		for k, i := range rest {
			if taggedFor(tags, &c.Manifest.Tracks[i]) {
				adopt(i, name, tags)
				rest = append(rest[:k], rest[k+1:]...)
				break
			}
		}
	}
	return changed
}

// taggedFor reports whether a file's identity tags name entry e.
func taggedFor(tags mp4tag.Tags, e *Entry) bool {
	id, req := tags.Freeform[TagSpotifyID], tags.Freeform[TagRequestedID]
	return (id != "" && (id == e.RequestedID || id == e.PlayedID)) || (req != "" && req == e.RequestedID)
}

// cleanTemps removes temp files a crashed write left in c's folder.
func (l *Library) cleanTemps(c *Collection) {
	files, err := os.ReadDir(c.Dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.IsDir() || !isTempName(f.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(c.Dir, f.Name())); err == nil {
			l.warn("%s: removed stale temp file %s", c.Folder(), f.Name())
		}
	}
}

func isTempName(name string) bool {
	return fileutil.IsTemp(name) || strings.HasPrefix(name, taggingTempPrefix)
}
