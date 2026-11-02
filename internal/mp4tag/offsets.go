package mp4tag

import (
	"encoding/binary"
	"fmt"
	"math"
)

// fixChunkOffsets rewrites stco/co64 entries inside moov so they follow the
// moved mdat. Only the moov/trak/mdia/minf/stbl chain can contain them.
func fixChunkOffsets(moov []byte, oldMdatData, shift int64) error {
	if shift == 0 {
		return nil
	}
	return shiftTables(moov[8:], oldMdatData, shift)
}

// shiftTables shifts the chunk-offset tables among the boxes of payload,
// and those in the track containers below them.
func shiftTables(payload []byte, oldMdatData, shift int64) error {
	for b, err := range childrenOf(payload) {
		if err != nil {
			return err
		}
		switch string(b.typ[:]) {
		case "trak", "mdia", "minf", "stbl":
			err = shiftTables(b.payload(), oldMdatData, shift)
		case "stco":
			err = shiftOffsets32(b.payload(), oldMdatData, shift)
		case "co64":
			err = shiftOffsets64(b.payload(), oldMdatData, shift)
		}
		if err != nil {
			return err
		}
	}
	return nil
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
