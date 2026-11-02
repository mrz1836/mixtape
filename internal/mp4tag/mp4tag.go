// Package mp4tag reads and writes iTunes-style metadata (the `ilst` atom set
// inside moov/udta/meta) in M4A files, in pure Go, with no tagging
// libraries and no shell-outs. It parses the MP4 box structure, rebuilds
// moov with the tag atoms, drops padding (`free`) boxes, and fixes up the
// stco/co64 chunk offsets so the audio payload stays valid.
//
// Besides the classic text and number atoms it handles freeform `----`
// atoms (mean "com.apple.iTunes") and cover art (`covr`). The audio itself
// (the mdat payload) is never touched, so [AudioSHA256] identifies a
// recording no matter how often it is retagged.
package mp4tag

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
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

type box struct {
	typ  [4]byte
	data []byte // whole box, header included
	off  int    // offset of the box start within the slice it came from
	hdr  int    // header length: 8, or 16 for a 64-bit largesize box
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

// `data` atom type codes.
const (
	dataUTF8 = 1
	dataJPEG = 13
	dataPNG  = 14
)

// maxMoovSize bounds the moov box ReadFile loads into memory. Real moov
// boxes are a few KiB plus the cover art.
const maxMoovSize = 64 << 20

// Write embeds tags into the M4A file at path, atomically (a temp file in
// the same directory, then rename), keeping the file's permissions. The
// audio payload is never re-encoded.
func Write(path string, tags Tags) error {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := Tag(data, tags)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tagging-*.m4a")
	if err != nil {
		return err
	}
	if err := writeAndClose(tmp, out, info.Mode().Perm()); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// writeAndClose writes data to f, sets its permissions, and closes it,
// reporting the first error.
func writeAndClose(f *os.File, data []byte, perm os.FileMode) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Tag returns data with tags embedded.
func Tag(data []byte, tags Tags) ([]byte, error) {
	top, err := parseBoxes(data, 0)
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
	oldMdatData := int64(mdat.off) + int64(mdat.hdr)

	newMoov, err := retagMoov(moov, tags)
	if err != nil {
		return nil, err
	}

	// New layout: top-level boxes in order with moov replaced and `free`
	// padding dropped. Compute where mdat's payload will start.
	var pos int
	for i := range top {
		if &top[i] == mdat {
			break
		}
		switch string(top[i].typ[:]) {
		case "free":
			continue
		case "moov":
			pos += len(newMoov)
		default:
			pos += len(top[i].data)
		}
	}
	shift := int64(pos) + int64(mdat.hdr) - oldMdatData

	// Patch chunk offsets inside the new moov (moov precedes mdat in
	// afconvert output, so the shift is nonzero exactly when tags changed
	// the file length).
	if err := fixChunkOffsets(newMoov, oldMdatData, shift); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	for i := range top {
		switch string(top[i].typ[:]) {
		case "free":
			continue
		case "moov":
			out.Write(newMoov)
		default:
			out.Write(top[i].data)
		}
	}
	return out.Bytes(), nil
}

// Parse reads the iTunes-style tags back out of M4A data.
func Parse(data []byte) (Tags, error) {
	top, err := parseBoxes(data, 0)
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
	top, err := parseBoxes(data, 0)
	if err != nil {
		return "", err
	}
	mdat := findBox(top, "mdat")
	if mdat == nil {
		return "", errNoMdat
	}
	sum := sha256.Sum256(mdat.data[mdat.hdr:])
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

// ---------- moov rebuilding ----------

func retagMoov(moov *box, tags Tags) ([]byte, error) {
	var w builder
	moovPayload := moov.payload()
	udta, err := child(moovPayload, "udta")
	if err != nil {
		return nil, err
	}
	var udtaPayload []byte // stays empty when moov has no udta yet
	if udta != nil {
		udtaPayload = udta.payload()
	}

	meta, err := w.meta(udtaPayload, tags)
	if err != nil {
		return nil, err
	}
	udtaKeep, err := childrenExcept(udtaPayload, "meta")
	if err != nil {
		return nil, err
	}
	newUdta := w.box("udta", append(concat(udtaKeep), meta...))

	moovKeep, err := childrenExcept(moovPayload, "udta")
	if err != nil {
		return nil, err
	}
	out := w.box("moov", append(concat(moovKeep), newUdta...))
	if w.err != nil {
		return nil, w.err
	}
	return out, nil
}

// meta rebuilds udta's meta box. An existing one keeps its other children
// (hdlr and others) and gets a fresh ilst; without one, a default meta with
// an mdir handler is created.
func (w *builder) meta(udtaPayload []byte, tags Tags) ([]byte, error) {
	m, err := child(udtaPayload, "meta")
	if err != nil {
		return nil, err
	}
	if m == nil {
		return w.defaultMeta(tags)
	}
	mp, err := metaPayload(*m)
	if err != nil {
		return nil, err
	}
	oldIlst, err := child(mp, "ilst")
	if err != nil {
		return nil, err
	}
	keep, err := childrenExcept(mp, "ilst")
	if err != nil {
		return nil, err
	}
	ilst, err := w.ilst(oldIlst, tags)
	if err != nil {
		return nil, err
	}
	return w.fullBox("meta", append(concat(keep), ilst...)), nil
}

func (w *builder) defaultMeta(tags Tags) ([]byte, error) {
	hdlr := w.box("hdlr", make([]byte, 25))
	if hdlr != nil {
		copy(hdlr[8+8:], "mdir") // handler type after version/flags + predefined
	}
	ilst, err := w.ilst(nil, tags)
	if err != nil {
		return nil, err
	}
	return w.fullBox("meta", append(hdlr, ilst...)), nil
}

// ilst builds the tag list. Atoms of oldIlst that this package does not
// manage are kept, including freeform atoms whose names tags.Freeform
// does not list.
func (w *builder) ilst(oldIlst *box, tags Tags) ([]byte, error) {
	var entries [][]byte
	if oldIlst != nil {
		err := walkChildren(oldIlst.payload(), func(b box) error {
			if isTagAtom(b.typ) {
				return nil // managed: rebuilt below
			}
			if string(b.typ[:]) == "----" {
				if ff, ok := parseFreeform(b); ok && ff.mean == FreeformMean {
					if _, managed := tags.Freeform[ff.name]; managed {
						return nil
					}
				}
			}
			entries = append(entries, b.data)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	entries = append(entries,
		w.textAtom("\xa9nam", tags.Title),
		w.textAtom("\xa9ART", tags.Artist),
		w.textAtom("aART", tags.AlbumArtist),
		w.textAtom("\xa9alb", tags.Album),
		w.textAtom("\xa9grp", tags.Grouping),
		w.textAtom("\xa9cmt", tags.Comment),
		w.textAtom("\xa9day", tags.Date),
		w.pairAtom("trkn", tags.TrackNumber, tags.TrackTotal),
		w.pairAtom("disk", tags.DiscNumber, tags.DiscTotal),
		w.coverAtom(tags.Cover),
	)
	names := make([]string, 0, len(tags.Freeform))
	for name := range tags.Freeform {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entries = append(entries, w.freeformAtom(name, tags.Freeform[name]))
	}
	return w.box("ilst", concat(entries)), nil
}

func isTagAtom(typ [4]byte) bool {
	switch string(typ[:]) {
	case "\xa9nam", "\xa9ART", "aART", "\xa9alb", "\xa9grp", "\xa9cmt", "\xa9day", "trkn", "disk", "covr":
		return true
	}
	return false
}

// parseMoov collects the tags found anywhere under moov.
func parseMoov(moov box) (Tags, error) {
	var tags Tags
	var walk func(b box) error
	walk = func(b box) error {
		switch string(b.typ[:]) {
		case "moov", "udta", "ilst", "trak", "mdia", "minf", "stbl":
			return walkChildren(b.payload(), walk)
		case "meta":
			payload, err := metaPayload(b)
			if err != nil {
				return err
			}
			return walkChildren(payload, walk)
		case "stco", "co64":
			return nil
		case "----":
			if ff, ok := parseFreeform(b); ok && ff.mean == FreeformMean && ff.hasValue {
				if tags.Freeform == nil {
					tags.Freeform = map[string]string{}
				}
				tags.Freeform[ff.name] = ff.value
			}
			return nil
		default:
			if isTagAtom(b.typ) {
				return readTagAtom(&tags, b)
			}
			return nil
		}
	}
	if err := walkChildren(moov.payload(), walk); err != nil {
		return Tags{}, err
	}
	return tags, nil
}

func readTagAtom(tags *Tags, b box) error {
	d, err := child(b.payload(), "data")
	if err != nil {
		return err
	}
	if d == nil {
		return nil
	}
	payload := d.payload()
	if len(payload) < 8 {
		return nil
	}
	flags := binary.BigEndian.Uint32(payload[0:4])
	val := payload[8:]
	switch {
	case string(b.typ[:]) == "trkn" && len(val) >= 8:
		tags.TrackNumber = int(binary.BigEndian.Uint16(val[2:4]))
		tags.TrackTotal = int(binary.BigEndian.Uint16(val[4:6]))
	case string(b.typ[:]) == "disk" && len(val) >= 8:
		tags.DiscNumber = int(binary.BigEndian.Uint16(val[2:4]))
		tags.DiscTotal = int(binary.BigEndian.Uint16(val[4:6]))
	case string(b.typ[:]) == "covr" && (flags == dataJPEG || flags == dataPNG):
		if tags.Cover == nil {
			tags.Cover = bytes.Clone(val)
		}
	case flags == dataUTF8:
		switch string(b.typ[:]) {
		case "\xa9nam":
			tags.Title = string(val)
		case "\xa9ART":
			tags.Artist = string(val)
		case "aART":
			tags.AlbumArtist = string(val)
		case "\xa9alb":
			tags.Album = string(val)
		case "\xa9grp":
			tags.Grouping = string(val)
		case "\xa9cmt":
			tags.Comment = string(val)
		case "\xa9day":
			tags.Date = string(val)
		}
	}
	return nil
}

// freeform is a decoded `----` atom.
type freeform struct {
	mean, name, value string
	hasValue          bool
}

// parseFreeform decodes a `----` atom's mean, name and UTF-8 data children.
// ok is false for a malformed atom, which callers keep untouched.
func parseFreeform(b box) (freeform, bool) {
	var ff freeform
	var haveMean, haveName bool
	err := walkChildren(b.payload(), func(c box) error {
		p := c.payload()
		switch string(c.typ[:]) {
		case "mean":
			if len(p) >= 4 {
				ff.mean, haveMean = string(p[4:]), true
			}
		case "name":
			if len(p) >= 4 {
				ff.name, haveName = string(p[4:]), true
			}
		case "data":
			if len(p) >= 8 && binary.BigEndian.Uint32(p[0:4]) == dataUTF8 && !ff.hasValue {
				ff.value, ff.hasValue = string(p[8:]), true
			}
		}
		return nil
	})
	return ff, err == nil && haveMean && haveName
}

// ---------- chunk offset fixups ----------

// fixChunkOffsets rewrites stco/co64 entries inside moov so they follow the
// moved mdat. Only the moov/trak/mdia/minf/stbl chain can contain them.
func fixChunkOffsets(moov []byte, oldMdatData, shift int64) error {
	if shift == 0 {
		return nil
	}
	var walk func(b box) error
	walk = func(b box) error {
		switch string(b.typ[:]) {
		case "trak", "mdia", "minf", "stbl":
			return walkChildren(b.payload(), walk)
		case "stco":
			return shiftOffsets32(b.payload(), oldMdatData, shift)
		case "co64":
			return shiftOffsets64(b.payload(), oldMdatData, shift)
		}
		return nil
	}
	return walkChildren(moov[8:], walk)
}

// entryCount reads a stco/co64 entry count and checks the table fits p.
func entryCount(p []byte, entrySize int) (int, error) {
	if len(p) < 8 {
		return 0, fmt.Errorf("%w: chunk offset table of %d bytes", errBadBox, len(p))
	}
	n := int(binary.BigEndian.Uint32(p[4:8]))
	if n > (len(p)-8)/entrySize {
		return 0, fmt.Errorf("%w: %d chunk offsets in a %d-byte table", errBadBox, n, len(p))
	}
	return n, nil
}

// shiftOffsets32 moves the 32-bit offsets of a stco table by shift.
func shiftOffsets32(p []byte, oldMdatData, shift int64) error {
	n, err := entryCount(p, 4)
	if err != nil {
		return err
	}
	for i := range n {
		o := 8 + 4*i
		v := int64(binary.BigEndian.Uint32(p[o : o+4]))
		if v < oldMdatData {
			continue
		}
		moved := v + shift
		if moved < 0 || moved > math.MaxUint32 {
			return fmt.Errorf("%w: stco entry %d moves to %d", errOffsetOverflow, i, moved)
		}
		binary.BigEndian.PutUint32(p[o:o+4], uint32(moved))
	}
	return nil
}

// shiftOffsets64 moves the 64-bit offsets of a co64 table by shift.
func shiftOffsets64(p []byte, oldMdatData, shift int64) error {
	n, err := entryCount(p, 8)
	if err != nil {
		return err
	}
	for i := range n {
		o := 8 + 8*i
		raw := binary.BigEndian.Uint64(p[o : o+8])
		if raw > math.MaxInt64 {
			return fmt.Errorf("%w: co64 entry %d is %d", errOffsetOverflow, i, raw)
		}
		v := int64(raw)
		if v < oldMdatData {
			continue
		}
		moved := v + shift
		if moved < 0 {
			return fmt.Errorf("%w: co64 entry %d moves to %d", errOffsetOverflow, i, moved)
		}
		binary.BigEndian.PutUint64(p[o:o+8], uint64(moved))
	}
	return nil
}

// ---------- box building ----------

// builder assembles MP4 boxes. The first size or range violation sticks in
// err and turns every later call into a no-op, so a whole tree is built
// first and checked once.
type builder struct {
	err error
}

func (w *builder) fail(err error) []byte {
	if w.err == nil {
		w.err = err
	}
	return nil
}

func (w *builder) box(typ string, payload []byte) []byte {
	if w.err != nil {
		return nil
	}
	n := 8 + len(payload)
	if n < 8 || n > math.MaxUint32 {
		return w.fail(fmt.Errorf("%w: %q box of %d bytes", errTooLarge, typ, n))
	}
	out := make([]byte, n)
	binary.BigEndian.PutUint32(out[0:4], uint32(n))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func (w *builder) fullBox(typ string, payload []byte) []byte {
	return w.box(typ, append([]byte{0, 0, 0, 0}, payload...)) // version/flags = 0
}

// dataBox builds a `data` atom: 4 bytes type, 4 bytes locale (0), value.
func (w *builder) dataBox(flags uint32, val []byte) []byte {
	payload := make([]byte, 8+len(val))
	binary.BigEndian.PutUint32(payload[0:4], flags)
	copy(payload[8:], val)
	return w.box("data", payload)
}

func (w *builder) textAtom(typ, val string) []byte {
	if val == "" {
		return nil
	}
	return w.box(typ, w.dataBox(dataUTF8, []byte(val)))
}

// pairAtom builds trkn/disk: 8 bytes — reserved, number, total, reserved.
func (w *builder) pairAtom(typ string, num, total int) []byte {
	if num == 0 && total == 0 {
		return nil
	}
	if num < 0 || num > math.MaxUint16 || total < 0 || total > math.MaxUint16 {
		return w.fail(fmt.Errorf("%w: %s %d/%d", errOutOfRange, typ, num, total))
	}
	v := make([]byte, 8)
	binary.BigEndian.PutUint16(v[2:4], uint16(num))
	binary.BigEndian.PutUint16(v[4:6], uint16(total))
	return w.box(typ, w.dataBox(0, v))
}

// coverAtom builds `covr` with the image type detected from magic bytes.
func (w *builder) coverAtom(img []byte) []byte {
	if len(img) == 0 {
		return nil
	}
	var typ uint32
	switch {
	case bytes.HasPrefix(img, []byte{0xFF, 0xD8, 0xFF}):
		typ = dataJPEG
	case bytes.HasPrefix(img, []byte("\x89PNG\r\n\x1a\n")):
		typ = dataPNG
	default:
		return w.fail(ErrCoverFormat)
	}
	return w.box("covr", w.dataBox(typ, img))
}

// freeformAtom builds a `----` atom; an empty value builds nothing, which
// removes the atom.
func (w *builder) freeformAtom(name, val string) []byte {
	if val == "" {
		return nil
	}
	return w.box("----", concat([][]byte{
		w.fullBox("mean", []byte(FreeformMean)),
		w.fullBox("name", []byte(name)),
		w.dataBox(dataUTF8, []byte(val)),
	}))
}

// ---------- box parsing ----------

// header is a decoded box header.
type header struct {
	typ  [4]byte
	size int64 // whole box, header included
	hdr  int   // header length
}

// readHeader decodes the box header at the start of b, where remaining is
// the number of bytes from the box start to the end of its container (for
// size 0, "extends to the end").
func readHeader(b []byte, remaining int64) (header, error) {
	if len(b) < 8 {
		return header{}, fmt.Errorf("%w: truncated header", errBadBox)
	}
	h := header{size: int64(binary.BigEndian.Uint32(b[0:4])), hdr: 8}
	copy(h.typ[:], b[4:8])
	switch h.size {
	case 0:
		h.size = remaining
	case 1:
		if len(b) < 16 {
			return header{}, fmt.Errorf("%w: truncated largesize header", errBadBox)
		}
		large := binary.BigEndian.Uint64(b[8:16])
		if large > math.MaxInt64 {
			return header{}, fmt.Errorf("%w: largesize %d", errBadBox, large)
		}
		h.size, h.hdr = int64(large), 16
	}
	if h.size < int64(h.hdr) {
		return header{}, fmt.Errorf("%w: size %d", errBadBox, h.size)
	}
	if h.size > remaining {
		return header{}, fmt.Errorf("%w: size %d overruns its container", errBadBox, h.size)
	}
	return h, nil
}

// parseBoxes parses consecutive top-level boxes in data, tracking their
// offsets. 64-bit largesize and to-end-of-file boxes are supported.
func parseBoxes(data []byte, base int) ([]box, error) {
	var boxes []box
	pos := 0
	for pos+8 <= len(data) {
		h, err := readHeader(data[pos:], int64(len(data)-pos))
		if err != nil {
			return nil, fmt.Errorf("%w at offset %d", err, base+pos)
		}
		size := int(h.size)
		boxes = append(boxes, box{typ: h.typ, data: data[pos : pos+size], off: base + pos, hdr: h.hdr})
		pos += size
	}
	if pos != len(data) && len(boxes) > 0 {
		return nil, fmt.Errorf("%w: %d bytes at offset %d", errTrailingBytes, len(data)-pos, base+pos)
	}
	return boxes, nil
}

// scanFile walks the top-level boxes of f, calling fn with each header and
// its offset until fn returns false or an error.
func scanFile(f *os.File, fn func(h header, off int64) (bool, error)) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	var buf [16]byte
	for off := int64(0); off+8 <= size; {
		n, readErr := f.ReadAt(buf[:], off)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		h, hdrErr := readHeader(buf[:n], size-off)
		if hdrErr != nil {
			return fmt.Errorf("%w at offset %d", hdrErr, off)
		}
		more, fnErr := fn(h, off)
		if fnErr != nil || !more {
			return fnErr
		}
		off += h.size
	}
	return nil
}

func findBox(boxes []box, typ string) *box {
	for i := range boxes {
		if string(boxes[i].typ[:]) == typ {
			return &boxes[i]
		}
	}
	return nil
}

func (b box) payload() []byte {
	if b.hdr == 0 {
		return b.data[8:]
	}
	return b.data[b.hdr:]
}

// metaPayload returns the children of a meta box, past its FullBox
// version/flags.
func metaPayload(m box) ([]byte, error) {
	p := m.payload()
	if len(p) < 4 {
		return nil, fmt.Errorf("%w: meta box of %d bytes", errBadBox, len(m.data))
	}
	return p[4:], nil
}

func walkChildren(payload []byte, fn func(box) error) error {
	pos := 0
	for pos+8 <= len(payload) {
		size := int(binary.BigEndian.Uint32(payload[pos : pos+4]))
		if size < 8 || pos+size > len(payload) {
			return fmt.Errorf("%w: child of size %d at %d", errBadBox, size, pos)
		}
		var typ [4]byte
		copy(typ[:], payload[pos+4:pos+8])
		if err := fn(box{typ: typ, data: payload[pos : pos+size], off: pos, hdr: 8}); err != nil {
			return err
		}
		pos += size
	}
	return nil
}

// child returns the first direct child of type want, or nil.
func child(payload []byte, want string) (*box, error) {
	var found *box
	err := walkChildren(payload, func(b box) error {
		if string(b.typ[:]) == want && found == nil {
			found = &b
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// childrenExcept returns the raw bytes of every direct child not of type skip.
func childrenExcept(payload []byte, skip string) ([][]byte, error) {
	var keep [][]byte
	err := walkChildren(payload, func(b box) error {
		if string(b.typ[:]) != skip {
			keep = append(keep, b.data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keep, nil
}

func concat(parts [][]byte) []byte {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
