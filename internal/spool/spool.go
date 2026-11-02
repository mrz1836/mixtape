// Package spool buffers a whole recording session on disk.
//
// Every captured frame is appended to ~30-second chunk files of raw PCM, and
// a sparse index of (frame, capture time) anchors maps the monotonic clock
// to frame positions. The engine never holds audio in memory: it decides
// where each song starts and ends from Spotify's position reports, then
// reads exactly that range back out. Memory use is one chunk's write buffer,
// whatever the session length; chunks behind the oldest open song are
// pruned.
package spool

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mrz1836/mixtape/internal/capture"
)

const (
	// chunkSeconds is the length of one chunk file.
	chunkSeconds = 30
	// anchorSpacing thins the time index: one anchor per this much audio.
	anchorSpacing = 50 * time.Millisecond
	// writeBuffer is the size of the append buffer.
	writeBuffer = 64 << 10
	// readBuffer is the size of the copy buffer used by ReadRange.
	readBuffer = 256 << 10

	chunkPrefix = "chunk-"
	chunkSuffix = ".pcm"
)

var (
	// ErrClosed reports use of a closed spool.
	ErrClosed = errors.New("spool: closed")
	// ErrFormat reports an unusable PCM format.
	ErrFormat = errors.New("spool: invalid format")
	// ErrShortBlock reports a block whose PCM is shorter than its frame count.
	ErrShortBlock = errors.New("spool: block shorter than its frame count")
)

// Spool is an append-only, chunked PCM store with a time index. One
// goroutine may Append while another reads (FrameAt, TimeAt, ReadRange,
// Prune); ReadRange and Prune must not run concurrently with each other.
type Spool struct {
	mu          sync.Mutex
	dir         string
	format      capture.Format
	bpf         int64 // bytes per frame
	chunkFrames int64

	chunks []*chunk // retained chunks, oldest first; the last one is written
	w      *bufio.Writer
	next   int // index for the next chunk file name

	frames  int64 // frames appended so far
	anchors []anchor
	closed  bool
}

type chunk struct {
	path   string
	start  int64 // first frame
	frames int64 // frames written (flushed or buffered)
	f      *os.File
}

type anchor struct {
	frame int64
	at    time.Time
}

// Open creates a spool in dir for audio in format f. Chunk files left in
// dir by an earlier, crashed session are removed.
func Open(dir string, f capture.Format) (*Spool, error) {
	if f.Rate <= 0 || f.Channels <= 0 {
		return nil, fmt.Errorf("%w: %d Hz, %d channels", ErrFormat, f.Rate, f.Channels)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	if err := removeChunks(dir); err != nil {
		return nil, err
	}
	return &Spool{
		dir:         dir,
		format:      f,
		bpf:         int64(f.BytesPerFrame()),
		chunkFrames: int64(f.Rate) * chunkSeconds,
	}, nil
}

// Format is the PCM format of the spool.
func (s *Spool) Format() capture.Format { return s.format }

// Append stores the frames of b and indexes their capture time. b.PCM is
// copied, so the caller may reuse it.
func (s *Spool) Append(b capture.Block) error {
	if b.Frames <= 0 {
		return nil
	}
	n := int64(b.Frames) * s.bpf
	if int64(len(b.PCM)) < n {
		return fmt.Errorf("%w: %d bytes for %d frames", ErrShortBlock, len(b.PCM), b.Frames)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.index(b.At)

	data := b.PCM[:n]
	for len(data) > 0 {
		c, err := s.writable()
		if err != nil {
			return err
		}
		room := (s.chunkFrames - c.frames) * s.bpf
		part := data
		if int64(len(part)) > room {
			part = data[:room]
		}
		if _, err := s.w.Write(part); err != nil {
			return fmt.Errorf("spool: write: %w", err)
		}
		written := int64(len(part)) / s.bpf
		c.frames += written
		s.frames += written
		data = data[len(part):]
	}
	return nil
}

// index records an anchor for the frame about to be appended, thinned to
// one per anchorSpacing. Timestamps that go backwards are ignored.
func (s *Spool) index(at time.Time) {
	if at.IsZero() {
		return
	}
	if k := len(s.anchors); k > 0 {
		last := s.anchors[k-1]
		if at.Sub(last.at) < anchorSpacing {
			return
		}
	}
	s.anchors = append(s.anchors, anchor{frame: s.frames, at: at})
}

// writable returns the chunk being written, rolling to a new one when the
// current chunk is full.
func (s *Spool) writable() (*chunk, error) {
	if k := len(s.chunks); k > 0 && s.chunks[k-1].frames < s.chunkFrames {
		return s.chunks[k-1], nil
	}
	if s.w != nil {
		if err := s.w.Flush(); err != nil {
			return nil, fmt.Errorf("spool: flush: %w", err)
		}
	}
	path := filepath.Join(s.dir, fmt.Sprintf("%s%06d%s", chunkPrefix, s.next, chunkSuffix))
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	s.next++
	c := &chunk{path: path, start: s.frames, f: f}
	s.chunks = append(s.chunks, c)
	if s.w == nil {
		s.w = bufio.NewWriterSize(f, writeBuffer)
	} else {
		s.w.Reset(f)
	}
	return c, nil
}

// Frames is the index of the next frame to be appended: the total count
// of frames captured so far.
func (s *Spool) Frames() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames
}

// Oldest is the first frame still retained.
func (s *Spool) Oldest() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.oldestLocked()
}

func (s *Spool) oldestLocked() int64 {
	if len(s.chunks) == 0 {
		return s.frames
	}
	return s.chunks[0].start
}

// FrameAt maps a monotonic time to a frame position by linear interpolation
// between anchors, extrapolating at the nominal rate beyond them. The
// result may lie outside [Oldest, Frames).
func (s *Spool) FrameAt(t time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.anchors
	switch {
	case len(a) == 0:
		return s.frames
	case len(a) == 1 || !t.After(a[0].at):
		return a[0].frame + s.format.Frames(t.Sub(a[0].at))
	}
	last := a[len(a)-1]
	if !t.Before(last.at) {
		return last.frame + s.format.Frames(t.Sub(last.at))
	}
	// First anchor strictly after t; i ≥ 1 because t is after a[0].
	i := sort.Search(len(a), func(i int) bool { return a[i].at.After(t) })
	lo, hi := a[i-1], a[i]
	frac := float64(t.Sub(lo.at)) / float64(hi.at.Sub(lo.at))
	return lo.frame + int64(frac*float64(hi.frame-lo.frame))
}

// TimeAt maps a frame position to its capture time; the inverse of FrameAt.
func (s *Spool) TimeAt(frame int64) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.anchors
	switch {
	case len(a) == 0:
		return time.Time{}
	case len(a) == 1 || frame <= a[0].frame:
		return a[0].at.Add(s.format.Duration(frame - a[0].frame))
	}
	last := a[len(a)-1]
	if frame >= last.frame {
		return last.at.Add(s.format.Duration(frame - last.frame))
	}
	i := sort.Search(len(a), func(i int) bool { return a[i].frame > frame })
	lo, hi := a[i-1], a[i]
	frac := float64(frame-lo.frame) / float64(hi.frame-lo.frame)
	return lo.at.Add(time.Duration(frac * float64(hi.at.Sub(lo.at))))
}

// ReadRange writes the PCM of frames [from, to) to w, clipped to the
// retained range [Oldest, Frames). It returns the number of frames written.
func (s *Spool) ReadRange(from, to int64, w io.Writer) (int64, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	if s.w != nil {
		if err := s.w.Flush(); err != nil {
			s.mu.Unlock()
			return 0, fmt.Errorf("spool: flush: %w", err)
		}
	}
	from = max(from, s.oldestLocked())
	to = min(to, s.frames)
	type span struct {
		f      *os.File
		off    int64 // byte offset in the file
		frames int64
	}
	var spans []span
	for _, c := range s.chunks {
		lo, hi := max(from, c.start), min(to, c.start+c.frames)
		if lo < hi {
			spans = append(spans, span{f: c.f, off: (lo - c.start) * s.bpf, frames: hi - lo})
		}
	}
	s.mu.Unlock()

	// The spans lie before the flushed end of an append-only file, so they
	// can be read without the lock while capture keeps appending.
	var done int64
	buf := make([]byte, readBuffer-readBuffer%int(s.bpf))
	for _, sp := range spans {
		r := io.NewSectionReader(sp.f, sp.off, sp.frames*s.bpf)
		n, err := io.CopyBuffer(w, r, buf)
		done += n / s.bpf
		if err != nil {
			return done, fmt.Errorf("spool: read: %w", err)
		}
	}
	return done, nil
}

// Prune deletes every chunk that lies entirely before frame before. The
// chunk being written is always kept. Anchors before the new oldest frame
// are dropped, except the last one at or before before, which keeps the
// time mapping accurate at the boundary.
func (s *Spool) Prune(before int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	var errs []error
	keep := 0
	for keep < len(s.chunks)-1 && s.chunks[keep].start+s.chunks[keep].frames <= before {
		c := s.chunks[keep]
		errs = append(errs, c.f.Close(), os.Remove(c.path))
		keep++
	}
	s.chunks = s.chunks[keep:]

	cut := 0
	for cut+1 < len(s.anchors) && s.anchors[cut+1].frame <= before {
		cut++
	}
	s.anchors = s.anchors[cut:]
	return errors.Join(errs...)
}

// Close closes and removes every chunk file. It is idempotent.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	for _, c := range s.chunks {
		errs = append(errs, c.f.Close(), os.Remove(c.path))
	}
	s.chunks, s.w, s.anchors = nil, nil, nil
	return errors.Join(errs...)
}

// removeChunks deletes chunk files left in dir by an earlier session.
func removeChunks(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("spool: %w", err)
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(name, chunkPrefix) && strings.HasSuffix(name, chunkSuffix) {
			errs = append(errs, os.Remove(filepath.Join(dir, name)))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("spool: removing stale chunks: %w", err)
	}
	return nil
}
