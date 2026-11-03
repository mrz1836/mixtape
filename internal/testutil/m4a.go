package testutil

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// iTunes `data` atom type for UTF-8 text.
const dataUTF8 = 1

// ErrBadFile reports a file the test reader cannot walk.
var ErrBadFile = errors.New("testutil: malformed MP4")

// Box builds an MP4 box with a 32-bit size header around the
// concatenated payload.
func Box(typ string, payload ...[]byte) []byte {
	p := bytes.Join(payload, nil)
	b := binary.BigEndian.AppendUint32(make([]byte, 0, 8+len(p)), uint32(8+len(p))) //nolint:gosec // G115: test files are tiny
	return append(append(b, typ...), p...)
}

// Box64 builds an MP4 box with a 64-bit largesize header (size field 1).
func Box64(typ string, payload ...[]byte) []byte {
	p := bytes.Join(payload, nil)
	b := binary.BigEndian.AppendUint32(make([]byte, 0, 16+len(p)), 1)
	b = append(b, typ...)
	b = binary.BigEndian.AppendUint64(b, uint64(16+len(p)))
	return append(b, p...)
}

// Box0 builds an MP4 box whose size field is 0: it extends to the end of
// its container (or of the file).
func Box0(typ string, payload ...[]byte) []byte {
	p := bytes.Join(payload, nil)
	return append(append(make([]byte, 4, 8+len(p)), typ...), p...)
}

// FullBox builds a box whose payload starts with version 0 and no flags.
func FullBox(typ string, payload ...[]byte) []byte {
	return Box(typ, append([][]byte{{0, 0, 0, 0}}, payload...)...)
}

// DataBox builds an iTunes `data` atom of the given type code.
func DataBox(code uint32, value []byte) []byte {
	return Box("data", binary.BigEndian.AppendUint32(nil, code), []byte{0, 0, 0, 0}, value)
}

// TextItem builds an ilst text atom, such as TextItem("\xa9nam", "Title").
func TextItem(typ, value string) []byte {
	return Box(typ, DataBox(dataUTF8, []byte(value)))
}

// FreeformItem builds a `----` ilst atom (mean, name, UTF-8 data).
func FreeformItem(mean, name, value string) []byte {
	return Box("----", FullBox("mean", []byte(mean)), FullBox("name", []byte(name)), DataBox(dataUTF8, []byte(value)))
}

// MdatHeader is how [M4A] writes the mdat box header.
type MdatHeader int

// mdat header forms.
const (
	Mdat32    MdatHeader = iota // a 32-bit size
	MdatLarge                   // a 64-bit largesize
	MdatToEnd                   // size 0: to the end of the file
)

// M4AOptions shape a synthetic M4A file.
type M4AOptions struct {
	// Audio is the mdat payload; empty means 128 bytes of a fixed pattern.
	Audio []byte
	// Chunks is the number of chunk offsets in the sample table, spread
	// evenly over Audio; 0 means 4.
	Chunks int
	// CO64 writes a 64-bit co64 chunk-offset table instead of stco.
	CO64 bool
	// Mdat is the form of the mdat header.
	Mdat MdatHeader
	// MoovLast puts moov after mdat (as streaming encoders do). It is
	// ignored with MdatToEnd, whose mdat must be the last box.
	MoovLast bool
	// Free inserts a free box of this many payload bytes before mdat.
	Free int
	// NoUdta leaves udta (and so any tags) out of moov.
	NoUdta bool
	// Items are existing ilst atoms (see [TextItem], [FreeformItem]).
	Items [][]byte
	// Codec names the sample entry of the sound track: "aac" writes an
	// `mp4a` entry, "alac" an `alac` one, any other four characters an
	// entry of that type, and "" none at all (an empty sample table).
	Codec string
}

// M4A returns a minimal, structurally valid M4A file: ftyp; moov with
// mvhd, one audio trak down to a chunk-offset table that points into
// mdat, and udta/meta/ilst; an optional free box; and mdat. It holds no
// real AAC, but everything mp4tag reads and rewrites is there.
func M4A(o M4AOptions) []byte {
	audio := o.Audio
	if len(audio) == 0 {
		audio = bytes.Repeat([]byte("synthetic audio!"), 8)
	}
	chunks := o.Chunks
	if chunks <= 0 {
		chunks = 4
	}
	ftyp := Box("ftyp", []byte("M4A "), []byte{0, 0, 2, 0}, []byte("M4A mp42isom"))
	var mdat []byte
	switch o.Mdat {
	case MdatLarge:
		mdat = Box64("mdat", audio)
	case MdatToEnd:
		mdat = Box0("mdat", audio)
	case Mdat32:
		mdat = Box("mdat", audio)
	}
	var free []byte
	if o.Free > 0 {
		free = Box("free", make([]byte, o.Free))
	}
	hdr := len(mdat) - len(audio)
	moovLast := o.MoovLast && o.Mdat != MdatToEnd

	// moov's size does not depend on the offset values, so lay the file out
	// once with zero offsets, then build moov with the real ones.
	moov := moovBox(o, make([]int64, chunks))
	start := len(ftyp) + len(moov) + len(free) + hdr
	if moovLast {
		start = len(ftyp) + len(free) + hdr
	}
	offsets := make([]int64, chunks)
	for i := range offsets {
		offsets[i] = int64(start + i*len(audio)/chunks)
	}
	moov = moovBox(o, offsets)
	if moovLast {
		return bytes.Join([][]byte{ftyp, free, mdat, moov}, nil)
	}
	return bytes.Join([][]byte{ftyp, moov, free, mdat}, nil)
}

// moovBox builds moov with the given chunk offsets.
func moovBox(o M4AOptions, offsets []int64) []byte {
	table := binary.BigEndian.AppendUint32(nil, uint32(len(offsets))) //nolint:gosec // G115: a handful of entries
	typ := "stco"
	for _, off := range offsets {
		if o.CO64 {
			table = binary.BigEndian.AppendUint64(table, uint64(off)) //nolint:gosec // G115: offsets are positive
		} else {
			table = binary.BigEndian.AppendUint32(table, uint32(off)) //nolint:gosec // G115: test files are tiny
		}
	}
	if o.CO64 {
		typ = "co64"
	}
	stbl := Box("stbl", sampleDescription(o.Codec), FullBox(typ, table))
	trak := Box("trak",
		FullBox("tkhd", make([]byte, 80)),
		Box("mdia",
			FullBox("mdhd", make([]byte, 20)),
			FullBox("hdlr", make([]byte, 4), []byte("soun"), make([]byte, 13)),
			Box("minf", FullBox("smhd", make([]byte, 4)), stbl),
		),
	)
	parts := [][]byte{FullBox("mvhd", make([]byte, 96)), trak}
	if !o.NoUdta {
		hdlr := FullBox("hdlr", make([]byte, 4), []byte("mdir"), make([]byte, 13))
		parts = append(parts, Box("udta", FullBox("meta", hdlr, Box("ilst", o.Items...))))
	}
	return Box("moov", parts...)
}

// sampleDescription builds the stsd box for codec (see M4AOptions.Codec).
func sampleDescription(codec string) []byte {
	if codec == "" {
		return FullBox("stsd", make([]byte, 4)) // no entries
	}
	return FullBox("stsd", binary.BigEndian.AppendUint32(nil, 1), SampleEntry(codec))
}

// SampleEntry builds an audio sample entry for codec ("aac", "alac" or a
// four-character type): the AudioSampleEntry fields of a 16-bit stereo
// 44.1 kHz stream, then the codec's configuration box, enough for a reader
// that looks at the entry's type.
func SampleEntry(codec string) []byte {
	fields := make([]byte, 28)
	binary.BigEndian.PutUint16(fields[6:8], 1)           // data reference index
	binary.BigEndian.PutUint16(fields[16:18], 2)         // channels
	binary.BigEndian.PutUint16(fields[18:20], 16)        // sample size
	binary.BigEndian.PutUint32(fields[24:28], 44100<<16) // sample rate, 16.16 fixed point
	switch codec {
	case "aac":
		return Box("mp4a", fields, FullBox("esds", make([]byte, 27)))
	case "alac":
		return Box("alac", fields, FullBox("alac", make([]byte, 24)))
	}
	return Box(codec, fields)
}

// box is one box found by the test reader.
type box struct {
	typ     string
	start   int64 // offset of the box in the file
	payload []byte
	off     int64 // offset of the payload in the file
}

// boxes walks the consecutive boxes of data, which starts at file offset
// base. It reads the three header forms on its own, independently of
// mp4tag, so tests can check mp4tag's output with it.
func boxes(data []byte, base int64) ([]box, error) {
	var out []box
	for pos := 0; pos+8 <= len(data); {
		size, hdr := int(binary.BigEndian.Uint32(data[pos:])), 8
		switch size {
		case 0:
			size = len(data) - pos
		case 1:
			if pos+16 > len(data) {
				return nil, fmt.Errorf("%w: short largesize at %d", ErrBadFile, base+int64(pos))
			}
			size, hdr = int(binary.BigEndian.Uint64(data[pos+8:])), 16 //nolint:gosec // G115: bounded below
		}
		if size < hdr || pos+size > len(data) {
			return nil, fmt.Errorf("%w: box of %d bytes at %d", ErrBadFile, size, base+int64(pos))
		}
		out = append(out, box{
			typ: string(data[pos+4 : pos+8]), start: base + int64(pos),
			payload: data[pos+hdr : pos+size], off: base + int64(pos+hdr),
		})
		pos += size
	}
	return out, nil
}

// find follows a path of box types from the top of file, such as
// "moov", "udta", and returns every box at its end.
func find(file []byte, path ...string) ([]box, error) {
	level := []box{{payload: file}}
	for i, typ := range path {
		var next []box
		for _, parent := range level {
			payload, off := parent.payload, parent.off
			if i > 0 && parent.typ == "meta" {
				payload, off = payload[4:], off+4 // past the full box's version and flags
			}
			kids, err := boxes(payload, off)
			if err != nil {
				return nil, err
			}
			for _, k := range kids {
				if k.typ == typ {
					next = append(next, k)
				}
			}
		}
		level = next
	}
	return level, nil
}

// MdatPayload returns the audio payload of file's top-level mdat and its
// offset in the file.
func MdatPayload(file []byte) ([]byte, int64, error) {
	mdat, err := find(file, "mdat")
	if err != nil {
		return nil, 0, err
	}
	if len(mdat) != 1 {
		return nil, 0, fmt.Errorf("%w: %d mdat boxes", ErrBadFile, len(mdat))
	}
	return mdat[0].payload, mdat[0].off, nil
}

// ChunkOffsets returns every stco and co64 entry of file's tracks, in
// order.
func ChunkOffsets(file []byte) ([]int64, error) {
	stbls, err := find(file, "moov", "trak", "mdia", "minf", "stbl")
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, stbl := range stbls {
		kids, err := boxes(stbl.payload, stbl.off)
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			width := map[string]int{"stco": 4, "co64": 8}[k.typ]
			if width == 0 {
				continue
			}
			p := k.payload
			if len(p) < 8 {
				return nil, fmt.Errorf("%w: %s of %d bytes", ErrBadFile, k.typ, len(p))
			}
			n := int(binary.BigEndian.Uint32(p[4:8]))
			if 8+n*width > len(p) {
				return nil, fmt.Errorf("%w: %d entries in a %d-byte %s", ErrBadFile, n, len(p), k.typ)
			}
			for i := range n {
				e := p[8+i*width:]
				if width == 4 {
					out = append(out, int64(binary.BigEndian.Uint32(e)))
				} else {
					out = append(out, int64(binary.BigEndian.Uint64(e))) //nolint:gosec // G115: test files are tiny
				}
			}
		}
	}
	return out, nil
}

// IlstTypes returns the types of the atoms in file's ilst, in order.
func IlstTypes(file []byte) ([]string, error) {
	ilst, err := find(file, "moov", "udta", "meta", "ilst")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range ilst {
		kids, err := boxes(l.payload, l.off)
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			out = append(out, k.typ)
		}
	}
	return out, nil
}

// JPEG is the shortest byte string with JPEG magic; mp4tag only sniffs it.
func JPEG() []byte { return []byte{0xFF, 0xD8, 0xFF, 0xE0, 'j', 'p', 'g'} }

// PNG is the PNG signature followed by a byte.
func PNG() []byte { return []byte("\x89PNG\r\n\x1a\nX") }
