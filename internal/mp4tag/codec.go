package mp4tag

import (
	"errors"
	"fmt"
	"strings"
)

// Codec is the audio format of an M4A file: the type of the sample entry
// that describes its sound track.
type Codec string

// Codecs Mixtape writes. Any other sample entry is reported as its own
// four-character code, trimmed (a file another tool made).
const (
	CodecALAC Codec = "alac" // Apple Lossless: the `alac` sample entry
	CodecAAC  Codec = "aac"  // AAC: the `mp4a` sample entry
)

var (
	// ErrUnknownCodec reports a codec name other than alac or aac.
	ErrUnknownCodec = errors.New("unknown codec")
	// errNoSoundTrack reports a moov without a sound track that describes
	// its samples.
	errNoSoundTrack = errors.New("mp4tag: no sound track with a sample description")
)

// ParseCodec parses a codec name, case-insensitively: alac or aac.
func ParseCodec(s string) (Codec, error) {
	switch c := Codec(strings.ToLower(strings.TrimSpace(s))); c {
	case CodecALAC, CodecAAC:
		return c, nil
	}
	return "", fmt.Errorf("%w %q (want alac or aac)", ErrUnknownCodec, s)
}

// Lossless reports whether the codec keeps every sample as it was.
func (c Codec) Lossless() bool { return c == CodecALAC }

// codecOfEntry maps a sample entry type to its codec.
func codecOfEntry(typ [4]byte) Codec {
	switch string(typ[:]) {
	case "alac":
		return CodecALAC
	case "mp4a":
		return CodecAAC
	}
	if name := strings.TrimSpace(string(typ[:])); name != "" {
		return Codec(name)
	}
	return Codec(typ[:]) // four spaces: still a name
}

// CodecOf returns the codec of the M4A data: the type of the first sample
// entry in the sample description (stsd) of the first sound track (a trak
// whose handler is `soun`).
func CodecOf(data []byte) (Codec, error) {
	top, err := parseBoxes(data)
	if err != nil {
		return "", err
	}
	moov := findBox(top, "moov")
	if moov == nil {
		return "", errNoMoov
	}
	return moovCodec(*moov)
}

// ReadCodec returns [CodecOf] the M4A file at path, loading only its moov.
func ReadCodec(path string) (Codec, error) {
	moov, err := loadMoov(path)
	if err != nil {
		return "", err
	}
	return moovCodec(moov)
}

// moovCodec finds the codec among moov's tracks.
func moovCodec(moov box) (Codec, error) {
	for trak, err := range childrenOf(moov.payload()) {
		if err != nil {
			return "", err
		}
		if string(trak.typ[:]) != "trak" {
			continue
		}
		c, ok, err := trakCodec(trak)
		if err != nil {
			return "", err
		}
		if ok {
			return c, nil
		}
	}
	return "", errNoSoundTrack
}

// trakCodec returns the codec of a sound track; ok is false for any other
// track, or a sound track without a sample entry.
func trakCodec(trak box) (Codec, bool, error) {
	mdia, err := child(trak.payload(), "mdia")
	if err != nil || mdia == nil {
		return "", false, err
	}
	hdlr, err := child(mdia.payload(), "hdlr")
	if err != nil || hdlr == nil {
		return "", false, err
	}
	// hdlr: version and flags, pre_defined, then the handler type.
	if p := hdlr.payload(); len(p) < 12 || string(p[8:12]) != "soun" {
		return "", false, nil
	}
	stsd, err := descend(mdia.payload(), "minf", "stbl", "stsd")
	if err != nil || stsd == nil {
		return "", false, err
	}
	// stsd: version and flags, entry count, then the sample entries.
	p := stsd.payload()
	if len(p) < 8 {
		return "", false, fmt.Errorf("%w: stsd of %d bytes", errBadBox, len(stsd.data))
	}
	for entry, entryErr := range childrenOf(p[8:]) {
		if entryErr != nil {
			return "", false, entryErr
		}
		return codecOfEntry(entry.typ), true, nil
	}
	return "", false, nil
}

// descend follows a path of child types down from payload and returns the
// box at its end, or nil when one is missing.
func descend(payload []byte, path ...string) (*box, error) {
	var b *box
	for _, typ := range path {
		next, err := child(payload, typ)
		if err != nil || next == nil {
			return nil, err
		}
		b, payload = next, next.payload()
	}
	return b, nil
}
