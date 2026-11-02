package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mrz1836/mixtape/internal/spotify"
)

// PlayerSettings are the Spotify settings a session may change. A nil
// field was not touched.
type PlayerSettings struct {
	Shuffle *bool `json:"shuffle,omitempty"`
	Repeat  *bool `json:"repeat,omitempty"`
	Volume  *int  `json:"volume,omitempty"`
}

// Empty reports whether no setting is recorded.
func (p PlayerSettings) Empty() bool { return p.Shuffle == nil && p.Repeat == nil && p.Volume == nil }

// SpotifyState records what a session changed in Spotify: the original
// values, saved before anything was changed, and the values it set.
type SpotifyState struct {
	Original PlayerSettings `json:"original"`
	Set      PlayerSettings `json:"set"`
	Restored bool           `json:"restored"`
}

// NeedsRestore reports whether settings were changed and not yet put back.
func (s *SpotifyState) NeedsRestore() bool { return s != nil && !s.Restored && !s.Set.Empty() }

// Restorer is the part of the Spotify player that Restore uses.
type Restorer interface {
	Poll(ctx context.Context) spotify.Snapshot
	SetShuffle(ctx context.Context, on bool) error
	SetRepeat(ctx context.Context, on bool) error
	SetVolume(ctx context.Context, volume int) error
}

// Restore puts back every setting the session changed, but only where
// Spotify still holds the value the session set: a change the user made
// since then wins. It returns the names of the settings restored.
func Restore(ctx context.Context, p Restorer, st SpotifyState) ([]string, error) {
	return restore(ctx, p, st, volumeSettle)
}

// volumeSettle is how long Spotify takes before a new volume reads back.
const volumeSettle = 300 * time.Millisecond

// restore is Restore with the volume's settle time as a parameter, so the
// tests need not wait for a real Spotify.
func restore(ctx context.Context, p Restorer, st SpotifyState, settle time.Duration) ([]string, error) {
	snap := p.Poll(ctx)
	if snap.Err != nil {
		return nil, snap.Err
	}
	var (
		restored []string
		errs     []error
	)
	record := func(name string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", name, err))
			return
		}
		restored = append(restored, name)
	}
	if v, ok := toRestore(st.Original.Shuffle, st.Set.Shuffle, snap.Shuffle); ok {
		record("shuffle", p.SetShuffle(ctx, v))
	}
	if v, ok := toRestore(st.Original.Repeat, st.Set.Repeat, snap.Repeat); ok {
		record("repeat", p.SetRepeat(ctx, v))
	}
	if v, ok := toRestore(st.Original.Volume, st.Set.Volume, snap.Volume); ok {
		record("volume", setVolume(ctx, p, v, settle))
	}
	return restored, errors.Join(errs...)
}

// toRestore returns the original value of a setting to put back: the
// session changed it (orig differs from set) and Spotify still holds the
// value it set (now).
func toRestore[T comparable](orig, set *T, now T) (T, bool) {
	var zero T
	if orig == nil || set == nil || now != *set || *orig == *set {
		return zero, false
	}
	return *orig, true
}

// setVolume sets v and makes sure Spotify reads it back as v. Spotify keeps
// the volume at a finer scale and reads most values back one lower than set
// (47 reads as 46), so a short read is nudged up. Each try waits settle for
// the new value to read back, or until ctx ends.
func setVolume(ctx context.Context, p Restorer, v int, settle time.Duration) error {
	target := v
	for range 3 {
		if err := p.SetVolume(ctx, min(target, 100)); err != nil {
			return err
		}
		if !wait(ctx, settle) {
			return nil // the set went through; there is no time left to check it
		}
		// The set went through; a failed or exact read-back ends the nudging.
		got := p.Poll(ctx)
		if got.Err == nil && got.Volume < v && target < 100 {
			target += v - got.Volume
			continue
		}
		break
	}
	return nil
}

// wait sleeps for d and reports whether ctx was still live at the end.
func wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
