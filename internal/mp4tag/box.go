package mp4tag

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
)

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

// box is a box in a byte slice.
type box struct {
	typ  [4]byte
	data []byte // whole box, header included
	off  int    // where the box starts in the bytes it was read from
	hdr  int    // header length: 8, or 16 for a 64-bit largesize box
}

// boxesOf yields the boxes laid end to end in payload. A box may use a
// 64-bit largesize or size 0 ("to the end of its container"). A malformed
// header ends the walk with its error, yielded beside a box that holds
// only off, where the header starts, for the caller to report. Fewer than
// 8 bytes after the last box cannot hold a header and end the walk without
// an error: inside udta a 4-byte terminator is common, and parseBoxes
// rejects them at the top level.
func boxesOf(payload []byte) iter.Seq2[box, error] {
	return func(yield func(box, error) bool) {
		pos := 0
		for pos+8 <= len(payload) {
			h, err := readHeader(payload[pos:], int64(len(payload)-pos))
			if err != nil {
				yield(box{off: pos}, err)
				return
			}
			size := int(h.size)
			if !yield(box{typ: h.typ, data: payload[pos : pos+size], off: pos, hdr: h.hdr}, nil) {
				return
			}
			pos += size
		}
	}
}

// childrenOf yields the boxes in a container's payload as boxesOf does,
// and gives a malformed child's error its offset in the container. The top
// level reports a file offset instead, which is why boxesOf leaves the
// wording to its callers.
func childrenOf(payload []byte) iter.Seq2[box, error] {
	return func(yield func(box, error) bool) {
		for b, err := range boxesOf(payload) {
			if err != nil {
				err = fmt.Errorf("%w (child at %d)", err, b.off)
			}
			if !yield(b, err) {
				return
			}
		}
	}
}

// parseBoxes parses the top-level boxes of data, which must fill it: bytes
// left after the last box are an error. Data too short to hold any box is
// not one; it has no moov or mdat, which callers report.
func parseBoxes(data []byte) ([]box, error) {
	var boxes []box
	end := 0 // where the last box ends
	for b, err := range boxesOf(data) {
		if err != nil {
			return nil, fmt.Errorf("%w at offset %d", err, b.off)
		}
		boxes = append(boxes, b)
		end = b.off + len(b.data)
	}
	if end != len(data) && len(boxes) > 0 {
		return nil, fmt.Errorf("%w: %d bytes at offset %d", errTrailingBytes, len(data)-end, end)
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

// raw returns the box's bytes ready to be copied into a rebuilt container:
// a size of 0 ("to the end of the container") is replaced by the box's
// real size, which stays true wherever the box lands.
func (b box) raw() []byte {
	n := len(b.data)
	if binary.BigEndian.Uint32(b.data[0:4]) != 0 || n > math.MaxUint32 {
		return b.data
	}
	out := bytes.Clone(b.data)
	binary.BigEndian.PutUint32(out[0:4], uint32(n))
	return out
}

// child returns the first direct child of type want, or nil. It walks
// every child, so a malformed one after it is still an error.
func child(payload []byte, want string) (*box, error) {
	var found *box
	for b, err := range childrenOf(payload) {
		if err != nil {
			return nil, err
		}
		if string(b.typ[:]) == want && found == nil {
			found = &b
		}
	}
	return found, nil
}

// childrenExcept returns the raw bytes of every direct child not of type skip.
func childrenExcept(payload []byte, skip string) ([][]byte, error) {
	var keep [][]byte
	for b, err := range childrenOf(payload) {
		if err != nil {
			return nil, err
		}
		if string(b.typ[:]) != skip {
			keep = append(keep, b.raw())
		}
	}
	return keep, nil
}
