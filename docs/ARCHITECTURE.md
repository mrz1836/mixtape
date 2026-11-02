# Mixtape architecture

This document explains how Mixtape turns "Spotify is playing" into exact,
complete, tagged files, and why each part is built the way it is. It is for
contributors and for anyone debugging a recording.

<br>

## The problem

Spotify gives a desktop app no audio API. What it does give, over
AppleScript, is the player state: which track is loaded, whether it plays,
and its position. Mixtape captures the Mac's audio and lines the two up:

- **Audio** arrives as timestamped PCM from one of two capture backends.
- **Player observations** arrive about every 120 ms from one long-running
  `osascript` process.
- Both carry times on the same monotonic clock, so every observation can be
  placed on the audio timeline.

The hard part is not recording; it is knowing, to the frame, where each song
starts and ends, and refusing to call a song done unless it was captured
start to finish exactly once.

<br>

## Packages

```
cmd/mixtape            two-line main: build metadata in, exit code out
internal/cli           the command tree (cobra), output, errors and exit codes
internal/config        config.json + MIXTAPE_* environment + flags
internal/spotify       links, the AppleScript player, the catalog, prefs
internal/capture       backend-neutral types and method selection (no cgo)
  capture/tap          Core Audio process tap (Objective-C, macOS 14.2+)
  capture/device       capture device such as BlackHole (malgo)
internal/spool         append-only chunked PCM on disk with a time index
internal/engine        plan, steer, observe, segment, commit
internal/library       folders, manifests, naming, encode/tag/hash, verify
internal/mp4tag        pure-Go MP4 tag writer (ilst, ---- freeform, covr)
internal/session       recorder lock, session.json, event log, detach
internal/permission    macOS privacy permissions of the host app (C)
internal/fileutil      atomic writes and copies
internal/wav           the 44-byte PCM WAV header
```

Cgo lives only in `capture/tap` and `capture/device`. Everything that
decides anything (the engine, the segmenter, the library) is pure Go and is
tested with fakes and with replays of real Spotify traces.

<br>

## Capture

Both backends implement `capture.Source` and deliver interleaved 16-bit PCM
at the device's native rate (no resampling), each block stamped with the
monotonic time of its first frame.

| | Process tap | BlackHole device |
|---|---|---|
| Install | nothing | `brew install blackhole-2ch` + a Multi-Output Device |
| Records | Spotify only | everything the Mac plays |
| macOS | 14.2+ | any |
| Permission | *System Audio Recording Only*, for the app that runs mixtape | Microphone, for that app |
| Over ssh / launchd | no (permission belongs to a GUI app) | yes |
| Can record muted | yes (`--mute`) | no |

The tap builds a private aggregate device around a `CATapDescription` of
every `com.spotify.client*` audio process. Its IO block runs on Core Audio's
real-time thread and touches no Go code: it converts Float32 to int16 into a
lock-free C ring and publishes the host time of its latest write under a
seqlock. A Go goroutine drains the ring every 10 ms. Listeners rebuild the
tap when Spotify's processes, the default output, or the rate change.

`auto` uses whatever `setup` verified, else the tap when the Mac supports it,
else the device. A session never switches backends midway.

**A denied permission delivers silence, not an error.** macOS gives the tap
(and the device) zeros when the responsible app has not been granted access.
The permission belongs to the app running mixtape (Terminal, iTerm, an IDE),
not to the mixtape binary.

`internal/permission` reads the state up front for the app macOS holds
responsible (`responsibility_get_pid_responsible_for_pid`):

| Permission | TCC service | Read with | Prompt |
|---|---|---|---|
| System audio recording (tap) | `kTCCServiceAudioCapture` | `TCCAccessPreflight` (private, loaded at run time) | none for apps that do not declare `NSAudioCaptureUsageDescription`, which includes Terminal and iTerm: switched on by hand under *System Audio Recording Only* |
| Microphone (device) | `kTCCServiceMicrophone` | `TCCAccessPreflight` | `TCCAccessRequest` |
| Automation (Spotify) | Apple Events | `AEDeterminePermissionToAutomateTarget` (public) | the same call, with `askUserIfNeeded` |

The *Screen & System Audio Recording* pane has two lists; only the bottom one,
*System Audio Recording Only*, grants `kTCCServiceAudioCapture`. A
screen-recording grant alone leaves the tap silent. When the private calls
are unavailable the state is "unknown" and the silence watchdog below is the
judge. The package is plain C, so only the tap links the Objective-C
runtime.

<br>

## The spool

Every captured frame goes to disk, in 30-second chunk files under
`~/.mixtape/spool`, with a sparse index of (frame, time) anchors one per
50 ms. `FrameAt(t)` interpolates between anchors. Memory stays at one chunk
whatever the session length; songs are frame ranges cut out of the spool
after they end, and old chunks are pruned behind the oldest open song.

<br>

## Observing Spotify

`spotify.Player.Watch` runs one `osascript` that loops: one Apple Event
round trip reads state, volume, shuffle, repeat, the track's metadata and,
last, `player position`, and `log`s it as one line. Reading the position
last puts the sample right before the line's arrival, which is its
timestamp. One persistent process costs a fraction of a process per poll and
has far less timing jitter.

The script never launches Spotify: every query sits behind `application
"Spotify" is running`. Every value spliced into AppleScript is a validated
base62 ID.

<br>

## Measured Spotify behavior

These come from `mixtape debug trace` runs against Spotify 1.2 on macOS 15
(BlackHole at 48 kHz, 100 ms polls). A trimmed trace is the golden fixture
`internal/engine/testdata/trace-complete.ndjson`.

| Behavior | Measurement | Consequence |
|---|---|---|
| Position updates | continuous; steady-state anchor spread 2–4 ms | the median of a run's anchors is exact |
| Latency | captured audio trails the reported position by ~0.14 s | `Tuning.Latency = 140 ms` |
| Paused position | ~0.16 s behind the last playing one | the paused value is the audio position: pauses are cut there |
| Start of a song | position reads exactly `0.000` for 1–4 polls while Spotify buffers | samples at exactly 0 are ignored |
| Start-over glitch | right after `play track`, Spotify plays ~0.2 s, jumps back to 0, then starts | a restart in the first 2 s resets the take; it is not a failed attempt |
| Track change | the new title and duration can appear one poll before the new URI | half-updated snapshots are ignored |
| Paused load | `play track` shows the new track paused for one poll before it plays | the take starts recording when it plays |
| Crossfade | the reported duration drops by the crossfade time ~1.4 s into a song; albums are exempt | detected from the first song (exit 3) |
| Prefs | `audio.crossfade_v2` in `prefs` is stale once `audio.crossfade_migrated_to_ps=true` | it is a warning, not a verdict |
| Settings writes | `set shuffling`/`set repeating` take effect, but a read in the same script still returns the old value | settings are read back on the next observation, never right after a write |
| Process tap | delivers callbacks (512 frames at 48 kHz, every 10.7 ms) of pure zeros until the host app has *System Audio Recording* permission | the silence watchdog, exit 5 |

Re-measure after Spotify updates with `mixtape debug trace --duration 2m`
while playing, pausing and skipping; the summary prints the onset offset for
each start.

<br>

## Segmenting a song

`engine.Segmentize` is a pure function from one track's observations to the
frame ranges to keep and a verdict.

1. **Anchors.** Each playing observation at time *t* with position *p*
   estimates the frame of the song's position 0:
   `anchor = frame(t) + Latency·rate − p·rate`.
2. **Runs.** A pause, or an anchor jumping by more than `SeekJump` (0.75 s),
   starts a new run. The anchor of a run is the median of its samples.
3. **Coverage.** Each run covers positions [start, end]: a run that began in
   the poll after the previous track ended starts at 0; a resumed run starts
   at the paused position; the last run ends at the duration if the last
   position was within `EndSlack` (1.5 s) of it and mixtape did not cause the
   change.
4. **Ranges.** Each run contributes `[anchor + start·rate, anchor + end·rate)`;
   the audio between runs (the pauses) is dropped, and `Margin` (150 ms) is
   added before the first and after the last frame.

A song is **complete** only if all of these hold:

- the first run started within `StartSlack` (0.5 s) of the top, and its
  start is in the spool;
- runs follow each other with gaps and overlaps under `Gap` (0.5 s): no
  forward or backward seek;
- it ended naturally, not because mixtape or the user moved on;
- the covered length is within [duration − 1.5 s, duration + 2 s];
- no sample deviates from its run's anchor by more than `MaxDrift` (0.5 s);
- no capture event (overflow, device or rate change) touched it;
- the capture was not silent for 10 s while the position advanced.

An incomplete song is discarded (or kept and flagged with
`--keep-partial`); in queue mode it is retried once from the top.

<br>

## The silence watchdog

If Spotify plays for 10 s and the capture has not yet seen one sample above
the silence floor in the whole session, capture is not working: the device
is not routed (exit 3) or the tap is not permitted (exit 5). After the first
audible sample, silence only invalidates the song it falls in.

<br>

## Steering (queue mode)

For each song to record, in order:

- If Spotify is already playing it from the top, recording simply continues.
- Otherwise mixtape sends `play track <uri> in context <album or playlist>`,
  so the next songs usually flow on with no gap, and waits up to 15 s (plus
  45 s while an ad plays).
- A relinked track (another ID with the same normalized title and lead
  artist, duration within 2 s) is accepted and its played ID recorded.
- If Spotify jumps past the requested song to a later one, the requested
  song is marked unavailable and the later one is recorded.
- A pause the user makes mid-song is respected: nothing is steered until
  playback resumes.

Shuffle is turned off for the session. Spotify is paused when the queue is
done. Every setting changed is saved to `session.json` first and restored at
the end, but only where Spotify still holds the value mixtape set; a crashed
session's changes are restored by the next `record`.

**Follow mode.** The public embed page lists at most 100 tracks. For a
longer playlist (or when the embed page fails), mixtape records the listed
songs, then follows the playlist in Spotify with repeat on, recording each
new song, until the playlist's first song is followed by its second: it has
looped.

**Live mode.** With no links, mixtape records whatever plays into `Singles`
(or `--into`). Songs already in that folder are skipped with "next"; songs
complete elsewhere are copied and skipped; a song joined mid-way is
restarted from the top. `--match` limits recording to matching titles and
leaves the rest alone.

<br>

## Concurrency

- The **engine goroutine** owns all session state.
- The **capture goroutine** (owned by the backend) appends to the spool and
  records the time of the last audible block (an atomic).
- One **worker goroutine** performs every library operation (encode, tag,
  copy, manifest writes) in order, so the library needs no locking and
  encoding never delays the observation of the next song.

<br>

## The library

```
~/Music/Mixtape/
  .mixtape.lock
  Singles/                      track links and live recordings
    mixtape.json                manifest (schema 1)
    Singles.m3u8                regenerated on every save
    Radon-Queen-Anne's-Lace.m4a
  Pink-Floyd-The-Dark-Side-of-the-Moon/
    01-Pink-Floyd-Speak-to-Me.m4a
  Todays-Top-Hits/
```

- Playlist and singles files are `Artist-Title.m4a`; album files are
  `NN-Artist-Title.m4a` with the album position. Order lives in the
  manifest and the `.m3u8`. Names are capped by bytes for APFS; a clash gets
  `-<id6>`.
- A folder is found by its collection's Spotify ID, so a renamed playlist
  keeps its folder.
- Each file carries freeform tags `SPOTIFY_ID`, `SPOTIFY_REQUESTED_ID` (when
  relinked) and `MIXTAPE_AUDIO_SHA256`: the SHA-256 of the AAC `mdat`
  payload, which retagging never changes. A song done in one folder is
  copied, not recorded, into another.

**Commit pipeline.** Render the ranges to a WAV in `~/.mixtape/work` →
`afconvert -f m4af -d aac -b 256000` → hash `mdat` → tag → write into the
folder (temp file, fsync, rename) → manifest (atomic) → `.m3u8`. If encoding
fails, the WAV moves to `work/failed/`, the song is `encode_failed`, and the
next run encodes it again without recording.

**Crash consistency.** A song whose file exists with matching tags but whose
manifest entry is not done (a crash between rename and manifest write) is
reconciled to recorded. Stale temp files are removed by the next writer.
`verify --fix` refuses to rewrite manifests when the library looks
unmounted (missing, or more than half of its files missing) unless `--force`.

<br>

## Locks

| Lock | Path | Held by | Purpose |
|---|---|---|---|
| Recorder | `~/Library/Caches/mixtape/recorder.lock` | `record` | one Spotify, one recorder, whatever `--home` |
| Library | `<library>/.mixtape.lock` | `record`, `verify --fix` | one writer per library |

Both are `flock`s; contention exits 6. The recorder lock file names the
holder's PID, session ID and home, so `status`, `wait` and `stop` find the
session from any home, and `stop` signals only a process that holds the
lock: never a reused PID.
