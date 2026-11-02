package session

import (
	"context"
	"errors"
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
	snap := p.Poll(ctx)
	if snap.Err != nil {
		return nil, snap.Err
	}
	var (
		restored []string
		errs     []error
	)
	if st.Set.Shuffle != nil && st.Original.Shuffle != nil && snap.Shuffle == *st.Set.Shuffle &&
		*st.Original.Shuffle != *st.Set.Shuffle {
		if err := p.SetShuffle(ctx, *st.Original.Shuffle); err != nil {
			errs = append(errs, err)
		} else {
			restored = append(restored, "shuffle")
		}
	}
	if st.Set.Repeat != nil && st.Original.Repeat != nil && snap.Repeat == *st.Set.Repeat &&
		*st.Original.Repeat != *st.Set.Repeat {
		if err := p.SetRepeat(ctx, *st.Original.Repeat); err != nil {
			errs = append(errs, err)
		} else {
			restored = append(restored, "repeat")
		}
	}
	if st.Set.Volume != nil && st.Original.Volume != nil && snap.Volume == *st.Set.Volume &&
		*st.Original.Volume != *st.Set.Volume {
		if err := setVolume(ctx, p, *st.Original.Volume); err != nil {
			errs = append(errs, err)
		} else {
			restored = append(restored, "volume")
		}
	}
	return restored, errors.Join(errs...)
}

// volumeSettle is how long Spotify takes before a new volume reads back.
const volumeSettle = 300 * time.Millisecond

// setVolume sets v and makes sure Spotify reads it back as v. Spotify keeps
// the volume at a finer scale and reads most values back one lower than set
// (47 reads as 46), so a short read is nudged up.
func setVolume(ctx context.Context, p Restorer, v int) error {
	target := v
	for range 3 {
		if err := p.SetVolume(ctx, min(target, 100)); err != nil {
			return err
		}
		time.Sleep(volumeSettle)
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
