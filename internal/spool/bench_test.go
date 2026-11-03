package spool

import (
	"io"
	"testing"
	"time"

	"github.com/mrz1836/mixtape/internal/capture"
)

// benchFormat is the format a real session spools: 48 kHz stereo.
func benchFormat() capture.Format { return capture.Format{Rate: 48000, Channels: 2} }

// BenchmarkAppend measures spooling one 10 ms capture block, the unit the
// backends deliver about a hundred times a second.
func BenchmarkAppend(b *testing.B) {
	f := benchFormat()
	s, err := Open(b.TempDir(), f)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	const frames = 480
	pcm := make([]byte, frames*f.BytesPerFrame())
	start := time.Now()
	b.SetBytes(int64(len(pcm)))
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		at := start.Add(time.Duration(i) * 10 * time.Millisecond)
		if err := s.Append(capture.Block{PCM: pcm, Frames: frames, At: at}); err != nil {
			b.Fatal(err)
		}
	}
}

// spooled returns a spool holding d of audio in 10 ms blocks.
func spooled(b *testing.B, d time.Duration) (*Spool, time.Time) {
	b.Helper()
	f := benchFormat()
	s, err := Open(b.TempDir(), f)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	const frames = 480
	pcm := make([]byte, frames*f.BytesPerFrame())
	start := time.Now()
	for i := range int(d / (10 * time.Millisecond)) {
		if err := s.Append(capture.Block{PCM: pcm, Frames: frames, At: start.Add(time.Duration(i) * 10 * time.Millisecond)}); err != nil {
			b.Fatal(err)
		}
	}
	return s, start
}

// BenchmarkFrameAt measures mapping a time to a frame in a 10-minute spool
// (about 12 000 anchors): the segmenter does it for every observation.
func BenchmarkFrameAt(b *testing.B) {
	s, start := spooled(b, 10*time.Minute)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		_ = s.FrameAt(start.Add(time.Duration(i%600) * time.Second))
	}
}

// BenchmarkReadRange measures reading a 3-minute song back out of the spool.
func BenchmarkReadRange(b *testing.B) {
	s, _ := spooled(b, 4*time.Minute)
	f := benchFormat()
	from, to := f.Frames(30*time.Second), f.Frames(210*time.Second)
	b.SetBytes((to - from) * int64(f.BytesPerFrame()))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.ReadRange(from, to, io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
