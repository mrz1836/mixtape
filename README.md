<div align="center">

# 📼&nbsp;&nbsp;Mixtape

**Record what the Spotify desktop app plays on your Mac into tagged, per-song M4A files**

<br/>

> ⚠️ **For educational and personal use only.** Mixtape is a learning project about audio
> capture and media files on macOS. We do not condone breaking any rules: respect Spotify's
> Terms of Use, artists' rights, and the copyright law where you live. Do not share
> recordings. Not affiliated with or endorsed by Spotify.

<br/>

<a href="https://github.com/mrz1836/mixtape/releases"><img src="https://img.shields.io/github/release-pre/mrz1836/mixtape?include_prereleases&style=flat-square&logo=github&color=black" alt="Release"></a>
<a href="https://golang.org/"><img src="https://img.shields.io/github/go-mod/go-version/mrz1836/mixtape?style=flat-square&logo=go&color=00ADD8" alt="Go Version"></a>
<a href="https://github.com/mrz1836/mixtape/blob/master/LICENSE"><img src="https://img.shields.io/github/license/mrz1836/mixtape?style=flat-square&color=blue&v=2" alt="License"></a>
<img src="https://img.shields.io/badge/platform-macOS-lightgrey?style=flat-square&logo=apple" alt="macOS">

</div>

<br/>

<div align="center">

### <code>Project Navigation</code>

</div>

<table align="center">
  <tr>
    <td align="center" width="33%">🚀&nbsp;<a href="#-installation"><code>Installation</code></a></td>
    <td align="center" width="33%">⚡&nbsp;<a href="#-quick-start"><code>Quick&nbsp;Start</code></a></td>
    <td align="center" width="33%">🗺️&nbsp;<a href="#%EF%B8%8F-command-palette"><code>Commands</code></a></td>
  </tr>
  <tr>
    <td align="center">🤖&nbsp;<a href="#-for-agents--scripts"><code>Agents&nbsp;&amp;&nbsp;Scripts</code></a></td>
    <td align="center">🎛️&nbsp;<a href="#%EF%B8%8F-how-it-works"><code>How&nbsp;it&nbsp;works</code></a></td>
    <td align="center">🩺&nbsp;<a href="#-troubleshooting"><code>Troubleshooting</code></a></td>
  </tr>
  <tr>
    <td align="center">📚&nbsp;<a href="#-documentation"><code>Documentation</code></a></td>
    <td align="center">⚖️&nbsp;<a href="#%EF%B8%8F-legal--responsible-use"><code>Legal</code></a></td>
    <td align="center">📝&nbsp;<a href="#-license"><code>License</code></a></td>
  </tr>
</table>

<br/>

## What it does

Point Mixtape at a Spotify link and it records the songs into your library, one
tagged, lossless M4A per song, named after the song:

```bash
mixtape record https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M
```

- **Only what is missing.** Songs already in the folder are skipped; songs
  recorded for another playlist are copied over instantly; a stopped session
  picks up where it left off. Files carry the Spotify ID and an audio hash, so
  they are recognized even after renames.
- **Lossless.** Songs are saved as Apple Lossless (ALAC) from a 24-bit
  capture, so nothing is lost after Spotify's own output; `--codec aac` saves
  256 kb/s AAC instead, at a quarter of the size. See
  [Sound quality](#sound-quality).
- **Exact cuts, complete songs.** Every song is cut out of a continuous capture
  by matching Spotify's playback position to the audio, to within a few
  milliseconds. Pauses are cut out (Spotify fades the sound out and back in
  around a pause, so the spot stays audible). A song that was not captured
  start to finish (a skip, a seek, a glitch) is never kept as done; it is
  recorded again.
- **No accounts, no API keys.** Mixtape drives the Spotify app over
  AppleScript and reads public track lists from Spotify's embed pages.
- **Two ways to hear Spotify.** A Core Audio process tap (macOS 14.2+, nothing
  to install, records Spotify alone) or BlackHole (macOS 13+).
- **Unattended.** Long playlists run on their own: the Mac stays awake, Spotify
  quitting or relaunching is ridden out, and when songs keep failing the same
  way (no sound arriving, songs not starting) the session stops early and says
  why instead of trying every song.
- **Built for people and agents.** A live status line and single-key controls
  on a terminal; NDJSON events, detached sessions and stable exit codes for
  scripts.

<br/>

## 🚀 Installation

**Requirements:** macOS 13 Ventura or later (the process tap needs macOS 14.2+)
and the Spotify desktop app.

Mixtape is a single binary for macOS (Apple Silicon and Intel). Install the
latest release into `~/.local/bin`, verified against the release's published
SHA-256 checksums before anything lands on your `PATH`:

```bash
# Install the latest mixtape release into ~/.local/bin, verified against checksums.txt
VER=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/mrz1836/mixtape/releases/latest | sed 's#.*/v##')
ARCH=$(uname -m | sed 's/x86_64/amd64/')
F="mixtape_${VER}_darwin_${ARCH}.tar.gz"; U="https://github.com/mrz1836/mixtape/releases/download/v${VER}"
mkdir -p ~/.local/bin && cd "$(mktemp -d)" && curl -fsSLO "$U/$F" \
  && WANT=$(curl -fsSL "$U/mixtape_${VER}_checksums.txt" | awk -v f="$F" '$2==f{print $1}') \
  && GOT=$(shasum -a 256 "$F" | awk '{print $1}') \
  && [ -n "$WANT" ] && [ "$WANT" = "$GOT" ] \
  && tar -xzf "$F" -C ~/.local/bin mixtape
mixtape version
```

If `mixtape` isn't found afterward, add `~/.local/bin` to your `PATH`
(`export PATH="$HOME/.local/bin:$PATH"` in `~/.zshrc`).

The binaries are not notarized by Apple. Installed as above (or with `gh` or
`mixtape update`) that does not matter; if you download the archive in a web
browser instead, macOS quarantines it, and this lets it run:
`xattr -d com.apple.quarantine ~/.local/bin/mixtape`.

<details>
<summary><strong>Build from source (contributors)</strong></summary>

Requires Go 1.26+ and the Xcode Command Line Tools (the capture backends use
cgo).

```bash
go install github.com/mrz1836/mixtape/cmd/mixtape@latest   # → $GOPATH/bin (usually ~/go/bin)

# or from a clone, with mage-x
git clone https://github.com/mrz1836/mixtape.git && cd mixtape
go install github.com/mrz1836/mage-x/cmd/magex@latest
magex build:dev  # → ~/.local/bin/mixtape
magex install    # → $GOPATH/bin/mixtape
```

A binary in `$GOPATH/bin` is yours to manage: `mixtape update` leaves it alone
(exit 5), so update it the same way you built it.

</details>

### Keep mixtape up to date

`mixtape update` (alias `upgrade`) downloads the latest release, verifies its
SHA-256 against the published checksums, and atomically replaces the running
binary: no `sudo` when it lives in `~/.local/bin`.

```bash
mixtape update            # download, verify and install the latest release
mixtape update --check    # report whether a newer version is available
mixtape update --force    # reinstall the latest even if already current
```

Interactive commands also run a cached background check and print a one-line
notice when a new version exists. It never prints in JSON mode, with
`--quiet`, or when stderr is not a terminal, and `MIXTAPE_NO_UPDATE_CHECK=1`
turns it off. A binary installed by `go install` or Homebrew is refused rather
than overwritten (exit 5).

<br/>

## ⚡ Quick Start

### 1. Set up once

```bash
mixtape setup     # capture method, permissions, library folder (guided)
mixtape doctor    # every prerequisite, with fixes (exit 3 when not ready)
```

A recording sounds exactly as Spotify plays it, so a few Spotify settings
matter:

- **Crossfade and Automix off** (required): Spotify → Settings → Playback →
  *Crossfade songs* and *Automix*. Blended songs overlap and cannot be cut;
  Mixtape stops (exit 3) when it sees either.
- **Volume at maximum:** recordings are made at Spotify's own volume slider
  (the Mac's volume does not matter). With the process tap, `--mute` does
  this for you and silences the speakers while it records.
- **Streaming quality as high as your account allows:** Settings → Audio
  quality → *Streaming quality* (*Lossless* where your plan has it).
  *Automatic* can drop the quality mid-song on a slow connection.
- **Normalize volume and the equalizer:** both change the sound that is
  recorded. Turn them off to keep each song as it was mastered.
- **Play on this Mac:** with Spotify Connect playing on another device, no
  sound reaches Mixtape.

### 2. Record

```bash
# A playlist, an album, or tracks (any mix; share links with ?si= are fine)
mixtape record https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M
mixtape record spotify:album:4LH4d3cOWNNsVw41Gqt2kv
mixtape record spotify:track:56o35W5Xr5AimZqAHjeNGh spotify:track:7gK6ZSHZVnigjKdmsXE8Hi

# See the plan first: what will be recorded, copied, or skipped
mixtape record <link> --dry-run

# Live mode: record whatever you play (songs you already have are skipped)
mixtape record
mixtape record --match "remix" --into "Remixes"
```

On a terminal: <kbd>space</kbd> pause/resume, <kbd>n</kbd> skip the song,
<kbd>q</kbd> quit (Ctrl-C twice forces it).

```
▶ Radon — Queen Anne's Lace (3:20)
✓ saved Radon — Queen Anne's Lace → Singles/Radon-Queen-Anne's-Lace.m4a
⏺ REC 1:12/2:56 │ Funkmammoth — Cloudy │ 1 of 2 · ~2m left
```

### 3. Browse and check

```bash
mixtape list               # every folder: done / pending / failed
mixtape list <link>        # each song of a link and what record would do next
mixtape verify             # files, tags and audio hashes against the manifests
```

<br/>

## 🗺️ Command palette

| Group | Command | What it does |
|---|---|---|
| Recording | `record [LINK…]` | Record tracks, albums, playlists; no links: whatever plays |
| | `status [--follow]` | The running (or last) session: progress, current song, ETA |
| | `wait [--timeout D]` | Block until the session ends; exit with its result (124: still running) |
| | `stop [--no-wait]` | Stop the running session gracefully and print the summary; with none running, put back what a crashed one left changed |
| Library | `list [LINK]` (`ls`) | Folders, or the status of each song of a link |
| | `verify [--fix]` | Check files, tags and audio hashes; repair manifests |
| Setup | `setup` | Guided onboarding |
| | `doctor` | Readiness checks with fixes |
| | `devices` | Audio devices and process-tap availability |
| | `config show\|set\|path` | Persistent settings (`~/.mixtape/config.json`) |
| Other | `update` (`upgrade`) · `version` · `completion` | |

Global flags: `-o/--output text|json|auto`, `-q/--quiet`, `-v/--verbose`,
`--no-color`, `--home`, `-L/--library`. The full reference, with every flag,
JSON shape and event, is in [docs/CLI.md](docs/CLI.md).

<br/>

## 🤖 For agents & scripts

Mixtape is designed to be driven unattended:

- **Machine output.** `-o json`, or any pipe, gives JSON on stdout and JSON
  lines on stderr, nothing else. `record` streams one NDJSON event per line:
  `session_started`, `plan`, `track_started`, `track_saved`, `track_copied`,
  `track_skipped`, `track_discarded`, `track_failed`, `warning`,
  `session_finished`.
- **Long sessions survive.** A playlist takes as long as it plays. Detach it
  and poll in short slices:

  ```bash
  mixtape record --detach <link> -o json   # {"session_id":…,"pid":…,"events":…,"log":…}
  mixtape wait --timeout 9m -o json        # exit 124 while running, then the session's own exit code
  mixtape status -o json                   # {"running":true,"session":{phase,current,counts,eta_seconds,…}}
  mixtape stop                             # graceful; the song in progress is discarded
  ```

- **Stable exit codes.**

  | Code | Meaning |
  |---|---|
  | 0 | Complete, or stopped on request |
  | 1 | General error |
  | 2 | Usage error (bad flag or link) |
  | 3 | Not ready: Spotify not running, crossfade on, capture not routed or silent, songs not starting |
  | 4 | Not found: unknown link, no session |
  | 5 | Permission: Automation or audio-capture permission, a library that cannot be written |
  | 6 | Busy: another session is recording |
  | 7 | Incomplete: finished with songs not recorded; run it again to retry |
  | 124 | `wait` timed out; the session is still running |
  | 130 | Forced quit |

- **Errors explain themselves.** Every failure is
  `{"error":{"code","message","hint","exit_code"}}` on stderr; `hint` says
  what to do.
- **It stops when nothing can work.** After three failed attempts in a row
  (each song is tried twice; `--max-failures`, or `max_failures` in the
  config; `0` never stops), the session ends with a cause: no sound reaching
  Mixtape, songs not starting in Spotify, or repeated failures.
- **Safe to retry.** `record` is idempotent: run the same command again and
  only what is missing happens. `--dry-run` previews it.
- **Sessions are on disk.** `~/.mixtape/session.json` (live state) and
  `~/.mixtape/sessions/<id>.ndjson` (every event).

<br/>

## 🎛️ How it works

Mixtape captures the Mac's audio continuously into a spool on disk while it
watches Spotify over AppleScript (one long-running `osascript`, an
observation every 150 ms). Every observation and every audio block carries a
time on the same clock, so each reported position pins the song's start to
an exact frame; the median over many observations is accurate to
milliseconds, measured at about 2–4 ms on real sessions. When a song ends, its
frames are cut out (pauses removed), encoded with `afconvert` (lossless ALAC by
default, or AAC), tagged, hashed, and written atomically into the library with
its manifest and `.m3u8`.

For a list of links Mixtape plays each song in its album or playlist context,
so the songs flow on naturally, and steps in only when Spotify plays
something else. It turns shuffle off for the session and puts your settings
back afterwards, even after a crash. While a session plays, the Mac is kept
from idle-sleeping (closing the lid still sleeps it).

The details, including the timing measurements behind every constant, are in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

### Capture methods

| | Process tap (default on macOS 14.2+) | BlackHole |
|---|---|---|
| Install | nothing | `brew install blackhole-2ch`, plus a Multi-Output Device |
| Records | Spotify only (no notification sounds) | everything the Mac plays |
| Muted recording | yes, `--mute` | no |
| Permission | *System Audio Recording* for the app that runs mixtape | *Microphone* for that app |
| Works over ssh / launchd | no | yes |

`mixtape setup` picks and tests one; `--capture tap|device` overrides it per
run, and `mixtape config set capture …` changes the default.

**Permissions.** macOS grants audio recording, microphone access and control
of Spotify to the *app* that runs mixtape (Terminal, iTerm, an IDE), never to
mixtape itself, and no program can grant itself these permissions. `mixtape
setup` handles them: it names the app macOS asks about, shows the system
prompt where macOS has one, and otherwise opens System Settings at the right
list and waits until you switch it on, then proves it with a test capture.
`mixtape doctor` reports each one without prompting.

The one that trips people up is the process tap's. Terminal apps cannot show
a prompt for it, so it must be switched on by hand: System Settings → Privacy
& Security → Screen & System Audio Recording → **System Audio Recording Only**
(the *bottom* list; the screen-recording list above it is not enough) → add
your terminal app. Without it the tap records silence, so `record` stops at
once with exit 5 and this fix.

**BlackHole routing.** In Audio MIDI Setup create a *Multi-Output Device* with
your speakers and *BlackHole 2ch* (speakers first, as the clock source), and
choose it as the sound output. `mixtape doctor` checks the routing.

### Sound quality

Mixtape records what Spotify plays: the decoded sound on its way to the
speakers, never Spotify's own file. With Spotify's volume at 100 %, *Normalize
volume* and the equalizer off, a 24-bit capture and the output at Spotify's
rate, the recording is a transparent copy of that sound.

| `codec` | What is saved | Size of a 4-minute song |
|---|---|---|
| `alac` (default) | Apple Lossless, 24-bit capture | about 30–40 MB (7–10 MB a minute) |
| `alac` with `bit_depth 16` | Apple Lossless, 16-bit | about 15 MB |
| `aac` | AAC at `aac_bitrate` (256 kb/s), 16-bit capture | about 7.5 MB |

Each setting has an `MIXTAPE_*` variable and a `record` flag of the same name
(`--codec`, `--aac-bitrate`, …); `mixtape config set codec aac` changes the
default:

| Setting | Default | Values |
|---|---|---|
| `codec` | `alac` | `alac` (lossless), `aac` |
| `aac_bitrate` | `256` | kb/s from 64 to 320, for `aac` only (below 96, Apple's encoder drops to 32 kHz) |
| `bit_depth` | `auto` | `auto` (24-bit for ALAC, 16-bit for AAC), `16`, `24` |
| `copy_codec` | `same` | `same`: copy a song found in another folder only in the codec this run records, else record it; `any`: copy it in any codec |
| `output_rate` | `keep` | `keep`, or a rate in Hz the output device offers, such as `44100` (process tap only) |

**The output rate.** Spotify plays at 44.1 kHz, and most Macs run their
speakers at 48 kHz, so Spotify resamples every song before Mixtape hears it.
`--output-rate 44100` runs the speakers at 44.1 kHz for the session and puts
them back afterwards, even after a crash (`mixtape stop` or the next
`mixtape record` does it; `mixtape doctor` shows it). A call app holding the
speakers (Teams, Zoom) can set the rate back at once: Mixtape then says so and
records at the speakers' rate. With BlackHole the setting does nothing: the
Multi-Output Device sets that rate.

**Upgrading from 0.1.** Songs you already have stay as they are (AAC). New
recordings are 24-bit ALAC. A song already in its folder is never recorded
again because the codec changed (`--redo` does that); a song found only as AAC
in another folder is recorded in ALAC rather than copied (`record --dry-run` and
`list <link>` show `other_codec`). `mixtape config set codec aac` gives the old
behavior, and `copy_codec any` keeps copying AAC files.

### The library

```
~/Music/Mixtape/
  Singles/                                  track links and live mode
    Radon-Queen-Anne's-Lace.m4a
    mixtape.json                            manifest: every song, its status and hash
    Singles.m3u8                            playlist in collection order
  Pink-Floyd-The-Dark-Side-of-the-Moon/     albums: NN-Artist-Title.m4a
    01-Pink-Floyd-Speak-to-Me.m4a
  Todays-Top-Hits/                          playlists: Artist-Title.m4a
```

Files are tagged with title, artist, album, album artist, track and disc
numbers, the release date (shown as the year), the collection name
(grouping) and the Spotify URL (comment), plus `SPOTIFY_ID` and
`MIXTAPE_AUDIO_SHA256`. Album and playlist pages do not date their songs, so
Mixtape reads each recorded song's own page for its release date (one small
request per song). Album artwork is opt-in (`--artwork`, or `mixtape config
set artwork true`).

<br/>

## 🩺 Troubleshooting

| Symptom | Fix |
|---|---|
| `spotify crossfade is on` or `crossfade appears to be on` (exit 3) | Spotify → Settings → Playback → turn off *Crossfade songs* and *Automix*. Newer Spotify versions keep crossfade where it cannot be read, so Mixtape also detects it from playback. `--allow-crossfade` records anyway (songs will overlap). |
| `… may not record system audio yet` or `the process tap is silent` (exit 5) | Run `mixtape setup`, or add your terminal app under *System Audio Recording Only* (see above; not the screen-recording list). If it is already there, quit and reopen the terminal app. Or use `--capture device`. |
| `the capture device is silent` (exit 3) | Select the Multi-Output Device (with BlackHole) as the Mac's sound output. |
| `spotify is playing, but no sound reaches mixtape` or `songs keep playing without any sound reaching mixtape` (exit 3) | Spotify is playing somewhere else: click the speaker icon at the bottom right of Spotify and choose this Mac. Also check that Spotify's own volume is not at 0. |
| `the songs keep not starting in Spotify` (exit 3) | Check that Spotify is signed in and online, and that a song plays when you click it. |
| `could not save the recording` (exit 5) | The library folder is gone or full: reconnect the drive or free some space, then run the same command again. Songs not saved stay pending. |
| `could not write the captured audio to disk` (exit 1) | Free some disk space on the drive that holds `~/.mixtape`. |
| `Liked Songs … are private to your account` (exit 2) | Mixtape cannot read your library's track lists. Add the songs to a playlist and link that, or play them in Spotify and run `mixtape record` with no link. |
| `not allowed to control Spotify` (exit 5) | System Settings → Privacy & Security → Automation → your terminal app → enable Spotify. |
| Recordings are quiet | Set Spotify's volume slider to the maximum, or record with `--mute` (process tap). |
| Songs take far more space than before | They are lossless ALAC now. `mixtape config set codec aac` for 256 kb/s AAC, or `bit_depth 16` for smaller lossless files. |
| doctor: `afconvert does not list the ALAC encoder here` | Run mixtape from a normal terminal session: a sandbox hides the encoders. A song that fails to encode keeps its WAV and is encoded on the next run. |
| `could not run … at 44100 Hz (another app set the output rate back)` | A call app (Teams, Zoom) holds the speakers at its rate. Quit it, or record without `output_rate`. |
| The speakers stayed at another rate after a crash | `mixtape stop` puts them back (so does the next `mixtape record`). |
| A long session stopped overnight | The Mac slept: keep the lid open (or use an external display) and leave it on power. |
| `another mixtape recording session is running` (exit 6) | `mixtape status`, then `mixtape stop`. |
| A song keeps failing | `mixtape list <link>` shows the reason. Songs unavailable in your country are marked `unavailable`. |
| Anything else | `mixtape doctor`; `mixtape record -v` prints timing diagnostics. |

<br/>

## Limits

- **macOS only**, with the Spotify desktop app. Playback must be on this Mac:
  Spotify Connect to another device records nothing (Mixtape stops and says
  so).
- **Real time.** A playlist takes as long as it plays. Ads (free accounts) play
  and are not recorded.
- **Track lists** come from Spotify's public, undocumented embed page. It lists
  at most 100 songs of a playlist; for longer playlists Mixtape follows the
  playlist in Spotify until it loops. If the page changes or fails, Mixtape
  falls back to the oEmbed endpoint (the name only) and follows the playlist
  the same way. Private playlists, Liked Songs and the rest of your own
  library cannot be read; podcasts and audiobooks are not supported.
- **What Spotify plays, not its file.** A recording is Spotify's decoded sound
  (see [Sound quality](#sound-quality)). On a MacBook Pro's built-in speakers
  (macOS 15) the process tap rolls the very top off: flat to 18 kHz, about
  2.5 dB down at 20 kHz. Through BlackHole the samples arrive bit for bit.
- One recording session at a time per Mac.

<br/>

## 📚 Documentation

- [docs/CLI.md](docs/CLI.md): every command, flag, JSON shape, event, setting and exit code
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): capture, timing, segmentation, the library, locks
- [.github/CONTRIBUTING.md](.github/CONTRIBUTING.md) · [.github/SECURITY.md](.github/SECURITY.md) · [.github/AGENTS.md](.github/AGENTS.md)

<details>
<summary><strong>Development</strong></summary>

Everything goes through [MAGE-X](https://github.com/mrz1836/mage-x) and
[go-pre-commit](https://github.com/mrz1836/go-pre-commit), as in the other
mrz1836 Go projects (`magex help` lists every command):

```bash
magex build:dev                              # build this checkout and install it as `mixtape` (~/.local/bin)
magex format:fix                             # gofumpt, gci and goimports
go-pre-commit run --all-files --skip lint    # whitespace, EOF, gofumpt, go mod tidy, gitleaks
magex lint                                   # golangci-lint with the repository's strict config
magex test                                   # the suite (magex test:race with the race detector)
magex test:coverage                          # coverage report
magex test:fuzz                              # the fuzz targets: every parser, the segmenter, MP4 tagging
```

Before a commit, run the four checks in order: `magex format:fix`,
`go-pre-commit run --all-files --skip lint`, `magex lint`, `magex test:race`.

Tests never touch Spotify or Core Audio: the engine runs against a simulated
Spotify and replays real timing traces (`internal/engine/testdata`), and the
suite passes on Linux too, where CI runs it. Tests that encode real audio skip
themselves when the ALAC or AAC encoder is unavailable (off macOS, or inside a
sandbox).

**Live testing** plays Spotify and opens real capture, so it runs only when
you ask for it, with Spotify playing on this Mac:

```bash
magex build:dev                              # the build under test, as `mixtape`
mixtape doctor                               # what this Mac is ready for
MIXTAPE_LIVE_CAPTURE=1 magex test            # the suite plus the live tap and device captures
MIXTAPE_LIVE_OUTPUT_RATE=1 magex test        # also switches the speakers' rate, and back
mixtape debug trace --duration 2m            # a timing trace while you play, pause and skip

# keep test recordings out of your real library and session state
export MIXTAPE_HOME=/tmp/mixtape-test/home MIXTAPE_LIBRARY=/tmp/mixtape-test/lib
mixtape record spotify:album:4LH4d3cOWNNsVw41Gqt2kv --limit 1 --mute
```

The device capture test reads `MIXTAPE_TEST_DEVICE` (default `BlackHole`); it
hears something only when the Mac's output is the Multi-Output Device.

**Benchmarks.** Re-measure after a change to the capture, spool, segmenter or
tagging paths:

```bash
magex bench:default time=2s
magex bench:save time=2s out=bench.txt
magex bench:compare old=before.txt new=after.txt
```

Latest baseline — Apple M1 Max · darwin/arm64 · Go 1.27 · `time=2s`:

| Benchmark | ns/op | B/op | allocs/op | Path covered |
|---|--:|--:|--:|---|
| `BenchmarkConvertF32/24-bit` (`internal/capture`) | 2,206 | 0 | 0 | converting one 10 ms Core Audio buffer to 24-bit PCM |
| `BenchmarkPeak/16-bit` (`internal/capture`) | 901 | 0 | 0 | loudness check of every 10 ms capture block |
| `BenchmarkPeak/24-bit` (`internal/capture`) | 962 | 0 | 0 | the same, at 24 bits |
| `BenchmarkAppend` (`internal/spool`) | 769 | 35 | 0 | spooling one 10 ms block (48 kHz stereo) |
| `BenchmarkFrameAt` (`internal/spool`) | 81 | 0 | 0 | time → frame in a 10-minute spool |
| `BenchmarkReadRange` (`internal/spool`) | 4,109,575 | 262,911 | 10 | reading a 3-minute song back out |
| `BenchmarkSegmentize` (`internal/engine`) | 241,013 | 523,980 | 35 | cutting a 10-minute song with a pause |
| `BenchmarkTag` (`internal/mp4tag`) | 669,096 | 8,491,858 | 73 | retagging a 7.5 MB song with a cover, in memory |
| `BenchmarkTagFile` (`internal/mp4tag`) | 5,300,652 | 926,078 | 83 | retagging a 40 MB ALAC song from disk, streamed |
| `BenchmarkAudioSHA256` (`internal/mp4tag`) | 3,191,493 | 464 | 5 | hashing a 7.5 MB song's audio |

> Local-machine numbers, not targets: compare runs on the same hardware.
> Last measured: 2026-11-03

**Releasing.** Run `magex test:race` on a Mac first (the encoder tests skip
on Linux CI, and the darwin-only code is not even compiled there), then `magex version:bump bump=patch branch=master push` tags and
pushes the release. On the tag, GoFortress runs lint, tests and security scans,
then builds both macOS binaries on a macOS runner (`RELEASE_RUNNER` in
`.github/env/90-project.env`) and publishes them with GoReleaser.
From a Mac, `GITHUB_TOKEN=… goreleaser release --clean` does the same locally.

</details>

<details>
<summary><strong>Shell completion</strong></summary>

```bash
# zsh (the macOS default): add to ~/.zshrc
eval "$(mixtape completion zsh)"

# bash 4+ (Homebrew's bash; macOS ships 3.2): add to ~/.bashrc
eval "$(mixtape completion bash)"

# fish
mixtape completion fish > ~/.config/fish/completions/mixtape.fish
```

</details>

<details>
<summary><strong>Uninstall</strong></summary>

```bash
rm ~/.local/bin/mixtape          # the binary (or wherever you installed it)
rm -rf ~/.mixtape                # settings, session logs, and audio buffers
rm -rf ~/Library/Caches/mixtape  # the recorder lock
rm -rf ~/Library/"Application Support"/mixtape  # the update-check cache
```

Your recordings in `~/Music/Mixtape` are yours: delete that folder only if you
no longer want them.

</details>

<br/>

## ⚖️ Legal & responsible use

Mixtape is an educational project about audio capture, timing and media
files on macOS. It records what your own Mac plays, for your personal
archive.

- **Respect the rules that apply to you.** Spotify's Terms of Use restrict
  copying content from its service. Recording may not be allowed for your
  account or in your country, and copyright law differs around the world. You
  alone are responsible for how you use Mixtape.
- **Do not distribute recordings.** Music belongs to its artists and rights
  holders.
- **Not affiliated with Spotify.** "Spotify" is a trademark of Spotify AB,
  used here only to describe compatibility. Mixtape uses no Spotify API keys
  and no account credentials; it controls the desktop app through macOS's
  public AppleScript interface and reads public web pages.
- **No warranty.** Mixtape is provided as is, under the [MIT license](LICENSE).

<br/>

## 🤖 AI Usage & Assistant Guidelines

Read the [AI Usage & Assistant Guidelines](.github/CLAUDE.md) for details on
how AI is used in this project and how to interact with AI assistants.

<br/>

## 👥 Maintainers

| [<img src="https://github.com/mrz1836.png" height="50" alt="MrZ" />](https://github.com/mrz1836) |
|:------------------------------------------------------------------------------------------------:|
|                                [MrZ](https://github.com/mrz1836)                                 |

<br/>

## 🤝 Contributing

View the [contributing guidelines](.github/CONTRIBUTING.md) and please follow
the [code of conduct](.github/CODE_OF_CONDUCT.md).

<br/>

## 📝 License

[![License](https://img.shields.io/github/license/mrz1836/mixtape.svg?style=flat&v=1)](LICENSE)

This project is licensed under the terms of the [`LICENSE`](LICENSE) file at
the repo root.
