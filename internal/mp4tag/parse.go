package mp4tag

import (
	"bytes"
	"encoding/binary"
)

// parseMoov collects the tags found anywhere under moov.
func parseMoov(moov box) (Tags, error) {
	var tags Tags
	if err := collectTags(&tags, moov.payload()); err != nil {
		return Tags{}, err
	}
	return tags, nil
}

// collectTags reads the tags among the boxes of payload into tags.
func collectTags(tags *Tags, payload []byte) error {
	for b, err := range childrenOf(payload) {
		if err != nil {
			return err
		}
		if err = collectTag(tags, b); err != nil {
			return err
		}
	}
	return nil
}

// collectTag reads b into tags when it is a tag atom, and the tags inside
// it when it is a container that can hold some.
func collectTag(tags *Tags, b box) error {
	switch string(b.typ[:]) {
	case "moov", "udta", "ilst", "trak", "mdia", "minf", "stbl":
		return collectTags(tags, b.payload())
	case "meta":
		payload, err := metaPayload(b)
		if err != nil {
			return err
		}
		return collectTags(tags, payload)
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
		if a, ok := findTagAtom(b.typ); ok {
			return readTagAtom(tags, a, b)
		}
		return nil
	}
}

// readTagAtom reads managed atom a from box b into tags. Values of an
// unexpected type are ignored.
func readTagAtom(tags *Tags, a tagAtom, b box) error {
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
	switch a.kind {
	case atomPair:
		// trkn carries 8 bytes; iTunes writes disk with only 6.
		if len(val) >= 6 {
			num, total := a.pair(tags)
			*num = int(binary.BigEndian.Uint16(val[2:4]))
			*total = int(binary.BigEndian.Uint16(val[4:6]))
		}
	case atomCover:
		if (flags == dataJPEG || flags == dataPNG) && tags.Cover == nil {
			tags.Cover = bytes.Clone(val)
		}
	case atomText:
		if flags == dataUTF8 {
			*a.text(tags) = string(val)
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
	for c, err := range childrenOf(b.payload()) {
		if err != nil {
			return freeform{}, false
		}
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
	}
	return ff, haveMean && haveName
}
