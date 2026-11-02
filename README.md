<div align="center">

# 📼&nbsp;&nbsp;Mixtape

**Record Spotify tracks, albums and playlists on macOS into tagged, per-song M4A files**

<br/>

> ⚠️ **For educational use only.** Mixtape is a personal-archiving and learning project.
> We do not condone breaking any rules: respect Spotify's Terms of Use, artists' rights,
> and the copyright law where you live. Not affiliated with or endorsed by Spotify.

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
tagged M4A per song, named after the song:

```bash
mixtape record https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M
```

- **Only what is missing.** Songs already in the folder are skipped; songs
  recorded for another playlist are copied over instantly; a stopped session
  picks up where it left off. Files carry the Spotify ID and an audio hash, so
  they are recognized even after renames.
- **Exact cuts, complete songs.** Every song is cut out of a continuous capture
  by matching Spotify's playback position to the audio, to within a few
  milliseconds. Pauses are cut out. A song that was not captured start to
  finish (a skip, a seek, a glitch) is never kept as done; it is recorded
  again.
- **No accounts, no API keys.** Mixtape drives the Spotify app over
  AppleScript and reads public track lists from Spotify's embed pages.
- **Two ways to hear Spotify.** A Core Audio process tap (macOS 14.2+, nothing
  to install, records Spotify alone) or BlackHole (any macOS).
- **Built for people and agents.** A live status line and single-key controls
  on a terminal; NDJSON events, detached sessions and stable exit codes for
  scripts.

<br/>

## 🚀 Installation

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

> **While the repository is private** the release downloads need GitHub
> authorization: use `gh release download --repo mrz1836/mixtape --pattern
> "mixtape_*_darwin_$(uname -m | sed 's/x86_64/amd64/').tar.gz"` with a
> signed-in [`gh`](https://cli.github.com), and give `mixtape update` a token
> through `MIXTAPE_GITHUB_TOKEN` (or a signed-in `gh`).

<details>
<summary><strong>Build from source (contributors)</strong></summary>

Requires Go 1.26+ and the Xcode Command Line Tools (the capture backends use
cgo).

```bash
git clone https://github.com/mrz1836/mixtape.git && cd mixtape
go install github.com/mrz1836/mage-x/cmd/magex@latest
magex build      # → ./cmd/mixtape/mixtape
magex install    # → ~/.local/bin/mixtape
```

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

Two Spotify settings matter:

- **Crossfade off:** Spotify → Settings → Playback → *Crossfade songs*.
  Crossfaded songs overlap and cannot be cut; Mixtape stops (exit 3) when it
  sees it.
- **Volume at maximum:** recordings are made at Spotify's own volume slider
  (the Mac's volume does not matter).

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
| | `stop [--no-wait]` | Stop the running session gracefully and print the summary |
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
  | 3 | Not ready: Spotify not running, crossfade on, capture not routed or silent |
  | 4 | Not found: unknown link, no session |
  | 5 | Permission: Automation or audio-capture permission, unwritable folder |
  | 6 | Busy: another session is recording |
  | 7 | Incomplete: finished with songs not recorded; run it again to retry |
  | 124 | `wait` timed out; the session is still running |
  | 130 | Forced quit |

- **Errors explain themselves.** Every failure is
  `{"error":{"code","message","hint","exit_code"}}` on stderr; `hint` says
  what to do.
- **Safe to retry.** `record` is idempotent: run the same command again and
  only what is missing happens. `--dry-run` previews it.
- **Sessions are on disk.** `~/.mixtape/session.json` (live state) and
  `~/.mixtape/sessions/<id>.ndjson` (every event).

<br/>

## 🎛️ How it works

Mixtape captures the Mac's audio continuously into a spool on disk while it
watches Spotify over AppleScript (one long-running `osascript`, about eight
observations a second). Every observation and every audio block carries a
time on the same clock, so each reported position pins the song's start to
an exact frame; the median over many observations is accurate to
milliseconds, measured at about 2–4 ms on real sessions. When a song ends, its
frames are cut out (pauses removed), encoded with `afconvert` to 256 kb/s AAC,
tagged, hashed, and written atomically into the library with its manifest and
`.m3u8`.

For a list of links Mixtape plays each song in its album or playlist context,
so the songs flow on naturally, and steps in only when Spotify plays
something else. It turns shuffle off for the session and puts your settings
back afterwards, even after a crash.

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
numbers, the collection name (grouping) and the Spotify URL (comment), plus
`SPOTIFY_ID` and `MIXTAPE_AUDIO_SHA256`. Album artwork is opt-in
(`--artwork`, or `mixtape config set artwork true`).

<br/>

## 🩺 Troubleshooting

| Symptom | Fix |
|---|---|
| `crossfade appears to be on` (exit 3) | Spotify → Settings → Playback → turn off *Crossfade songs*. Spotify keeps this setting where it cannot be read directly, so Mixtape detects it from playback. |
| `… may not record system audio yet` or `the process tap is silent` (exit 5) | Run `mixtape setup`, or add your terminal app under *System Audio Recording Only* (see above; not the screen-recording list). If it is already there, quit and reopen the terminal app. Or use `--capture device`. |
| `the capture device is silent` (exit 3) | Select the Multi-Output Device (with BlackHole) as the Mac's sound output. |
| `not allowed to control Spotify` (exit 5) | System Settings → Privacy & Security → Automation → your terminal app → enable Spotify. |
| Recordings are quiet | Set Spotify's volume slider to the maximum. |
| `another mixtape recording session is running` (exit 6) | `mixtape status`, then `mixtape stop`. |
| A song keeps failing | `mixtape list <link>` shows the reason. Songs unavailable in your country are marked `unavailable`. |
| Anything else | `mixtape doctor`; `mixtape record -v` prints timing diagnostics. |

<br/>

## Limits

- **macOS only**, with the Spotify desktop app. Playback must be on this Mac:
  Spotify Connect to another device records nothing (and Mixtape notices).
- **Real time.** A playlist takes as long as it plays. Ads (free accounts) play
  and are not recorded.
- **Track lists** come from Spotify's public, undocumented embed page. It lists
  at most 100 songs of a playlist; for longer playlists Mixtape follows the
  playlist in Spotify until it loops. If the page changes or fails, Mixtape
  falls back to the oEmbed endpoint (the name only) and follows the playlist
  the same way. Private playlists cannot be read.
- One recording session at a time per Mac.

<br/>

## 📚 Documentation

- [docs/CLI.md](docs/CLI.md): every command, flag, JSON shape, event, setting and exit code
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): capture, timing, segmentation, the library, locks
- [.github/CONTRIBUTING.md](.github/CONTRIBUTING.md) · [.github/SECURITY.md](.github/SECURITY.md) · [.github/AGENTS.md](.github/AGENTS.md)

<details>
<summary><strong>Development</strong></summary>

```bash
magex format:fix   # gofumpt + goimports
magex lint         # golangci-lint with the repository's strict config
magex test:race    # unit tests with the race detector
magex build        # build ./cmd/mixtape/mixtape
```

Tests never touch Spotify or Core Audio: the engine runs against a simulated
Spotify and replays real timing traces (`internal/engine/testdata`). Tests that
encode real audio skip themselves when the AAC encoder is unavailable (inside a
sandbox). Live capture checks run only with both `-tags captureintegration`
and `MIXTAPE_LIVE_CAPTURE=1`, with Spotify playing.

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
