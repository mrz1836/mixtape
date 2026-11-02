package cli

// spotifyProcess is what a look at the process list found.
type spotifyProcess int

const (
	spotifyUnknown spotifyProcess = iota // the process list could not be read
	spotifyStopped
	spotifyRunning
)

// spotifyStatus checks whether the Spotify app is running. err is set, and
// the state unknown, when the process list could not be read.
func (a *app) spotifyStatus() (spotifyProcess, error) {
	running, err := a.deps.services.spotifyRunning()
	switch {
	case err != nil:
		return spotifyUnknown, err
	case running:
		return spotifyRunning, nil
	}
	return spotifyStopped, nil
}
