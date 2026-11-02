package engine

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/wav"
)

// renderBuffer is the WAV write buffer.
const renderBuffer = 1 << 20

// rangeReader is the part of the spool render reads from.
type rangeReader interface {
	ReadRange(from, to int64, w io.Writer) (int64, error)
}

// render writes a segment's frames to a WAV in the work directory.
func (e *Engine) render(t *take, seg Segment) (string, error) {
	// The engine lock is held only to pick the spool and its bounds: render
	// runs on the engine goroutine, as do pruning and reopening, so the
	// spool stays put while it is read, and capture keeps appending (the
	// spool guards itself) instead of waiting for the whole WAV write.
	e.spoolMu.Lock()
	sp := e.spool
	if sp == nil {
		e.spoolMu.Unlock()
		return "", errNoSpool
	}
	oldest, newest := sp.Oldest(), sp.Frames()
	e.spoolMu.Unlock()
	ranges := clipRanges(seg.Ranges, oldest, newest)
	if len(ranges) == 0 {
		return "", errNoSpool
	}
	if err := os.MkdirAll(e.cfg.WorkDir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d.wav", safeName(t.item.Track.ID), t.startedAt.UnixNano())
	path := filepath.Join(e.cfg.WorkDir, name)
	if err := writeWAV(path, sp, sp.Format(), ranges); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// clipRanges limits ranges to the frames the spool still holds,
// [oldest, newest), dropping those left empty.
func clipRanges(ranges []Range, oldest, newest int64) []Range {
	out := make([]Range, 0, len(ranges))
	for _, r := range ranges {
		r.From, r.To = max(r.From, oldest), min(r.To, newest)
		if r.Len() > 0 {
			out = append(out, r)
		}
	}
	return out
}

// writeWAV writes the frames of ranges, read from src, as one WAV at path.
func writeWAV(path string, src rangeReader, f capture.Format, ranges []Range) error {
	var total int64
	for _, r := range ranges {
		total += r.Len()
	}
	file, err := os.Create(filepath.Clean(path))
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(file, renderBuffer)
	err = wav.WriteHeader(w, total*int64(f.BytesPerFrame()), f.Rate, f.Channels)
	for _, r := range ranges {
		if err != nil {
			break
		}
		var n int64
		n, err = src.ReadRange(r.From, r.To, w)
		if err == nil && n != r.Len() {
			err = fmt.Errorf("%w: %d of %d frames", errShortRead, n, r.Len())
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	return err
}

// safeName keeps the letters and digits of s, so it can name a file.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, s)
}
