package engine

import (
	"testing"
)

// BenchmarkPeak measures the loudness check every captured block passes
// through on the capture goroutine (10 ms of 48 kHz stereo).
func BenchmarkPeak(b *testing.B) {
	pcm := make([]byte, 480*4)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	b.SetBytes(int64(len(pcm)))
	b.ReportAllocs()
	for b.Loop() {
		_ = peak(pcm)
	}
}

// BenchmarkSegmentize measures cutting a 10-minute song observed every
// 150 ms with a pause in the middle: about 4 000 observations, the most a
// take normally holds.
func BenchmarkSegmentize(b *testing.B) {
	s := song{dur: 600, audioStart: 2, pauseAt: 300, pauseFor: 5}
	tl := s.timeline(600)
	tn := DefaultTuning()
	b.ReportAllocs()
	for b.Loop() {
		seg := Segmentize(tl, linClock{t0: simT0}, simRate, 0, tn)
		if !seg.Complete {
			b.Fatal(seg.Reason)
		}
	}
}

// BenchmarkSplitRuns isolates run splitting, where recentAnchor takes a
// median of the last few samples for every observation.
func BenchmarkSplitRuns(b *testing.B) {
	s := song{dur: 600, audioStart: 2}
	tl := s.timeline(600)
	tn := DefaultTuning()
	b.ReportAllocs()
	for b.Loop() {
		_ = splitRuns(tl, linClock{t0: simT0}, simRate, tl.Duration.Seconds(), tn)
	}
}
