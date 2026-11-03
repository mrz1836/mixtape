package mp4tag

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"maps"
	"math"
	"slices"
)

// `data` atom type codes.
const (
	dataUTF8 = 1
	dataJPEG = 13
	dataPNG  = 14
)

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
	newUdta := w.box("udta", append(slices.Concat(udtaKeep...), meta...))

	moovKeep, err := childrenExcept(moovPayload, "udta")
	if err != nil {
		return nil, err
	}
	out := w.box("moov", append(slices.Concat(moovKeep...), newUdta...))
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
	return w.fullBox("meta", append(slices.Concat(keep...), ilst...)), nil
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
		for b, err := range childrenOf(oldIlst.payload()) {
			if err != nil {
				return nil, err
			}
			if _, managed := findTagAtom(b.typ); managed {
				continue // rebuilt below
			}
			if string(b.typ[:]) == "----" {
				if ff, ok := parseFreeform(b); ok && ff.mean == FreeformMean {
					if _, managed := tags.Freeform[ff.name]; managed {
						continue
					}
				}
			}
			entries = append(entries, b.raw())
		}
	}
	for _, a := range tagAtoms() {
		entries = append(entries, w.tagAtom(a, &tags))
	}
	for _, name := range slices.Sorted(maps.Keys(tags.Freeform)) {
		entries = append(entries, w.freeformAtom(name, tags.Freeform[name]))
	}
	return w.box("ilst", slices.Concat(entries...)), nil
}

// atomKind is how a managed atom stores its value.
type atomKind int

const (
	atomText  atomKind = iota // UTF-8 text
	atomPair                  // number and total (trkn, disk)
	atomCover                 // JPEG or PNG image
)

// tagAtom is one atom Tags manages: its type and where its value lives.
type tagAtom struct {
	typ  string
	kind atomKind
	text func(*Tags) *string
	pair func(*Tags) (num, total *int)
}

// tagAtoms lists the atoms Tags manages, in the order they are written.
// Building, recognizing and reading tags all go through it, so a retag can
// never leave a managed atom behind or write it twice.
func tagAtoms() []tagAtom {
	text := func(typ string, f func(*Tags) *string) tagAtom { return tagAtom{typ: typ, kind: atomText, text: f} }
	pair := func(typ string, f func(*Tags) (*int, *int)) tagAtom {
		return tagAtom{typ: typ, kind: atomPair, pair: f}
	}
	return []tagAtom{
		text("\xa9nam", func(t *Tags) *string { return &t.Title }),
		text("\xa9ART", func(t *Tags) *string { return &t.Artist }),
		text("aART", func(t *Tags) *string { return &t.AlbumArtist }),
		text("\xa9alb", func(t *Tags) *string { return &t.Album }),
		text("\xa9grp", func(t *Tags) *string { return &t.Grouping }),
		text("\xa9cmt", func(t *Tags) *string { return &t.Comment }),
		text("\xa9day", func(t *Tags) *string { return &t.Date }),
		pair("trkn", func(t *Tags) (*int, *int) { return &t.TrackNumber, &t.TrackTotal }),
		pair("disk", func(t *Tags) (*int, *int) { return &t.DiscNumber, &t.DiscTotal }),
		{typ: "covr", kind: atomCover},
	}
}

// findTagAtom returns the managed atom of type typ.
func findTagAtom(typ [4]byte) (tagAtom, bool) {
	for _, a := range tagAtoms() {
		if a.typ == string(typ[:]) {
			return a, true
		}
	}
	return tagAtom{}, false
}

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

// tagAtom builds atom a from tags (nothing for an empty value).
func (w *builder) tagAtom(a tagAtom, tags *Tags) []byte {
	switch a.kind {
	case atomPair:
		num, total := a.pair(tags)
		return w.pairAtom(a.typ, *num, *total)
	case atomCover:
		return w.coverAtom(tags.Cover)
	case atomText:
	}
	return w.textAtom(a.typ, *a.text(tags))
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
	return w.box("----", slices.Concat(
		w.fullBox("mean", []byte(FreeformMean)),
		w.fullBox("name", []byte(name)),
		w.dataBox(dataUTF8, []byte(val)),
	))
}
