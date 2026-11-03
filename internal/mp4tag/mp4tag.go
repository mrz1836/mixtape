// Package mp4tag embeds iTunes-style metadata (the `ilst` atom set inside
// moov/udta/meta) into M4A data and reads it back, in pure Go, with no
// tagging libraries and no shell-outs. [Tag] and [Parse] work on bytes in
// memory, and writing the tagged bytes is up to the caller. [ReadFile] and
// [HashFile] read the tags and hash the audio of a file on disk, loading
// only moov or streaming the audio.
//
// Tagging parses the MP4 box structure, rebuilds moov with the tag atoms,
// drops padding (`free`) boxes, and fixes up the stco/co64 chunk offsets
// so the audio payload stays valid. Besides the classic text and number
// atoms it handles freeform `----` atoms (mean "com.apple.iTunes") and
// cover art (`covr`). The audio itself (the mdat payload) is never
// touched, so [AudioSHA256] identifies a recording no matter how often it
// is retagged.
package mp4tag

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// FreeformMean is the namespace of the freeform atoms this package reads
// and writes.
const FreeformMean = "com.apple.iTunes"

// Tags is the metadata to embed in an M4A file. Empty strings, zero
// numbers and an empty Cover are skipped (and remove an existing value).
type Tags struct {
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Grouping    string // used for the collection (playlist) name
	Comment     string // used for the Spotify track URL
	TrackNumber int
	TrackTotal  int
	DiscNumber  int
	DiscTotal   int
	// Date is the release date (`©day`): YYYY-MM-DD, or a bare year.
	Date string
	// Freeform holds `----` atoms under FreeformMean, keyed by name. On
	// write, a listed name replaces the existing atom (an empty value
	// removes it); freeform atoms not listed are preserved.
	Freeform map[string]string
	// Cover is JPEG or PNG artwork for the `covr` atom.
	Cover []byte
}

var (
	errNoMoov         = errors.New("mp4tag: no moov box")
	errNoMdat         = errors.New("mp4tag: no mdat box")
	errBadBox         = errors.New("mp4tag: malformed box")
	errTrailingBytes  = errors.New("mp4tag: trailing bytes after the last box")
	errOffsetOverflow = errors.New("mp4tag: chunk offset out of range")
	errTooLarge       = errors.New("mp4tag: box exceeds the 32-bit size limit")
	errOutOfRange     = errors.New("mp4tag: number does not fit its 16-bit field")

	// ErrCoverFormat reports cover art that is neither JPEG nor PNG.
	ErrCoverFormat = errors.New("mp4tag: cover art must be JPEG or PNG")
)

// maxMoovSize bounds the moov box ReadFile loads into memory. Real moov
// boxes are a few KiB plus the cover art.
const maxMoovSize = 64 << 20

// Tag returns data with tags embedded.
//
// It supports files with exactly one moov and one mdat, the layout
// afconvert writes, and makes no attempt to fix up others. Every chunk
// offset at or past the first mdat's payload moves as far as that payload
// does, which is right only while the audio sits in one mdat: when a
// second mdat follows moov and the first one precedes it, a change in
// moov's size moves the second mdat but none of the chunk offsets that
// point into it. A second top-level moov is replaced by the retagged
// first one.
func Tag(data []byte, tags Tags) ([]byte, error) {
	top, err := parseBoxes(data)
	if err != nil {
		return nil, err
	}
	moov, mdat := findBox(top, "moov"), findBox(top, "mdat")
	if moov == nil {
		return nil, errNoMoov
	}
	if mdat == nil {
		return nil, errNoMdat
	}

	newMoov, err := retagMoov(moov, tags)
	if err != nil {
		return nil, err
	}

	// The output is the top-level boxes in order, with moov retagged and
	// free padding dropped. mdat's payload moves by the difference between
	// what precedes it in the output and in data.
	parts := make([][]byte, 0, len(top))
	outPos := 0 // where the next part starts in the output
	var shift int64
	for i := range top {
		b := &top[i]
		if b == mdat {
			shift = int64(outPos - b.off)
		}
		part := b.data
		switch string(b.typ[:]) {
		case "free":
			continue
		case "moov":
			part = newMoov
		}
		parts = append(parts, part)
		outPos += len(part)
	}

	// Patch the chunk offsets inside the new moov, which parts shares, so
	// the output gets them too. moov precedes mdat in afconvert output, so
	// the shift is nonzero exactly when tags changed the file length.
	if err := fixChunkOffsets(newMoov, int64(mdat.off+mdat.hdr), shift); err != nil {
		return nil, err
	}
	return slices.Concat(parts...), nil
}

// Parse reads the iTunes-style tags back out of M4A data.
func Parse(data []byte) (Tags, error) {
	top, err := parseBoxes(data)
	if err != nil {
		return Tags{}, err
	}
	moov := findBox(top, "moov")
	if moov == nil {
		return Tags{}, errNoMoov
	}
	return parseMoov(*moov)
}

// ReadFile reads the tags of the M4A file at path. Only the moov box is
// loaded, so the audio is never read.
func ReadFile(path string) (Tags, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return Tags{}, err
	}
	defer func() { _ = f.Close() }()

	var tags Tags
	found := false
	err = scanFile(f, func(h header, off int64) (bool, error) {
		if h.typ != [4]byte{'m', 'o', 'o', 'v'} {
			return true, nil
		}
		if h.size > maxMoovSize {
			return false, fmt.Errorf("%w: moov of %d bytes", errTooLarge, h.size)
		}
		buf := make([]byte, h.size)
		if _, readErr := f.ReadAt(buf, off); readErr != nil {
			return false, readErr
		}
		var parseErr error
		tags, parseErr = parseMoov(box{typ: h.typ, data: buf, hdr: h.hdr})
		found = true
		return false, parseErr
	})
	if err != nil {
		return Tags{}, err
	}
	if !found {
		return Tags{}, errNoMoov
	}
	return tags, nil
}

// AudioSHA256 returns the lowercase hex SHA-256 of the top-level mdat
// payload: the encoded audio, which tagging never changes.
func AudioSHA256(data []byte) (string, error) {
	top, err := parseBoxes(data)
	if err != nil {
		return "", err
	}
	mdat := findBox(top, "mdat")
	if mdat == nil {
		return "", errNoMdat
	}
	sum := sha256.Sum256(mdat.payload())
	return hex.EncodeToString(sum[:]), nil
}

// HashFile returns [AudioSHA256] of the M4A file at path, streaming the
// audio rather than loading it.
func HashFile(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	found := false
	err = scanFile(f, func(hd header, off int64) (bool, error) {
		if hd.typ != [4]byte{'m', 'd', 'a', 't'} {
			return true, nil
		}
		found = true
		_, copyErr := io.Copy(h, io.NewSectionReader(f, off+int64(hd.hdr), hd.size-int64(hd.hdr)))
		return false, copyErr
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", errNoMdat
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
