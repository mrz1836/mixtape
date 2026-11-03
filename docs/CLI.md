# Mixtape CLI reference

Every command, flag, output shape, event, setting and exit code. For the
why and how, see [ARCHITECTURE.md](ARCHITECTURE.md).

```
Recording  record [LINK|URI...]   Record tracks, albums, playlists; no links = live mode
           status [--follow]      The running (or last) session
           wait [--timeout D]     Block until the session ends; exit with its result
           stop [--no-wait]       Stop the running session gracefully
Library    list [LINK]  (ls)      Library folders, or each song of one link
           verify [--fix]         Check files, tags and audio hashes
Setup      setup                  Guided first-time setup
           doctor                 Readiness checks (exit 3 when not ready)
           devices                Audio devices and process-tap availability
           config show|set|path   Persistent settings
Other      update (upgrade)       Self-update from GitHub releases
           version                Build version
           completion SHELL       Shell completion script
```

<br>

## Global flags

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-o, --output` | `MIXTAPE_OUTPUT` | `auto` | `text`, `json`, or `auto`: text on a terminal, JSON when piped |
| `-q, --quiet` | | | Print only results and errors |
| `-v, --verbose` | | | Diagnostics on stderr; with `-o json`, extra `boundary` events |
| `--no-color` | `NO_COLOR` | | No ANSI color (also off when not a terminal, or `TERM=dumb`) |
| `--home` | `MIXTAPE_HOME` | `~/.mixtape` | State: config, session, event logs, spool, work files |
| `-L, --library` | `MIXTAPE_LIBRARY` | `~/Music/Mixtape` | The library folder |

`--verbose` and `--quiet` cannot be combined. There is no root `-c` or `-f`:
`update` owns them.

**Output contract.** In JSON mode stdout carries only JSON: one document per
command, or an NDJSON event stream for `record` and `status --follow`. JSON
is indented on a terminal and compact when piped; NDJSON is always compact.
stderr carries only JSON lines too: `{"warning":{"message":…}}` and, on
failure, one error document. In text mode errors read
`mixtape: <command>: <message>` followed by `  fix: <hint>` when there is one.

```json
{"error":{"code":"not_ready","message":"spotify crossfade is on, so songs overlap and cannot be cut cleanly (crossfade is set to 6.0s)","hint":"in Spotify → Settings → Playback turn off \"Crossfade songs\" and \"Automix\", or pass --allow-crossfade","exit_code":3}}
```

<br>

## Exit codes

| Code | `error.code` | Meaning |
|---|---|---|
| 0 | | Success: complete, or stopped on request |
| 1 | `error` | General error (also: `verify` found problems) |
| 2 | `usage` | Wrong command line: unknown flag or command, bad link, invalid setting |
| 3 | `not_ready` | A prerequisite is missing: Spotify not running, crossfade on, capture not routed or silent, no sound reaching mixtape, songs not starting in Spotify |
| 4 | `not_found` | A link Spotify does not know, or no session |
| 5 | `permission` | macOS refused: Automation or audio-capture permission, a library that cannot be written (also mid-session), update refused |
| 6 | `busy` | Another recording session (or library writer) holds the lock |
| 7 | `incomplete` | The session finished with songs not recorded (failed, unavailable, idle timeout, or songs failing in a row for mixed reasons) |
| 124 | `timeout` | `wait --timeout` expired while the session is still running |
| 130 | `interrupted` | Forced quit (a second Ctrl-C) |

<br>

## record

```
mixtape record [LINK|URI...] [flags]
```

Records the songs of each link that are not already in the library, one
tagged M4A per song: lossless ALAC from a 24-bit capture by default, or AAC
(`--codec aac`). Links are `open.spotify.com` URLs (share links with
`?si=` are fine, `/intl-xx/` paths too) or `spotify:track|album|playlist:`
URIs. Track links go to `Singles` (or `--into`); each album and playlist gets
its own folder. With no links, mixtape records whatever Spotify plays (live
mode). Liked Songs and the rest of your own library (`spotify:collection:…`),
artists, podcasts and audiobooks are not supported (exit 2, with what to do
instead).

For each song the plan decides one action:

| Action | When |
|---|---|
| `skip` | Already complete in this folder, in whatever codec (a codec change never redoes a song; `--redo` does) |
| `copy` | Complete elsewhere in the library (or recorded earlier in this session) in the codec this session records (`copy_codec same`), or in any codec (`copy_codec any`): copied and retagged, no playback |
| `encode` | Recorded before but encoding failed: the kept WAV is encoded again |
| `record` | Everything else. `hint: unavailable` marks songs the catalog says cannot play here; they are still tried. `reason: other_codec` marks a song the library holds elsewhere only in another codec |

The codec of a song elsewhere is read from its file, not from a manifest, so
the plan, a song decided during a live session, `--dry-run` and `list LINK`
always agree.

| Flag | Meaning |
|---|---|
| `--dry-run` | Print the plan and change nothing |
| `--detach` | Run in the background; print `{session_id, pid, events, log}` and return |
| `--redo` | Record again songs that are already done |
| `--limit N` | Stop after recording N songs |
| `--match TEXT` | Live mode: record only songs whose title contains TEXT; leave the rest alone |
| `--into NAME` | Folder for track links and live recordings (default `Singles`) |
| `--keep-partial` | Keep songs not captured start to finish (flagged `partial`; replaced next run) |
| `--keep-wav` | Keep each song's WAV in the work folder |
| `--artwork` | Embed album artwork (one image download per song) |
| `--capture auto\|tap\|device` | Capture method |
| `--device NAME` | Capture device (substring) for `--capture device` |
| `--mute` | Tap only: silence Spotify on the speakers while recording (volume raised to 100 meanwhile) |
| `--idle-timeout D` | End after D with nothing to record (default 10m; 0 = never) |
| `--max-failures N` | End after N failed attempts in a row; a song is tried twice (default 3; 0 = never) |
| `--allow-crossfade` | Record even though crossfade or automix is on (songs will overlap) |
| `--codec alac\|aac` | Audio format of new songs (config `codec`). Not to be confused with `-o/--output`, which is how mixtape reports |
| `--aac-bitrate KBPS` | AAC bit rate, 64 to 320 (config `aac_bitrate`) |
| `--bit-depth auto\|16\|24` | Capture depth: `auto` is 24 for ALAC, 16 for AAC (config `bit_depth`) |
| `--copy-codec same\|any` | Which songs elsewhere are copied (config `copy_codec`) |
| `--output-rate keep\|HZ` | Tap only: run the default output at HZ while recording, then put it back (config `output_rate`) |

**Before anything plays** mixtape checks that Spotify is installed and
running, crossfade and automix are off, afconvert exists, no other session
is recording, Spotify answers (Automation permission), and macOS lets the
app that runs mixtape capture audio (exit 5 at once otherwise: the capture
would be silent). When every song is already done, nothing plays and none of
that is needed.

**During the session** shuffle is turned off (and repeat on, to follow a
long playlist), and restored at the end. With `output_rate` the default output
runs at that rate (process tap only): what to undo is saved before anything
changes, the original rate comes back at the end (only if the device still
runs at the rate set: a change made since wins), and after a crash the next
`record` or `mixtape stop` puts it back. A device that does not offer the
rate, or an app that sets it straight back (a call app holding the speakers),
is a warning, and the session records at the rate the device has. Spotify is paused when the queue is
done, and the Mac is kept from idle-sleeping until then (closing the lid
still sleeps it). Songs that were not captured start to finish are discarded
and retried once. A false start (Spotify jumping elsewhere before the song
really began) is requested again without counting as an attempt.

**When nothing can work** the session stops early instead of trying every
song: after `--max-failures` failed attempts in a row, it ends with what
they have in common: `songs keep playing without any sound reaching
mixtape` (Spotify plays on another device or is muted; exit 3), `the songs
keep not starting in Spotify` (signed out or offline; exit 3), or `the
songs keep failing` (mixed causes; exit 7). A library that cannot take a
file (an unplugged drive, a full disk) ends the session at once (exit 5), as
does audio that cannot be buffered to disk (exit 1). Songs not saved stay
pending for the next run.

**Keys and signals.** On a terminal: `space` pause/resume, `n` skip the song
(not retried), `q` quit; Ctrl-C quits after the current song is settled, a
second Ctrl-C forces it. SIGTERM (what `mixtape stop` sends) is a graceful
stop; SIGHUP and SIGPIPE are ignored.

### Dry run (`--dry-run -o json`)

```json
{
  "mode": "queue",
  "library": "/Users/you/Music/Mixtape",
  "items": [
    {"action": "record", "id": "56o35W5Xr5AimZqAHjeNGh", "title": "Queen Anne's Lace", "artist": "Radon",
     "collection": "Singles", "position": 1, "duration_ms": 200293},
    {"action": "copy", "id": "7gK6ZSHZVnigjKdmsXE8Hi", "title": "Cloudy", "artist": "Funkmammoth",
     "collection": "Road Trip", "position": 2, "duration_ms": 176170, "from": "Singles/Funkmammoth-Cloudy.m4a"},
    {"action": "record", "id": "4cOdK2wGLETKBW3PvgPWqT", "title": "Never Gonna Give You Up", "artist": "Rick Astley",
     "collection": "Road Trip", "position": 3, "duration_ms": 213573, "reason": "other_codec"}
  ],
  "counts": {"record": 2, "copy": 1}
}
```

`reason` appears only when it applies: `other_codec` means the song is in the
library elsewhere, but only in another codec than this session records, and
`copy_codec` is `same`.

`mode` is `queue`, `follow` (a playlist past the embed page's 100-track
list, followed in Spotify), or `live`.

### Events (`-o json`)

One compact JSON object per line on stdout, also appended to
`~/.mixtape/sessions/<session_id>.ndjson`. Every event has `type`, `time`
(RFC 3339) and `session_id`; other fields appear when they apply.

| `type` | Fields | Meaning |
|---|---|---|
| `session_started` | `mode`, `capture`, `library`, `pid`, `codec`, `bit_depth`, `sample_rate` | The session began. `codec` is what it encodes to; `bit_depth` and `sample_rate` describe the capture, and are absent when nothing plays |
| `plan` | `items[]` (as in the dry run) | What will happen |
| `track_started` | `track` | A song began recording from the top; sent once Spotify has played its first 2 seconds, so a false start (Spotify resuming elsewhere first) sends nothing |
| `track_saved` | `track` with `file`, `recorded_ms`, `audio_sha256`, `codec`, `partial` | A song is in the library |
| `track_copied` | `track` with `file`, `copied_from`, `codec` | A song was copied from another folder |
| `track_skipped` | `track`, `reason` | Already done, or skipped by you |
| `track_discarded` | `track`, `reason` | A take was incomplete; it is retried once |
| `track_failed` | `track`, `reason` | Given up for this session (marked in the manifest) |
| `warning` | `message` | Something worth knowing; the session continues |
| `boundary` | `track`, `offset_seconds`, `drift_seconds`, `runs`, `reason` | `--verbose` only: measured timing of a song (`reason` when it was incomplete) |
| `session_finished` | `result`, `counts`, `exit_code`, `message` | The end: `result` is `complete`, `incomplete` or `stopped` |

`track` is `{id, played_id?, title, artist, collection, position,
duration_ms, file?, recorded_ms?, audio_sha256?, copied_from?, attempt?,
partial?}`. `id` is the song's ID in the collection; `played_id` the ID
Spotify actually played when it relinked the song.

`counts` is `{total, recorded, copied, skipped, failed, discarded,
remaining}`.

```json
{"type":"track_saved","time":"2026-11-02T15:05:31Z","session_id":"20261102-150157-9de70e","track":{"id":"56o35W5Xr5AimZqAHjeNGh","title":"Queen Anne's Lace","artist":"Radon","collection":"Singles","position":1,"duration_ms":200293,"file":"Singles/Radon-Queen-Anne's-Lace.m4a","recorded_ms":200293,"audio_sha256":"…"}}
```

### Detached sessions

```bash
mixtape record --detach <link>          # {"session_id":…,"pid":…,"events":…,"log":…}
mixtape wait --timeout 9m               # 124 while running; then the session's exit code
mixtape status -o json                  # progress at any time
mixtape stop                            # graceful; prints the summary
```

The background process runs in its own session (no SIGHUP from the
terminal), with a human-readable log at `log` and the event stream at
`events`.

<br>

## status

```
mixtape status [--follow]
```

Shows the running session, or the last one. `-o json` prints
`{"running": bool, "stale": true, "session": {...}}` (`stale` only when
true), where `session` is `session.json`:

```json
{
  "schema": 1,
  "session_id": "20261102-150157-9de70e",
  "pid": 33654,
  "phase": "recording",
  "mode": "queue",
  "started_at": "2026-11-02T15:01:57Z",
  "updated_at": "2026-11-02T15:02:05Z",
  "library": "/Users/you/Music/Mixtape",
  "capture": "device (BlackHole 2ch, 24-bit 48 kHz)",
  "targets": ["spotify:track:56o35W5Xr5AimZqAHjeNGh"],
  "current": {"track_id": "56o35W5Xr5AimZqAHjeNGh", "title": "Queen Anne's Lace", "artist": "Radon",
              "collection": "Singles", "position_seconds": 5.1, "duration_seconds": 200.293, "recording": true},
  "counts": {"total": 2, "recorded": 0, "copied": 0, "skipped": 1, "failed": 0, "discarded": 0, "remaining": 1},
  "eta_seconds": 195,
  "events": "/Users/you/.mixtape/sessions/20261102-150157-9de70e.ndjson"
}
```

`phase` is `starting`, `recording`, `paused`, `stopping` or `finished`; a
finished session adds `result`, `exit_code`, `finished_at` and possibly
`error` (also when it failed or was stopped while still setting up). A
`spotify` object records the Spotify settings the session changed and
whether they were restored. `stale: true` means the session stopped without finishing (killed
or crashed); the next `record` resumes it. `--follow` streams the event log
until the session ends (it gives up with an error if the recorder lock stays
unreadable for 30 s). No session at all exits 4.

## wait

```
mixtape wait [--timeout D]
```

Blocks until the running session ends and exits with the session's exit
code; when none is running, returns the last session's result at once (a
crashed session exits 1). `--timeout` exits 124 while the session keeps
running; agents should wait in slices shorter than their own tool timeout.

## stop

```
mixtape stop [--no-wait]
```

Sends SIGTERM to the process that holds the recorder lock (never to a bare
PID), waits for it to exit (up to 2 minutes), and prints the summary (the
`status` JSON). The song in progress is discarded, Spotify is paused and its
settings restored. `--no-wait` returns once the signal is sent, with
`{"stopping": true, "session_id", "pid"}`. A session that ended just before
the signal is reported as it finished. When the recorder lock cannot be read
until the 2 minutes are up, `stop` reports that error rather than success.

With no session running, `stop` takes the recorder lock and puts back what an
interrupted session left changed: the output device's rate (`output_rate`) and
Spotify's shuffle, repeat and volume, where they still hold what that session
set. It prints `{"stopping": false, "restored": ["output MacBook Pro Speakers
back to 48000 Hz", "Spotify shuffle"]}` and exits 0. With nothing to put back
either, it exits 4.

<br>

## list (ls)

```
mixtape list [LINK]
```

Without arguments: every folder with its kind and how many songs are done,
pending and failed (`{"library", "collections": [{folder, kind, name,
spotify_id, url, songs, done, pending, failed, updated_at}]}`).

With a link: each song with its status in the library (`recorded`, `copied`,
`partial`, `pending`, `failed`, `unavailable`, `encode_failed`, or `new`),
the `codec` of its file in that folder, and what the next `record` would do
(`next_action`: `skip`, `copy`, `encode`, `record`, with `reason:
other_codec` as in the plan). `{"library", "collection", "name", "tracks":
[{position, id, title, artist, status, next_action, reason, file, codec,
fail_reason}]}`.

## verify

```
mixtape verify [--fix [--force]]
```

Checks every done song: the file exists, its tags name the right track, its
audio hash matches the manifest. Finds files that moved (by hash) and files
no manifest mentions (orphans). Exits 1 when problems remain; orphans alone
are reported but do not fail it.

`--fix` re-points files moved within their folder, sets missing or damaged
songs back to pending (the next `record` records them), refreshes stale
hashes, and regenerates every `.m3u8`. It refuses when the library folder is
missing or more than half of its songs are missing (an unmounted drive);
`--force` overrides. Output: `{collections, files, ok, problems[], fixed[]}`
with each problem `{collection, file, track_id, kind, detail}`, kinds
`missing`, `moved`, `hash_mismatch`, `tag_mismatch`, `unreadable`, `orphan`,
`status_mismatch`.

<br>

## setup

```
mixtape setup [--yes]
```

Interactive onboarding: Spotify and its Automation prompt, the capture method
(process tap recommended on macOS 14.2+, else BlackHole, which setup can
install with Homebrew and route via Audio MIDI Setup), a short test capture
while Spotify plays, crossfade, and the library folder. Only a verified
method is saved. Without a terminal (or with `-o json`) it prints the
`doctor` report and the manual steps, and exits 3 when not ready.

**Permissions.** macOS grants them to the app that runs mixtape, never to
mixtape, and no program can grant itself one. For each permission the chosen
method needs (Automation for Spotify; *System Audio Recording Only* for the
tap; Microphone for a capture device) setup checks without prompting, shows
the system prompt where macOS has one, and otherwise names the app, opens
System Settings at the right list, and waits up to 3 minutes for the switch.
Terminal apps cannot prompt for system audio recording, so the tap's
permission is always switched on by hand.

## doctor

```
mixtape doctor
```

`{"ready": bool, "capture": "tap"|"device", "checks": [{id, name, status,
detail, fix}]}` with `status` `ok`, `warn`, `fail` or `skip`. Check IDs:
`macos`, `config`, `spotify_installed`, `spotify_running`, `automation`,
`crossfade`, `volume`, `capture`, `tap`, `system_audio_recording` (tap),
`device`, `routing`, `microphone` (device), `output`, `afconvert`,
`encoder`, `library`, `session`. Exits 3 when any check fails; warnings do
not block. `output` names the default output device and its rate, and warns
when `output_rate` cannot apply (not the tap, or a rate the device does not
offer). `encoder` warns when afconvert does not list the encoder for `codec`
(a sandbox hides them). `session` also warns about a rate or Spotify settings
a session left changed, whatever way it ended (`mixtape stop` puts them
back). Doctor never
shows a permission prompt: an undecided Automation permission is a warning.
When macOS will not list the running apps, `spotify_running` is a warning
("could not check Spotify") and Spotify is asked directly, which never
launches it.

## devices

`{"tap": {"supported": bool, "reason": "…"}, "default_output": "…",
"devices": [{name, input, output, default_input, default_output}]}`. `reason`
says why the tap is unsupported; it is absent when the tap is supported.

## config

```
mixtape config show            # every setting, its value and source
mixtape config set KEY VALUE   # validate and save; VALUE "" restores the default
mixtape config path            # the file's path
```

Settings resolve from, highest first: flag, environment, config file,
default. The file is JSON (`~/.mixtape/config.json`); keys are lowercase, and
unknown keys (including a known one in other case) are kept and reported as
warnings.

| Key | Env | Default | Meaning |
|---|---|---|---|
| `library` | `MIXTAPE_LIBRARY` | `~/Music/Mixtape` | Library folder |
| `capture` | `MIXTAPE_CAPTURE` | `auto` | `auto`, `tap` or `device` |
| `device` | `MIXTAPE_DEVICE` | `BlackHole` | Capture device name (substring) |
| `artwork` | `MIXTAPE_ARTWORK` | `false` | Embed album artwork |
| `idle_timeout` | `MIXTAPE_IDLE_TIMEOUT` | `10m0s` | End a session after this long with nothing to record |
| `max_failures` | `MIXTAPE_MAX_FAILURES` | `3` | End a session after this many failed attempts in a row; a song is tried twice (0 = never) |
| `codec` | `MIXTAPE_CODEC` | `alac` | Format of new recordings: `alac` (lossless) or `aac` |
| `aac_bitrate` | `MIXTAPE_AAC_BITRATE` | `256` | AAC bit rate in kb/s, 64 to 320 (`codec aac` only; below 96 Apple's encoder drops to 32 kHz) |
| `bit_depth` | `MIXTAPE_BIT_DEPTH` | `auto` | Capture depth: `auto` (24-bit for ALAC, 16-bit for AAC), `16` or `24` |
| `copy_codec` | `MIXTAPE_COPY_CODEC` | `same` | `same`: copy a song found elsewhere only in `codec`, else record it; `any`: copy it in any codec |
| `output_rate` | `MIXTAPE_OUTPUT_RATE` | `keep` | `keep`, or a rate in Hz to run the default output at while the process tap records (such as `44100`) |

`bit_depth` and `output_rate` take a word or a number: the file may hold
either `"24"` or `24`, and `config set` writes numbers as numbers.

Other environment variables: `MIXTAPE_HOME`, `MIXTAPE_OUTPUT`, `NO_COLOR`,
`MIXTAPE_GITHUB_TOKEN` (a GitHub token for `update`, which otherwise uses a
signed-in `gh` or no authentication), `MIXTAPE_NO_UPDATE_CHECK=1` (no
passive update notice; `NO_UPDATE_CHECK=1` and `CI=true` turn it off too),
and `MIXTAPE_UPDATE_CHECK_INTERVAL` (how often the passive check runs, e.g.
`24h`).

<br>

## update (upgrade)

```
mixtape update [--check] [--force] [--verbose]
```

Downloads the latest release for this Mac, verifies its SHA-256 against the
release's checksums file, and atomically replaces the running binary. A
binary owned by `go install` or Homebrew is refused (exit 5) with the right
upgrade command. A passive notice of new versions appears after commands
run in an interactive text session, never in JSON mode, with `--quiet`, or
when stderr is not a terminal. `update` prints text in every output mode.

## version, completion

`mixtape version` (or `mixtape --version`) prints `{"version","commit","date"}`
(text on a terminal). A binary built with `go install …@vX.Y.Z` reports that
version; other source builds report `dev`.
`mixtape completion bash|zsh|fish|powershell` prints a completion script; the
README shows how to load it.

<br>

## Files

| Path | What |
|---|---|
| `~/.mixtape/config.json` | Settings |
| `~/.mixtape/session.json` | The running or last session |
| `~/.mixtape/sessions/<id>.ndjson` | Each session's events (`<id>.log` for detached ones) |
| `~/.mixtape/spool/` | Captured audio of the running session (deleted at the end) |
| `~/.mixtape/work/` | Encoding scratch; `failed/` keeps WAVs whose encode (or tagging) failed, for the next run |
| `~/Library/Caches/mixtape/recorder.lock` | The recorder lock |
| `~/Library/Caches/mixtape/output-restore.json` | An output rate a session switched and has not put back yet (per user, whatever `--home`) |
| `~/Library/Application Support/mixtape/` | The passive update check's cache |
| `<library>/<Folder>/mixtape.json` | A folder's manifest |
| `<library>/<Folder>/<Folder>.m3u8` | The folder's playlist, in collection order |
| `<library>/.mixtape.lock` | The library writer lock |

### Manifest (`mixtape.json`, schema 1)

```json
{
  "schema": 1,
  "kind": "playlist",
  "spotify_id": "37i9dQZF1DXcBWIGoYBM5M",
  "name": "Today's Top Hits",
  "owner": "Spotify",
  "url": "https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M",
  "updated_at": "2026-11-02T15:05:31Z",
  "tracks": [
    {"position": 1, "requested_id": "…", "played_id": "…", "title": "…", "artist": "…", "album": "…",
     "album_artist": "…", "duration_ms": 213573, "status": "recorded", "file": "Artist-Title.m4a",
     "recorded_ms": 213573, "audio_sha256": "…", "codec": "alac", "recorded_at": "…", "track_number": 1,
     "disc_number": 1, "release_date": "1987-07-27"}
  ]
}
```

`kind` is `playlist`, `album`, `singles` or `folder`. Track `status` is
`pending`, `recorded`, `copied`, `unavailable`, `failed`, `encode_failed` or
`removed` (left the collection; the file is kept), with `fail_reason`,
`copied_from`, `partial` and `pending_wav` when they apply. `codec` (`alac` or
`aac`) caches the codec of the file; songs stored before mixtape recorded ALAC
have none, and the file is what counts.
