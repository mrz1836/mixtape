package mp4tag

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Retagged is an M4A file on disk with new tags, ready to be written out.
// [TagFile] has rebuilt its moov in memory and checked the whole layout;
// WriteTo streams the result, copying the audio straight from the file, so
// memory use stays at the size of moov however long the song is.
type Retagged struct {
	f     *os.File
	parts []filePart
	size  int64
}

// filePart is one piece of the output: bytes in memory (the new moov), or
// a range of the source file.
type filePart struct {
	data   []byte
	off, n int64
}

// TagFile prepares the M4A file at path with tags embedded, as [Tag] would
// return them for the file's bytes: every tag or layout error is reported
// here, before anything is written. Close the result when done; the file
// must not change until then.
func TagFile(path string, tags Tags) (*Retagged, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	r, err := prepare(f, tags)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

// topBox is a top-level box of a file.
type topBox struct {
	h   header
	off int64
}

// prepare lays out the retagged file the way Tag does: top-level boxes in
// order, moov rebuilt, free padding dropped, chunk offsets shifted.
func prepare(f *os.File, tags Tags) (*Retagged, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var top []topBox
	err = scanFile(f, func(h header, off int64) (bool, error) {
		top = append(top, topBox{h: h, off: off})
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	// Like parseBoxes: bytes after the last box that cannot hold a header.
	if n := len(top); n > 0 {
		if end := top[n-1].off + top[n-1].h.size; end != info.Size() {
			return nil, fmt.Errorf("%w: %d bytes at offset %d", errTrailingBytes, info.Size()-end, end)
		}
	}
	moov, mdat := firstTop(top, "moov"), firstTop(top, "mdat")
	if moov == nil {
		return nil, errNoMoov
	}
	if mdat == nil {
		return nil, errNoMdat
	}
	buf, err := readMoov(f, moov.h, moov.off)
	if err != nil {
		return nil, err
	}
	newMoov, err := retagMoov(&box{typ: moov.h.typ, data: buf, off: int(moov.off), hdr: moov.h.hdr}, tags)
	if err != nil {
		return nil, err
	}

	r := &Retagged{f: f}
	var shift int64
	for i := range top {
		b := &top[i]
		if b == mdat {
			shift = r.size - b.off
		}
		part := filePart{off: b.off, n: b.h.size}
		switch string(b.h.typ[:]) {
		case "free":
			continue
		case "moov":
			part = filePart{data: newMoov, n: int64(len(newMoov))}
		}
		r.parts = append(r.parts, part)
		r.size += part.n
	}
	// Every moov part shares newMoov, so this patches them all, as in Tag.
	if err := fixChunkOffsets(newMoov, mdat.off+int64(mdat.h.hdr), shift); err != nil {
		return nil, err
	}
	return r, nil
}

func firstTop(top []topBox, typ string) *topBox {
	for i := range top {
		if string(top[i].h.typ[:]) == typ {
			return &top[i]
		}
	}
	return nil
}

// Size is the length of the retagged file.
func (r *Retagged) Size() int64 { return r.size }

// WriteTo writes the retagged file to w.
func (r *Retagged) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for _, p := range r.parts {
		var (
			n   int64
			err error
		)
		if p.data != nil {
			var k int
			k, err = w.Write(p.data)
			n = int64(k)
		} else {
			n, err = io.Copy(w, io.NewSectionReader(r.f, p.off, p.n))
		}
		total += n
		if err != nil {
			return total, err
		}
		if n != p.n {
			return total, fmt.Errorf("%w: %d of %d bytes at offset %d", io.ErrUnexpectedEOF, n, p.n, p.off)
		}
	}
	return total, nil
}

// Close releases the source file.
func (r *Retagged) Close() error { return r.f.Close() }
