# CLAUDE.md — mixtape agent guide

> **Read first:** [AGENTS.md](AGENTS.md) is the root authority for coding,
> commit, PR, release, and CI rules. [tech-conventions/](tech-conventions/)
> has the deep guides. If anything below conflicts with those, **they win.**

<br>

## What this project is

`mixtape` is a macOS CLI that records Spotify tracks, albums and playlists into
tagged, per-song M4A files. It captures audio (a Core Audio process tap, or a
capture device such as BlackHole), follows Spotify over AppleScript, cuts each
song out of a spooled capture by matching Spotify's position to the audio, and
stores it in a library with manifests, `.m3u8` playlists and audio hashes.

- **Module:** `github.com/mrz1836/mixtape` — Go 1.26, **cgo required** for
  `internal/capture/tap` (Objective-C) and `internal/capture/device` (malgo).
  Everything else is pure Go.
- **Platform:** macOS only.
- **Dependencies:** cobra, pflag, x/term, x/sys, go-selfupdate, malgo; testify
  for tests. Adding one is a design decision, not a convenience — ask first.
- **Network:** Spotify's public embed and oEmbed pages (track lists), the
  image CDN when `--artwork` is on, and GitHub for `update`. No credentials,
  no API keys. Keep it that way.
- **Reference docs:** [../docs/CLI.md](../docs/CLI.md) (the public contract:
  commands, JSON shapes, events, exit codes) and
  [../docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md) (timing, segmentation,
  library, locks, and the measured Spotify behavior behind every constant).

| Package | Responsibility |
|---|---|
| `cmd/mixtape` | two-line main |
| `internal/cli` | cobra tree, output (text/JSON/NDJSON), error → exit-code mapping, commands |
| `internal/config` | `config.json` + `MIXTAPE_*` env + flags, with value sources |
| `internal/spotify` | link parsing, AppleScript player, catalog (embed/oEmbed), prefs |
| `internal/capture` | backend-neutral types and method selection (no cgo) |
| `internal/capture/tap` | Core Audio process tap (ObjC, C ring) |
| `internal/capture/device` | malgo capture device |
| `internal/spool` | chunked PCM spool with a time index |
| `internal/engine` | plan, steer, segment (pure), commit worker |
| `internal/library` | folders, manifests, naming, encode/tag/hash, verify |
| `internal/mp4tag` | pure-Go MP4 tags (`ilst`, `----` freeform, `covr`, `mdat` hash) |
| `internal/session` | recorder lock, `session.json`, event log, detach, Spotify restore |
| `internal/permission` | macOS privacy permissions of the host app: check, prompt, Settings deep links (C) |
| `internal/fileutil` | atomic writes and copies |
| `internal/wav` | PCM16 WAV header |
| `internal/testutil` | test-only: synthetic M4A/WAV files, a stand-in encoder, AAC and live-capture skips |

<br>

## Build, lint, test — always via `magex`

This repo uses MAGE-X, not raw `go` invocations. Full reference:
[tech-conventions/mage-x.md](tech-conventions/mage-x.md). Daily commands:

| Command | When to use |
|---|---|
| `magex format:fix` | Before every commit |
| `magex lint` | All linters (`magex lint:fix` to auto-fix) |
| `magex test` | Fast unit tests (default development loop) |
| `magex test:race` | Race detector — required for goroutine-touching changes |
| `magex build` | Build the `mixtape` binary into `./cmd/mixtape` |

`magex build` injects `version`, `commit` and `buildDate` through the ldflags in
[../.mage.yaml](../.mage.yaml); `mixtape version` prints them.

**Sandboxes.** Tests that encode real AAC skip themselves when `afconvert`
cannot see its AAC encoder (it can't inside an agent sandbox); run the suite
outside the sandbox before calling a change done. Never run anything that
plays Spotify or opens a capture device without the owner present: live checks
need both `-tags captureintegration` and `MIXTAPE_LIVE_CAPTURE=1` (`magex test`
enables every build tag it finds, so the tag alone must stay harmless), and
timing traces come from `mixtape debug trace`.

<br>

## Always run `/fix-all` before finishing

**Every change must pass the `/fix-all` process — no exceptions.** Do not
consider work complete, and do not commit, until format, pre-commit, lint, and
tests all pass:

```sh
magex format:fix && go-pre-commit run --all-files --skip lint && magex lint && magex test:race
```

Fix root causes, not symptoms. Read a file before modifying it. If tests fail,
say so with the output — never claim done on a red run.

<br>

## Contracts that must not drift

- **Exit codes and `error.code` strings** (`internal/cli/errors.go`): add, never
  renumber. Every new domain error gets a rule with a hint in `rules()`.
- **JSON output and NDJSON events** (`docs/CLI.md`): fields may be added, never
  renamed or removed. In JSON mode stdout and stderr carry only JSON.
- **Manifest schema 1** (`internal/library/types.go`): readers must tolerate
  unknown fields; a breaking change bumps `Schema` with a migration.
- **Help:** every command has Short, Long and Example; top-level commands have a
  group (`internal/cli/help_test.go` enforces it).
- **Timing constants** (`engine.DefaultTuning`): change them only with a trace
  that justifies it, and update the table in `docs/ARCHITECTURE.md`.

<br>

## Project-specific CI / build config: `.github/env/90-project.env`

[env/90-project.env](env/90-project.env) is **the** home for settings unique to
this repo (Go version pins, runners, coverage provider). The lower-numbered
`.github/env/` files are **synced across repos via go-broadcast** and get
overwritten; `90-project.env` loads last, overrides them, and is excluded from
the sync. Local, gitignored overrides go in `99-local.env`.

The same applies repo-wide: files delivered by go-broadcast (workflows, actions,
tech-conventions, community docs, `.vscode/`, editor configs) are edited in the
go-broadcast source, not here.

**Releases** are the one exception. GoFortress runs on Linux runners (the
module builds and tests there), which cannot build the macOS cgo binaries, so
`fortress.yml` has no tag trigger in this repo and
[workflows/release.yml](workflows/release.yml) releases `v*` tags on a macOS
runner. If go-broadcast ever syncs workflows into this repo, keep that change
(or exclude `fortress.yml`): otherwise a tag also starts a Fortress release
that fails on Linux.

<br>

## Don't

- Don't commit recordings or traces of personal listening. `*.wav`, `*.m4a` and
  `bin/` are gitignored; fixtures in `testdata/` hold public track IDs only.
- Don't add Spotify credentials or API keys. The only network calls are
  Spotify's public embed and oEmbed pages (track lists, and a song's release
  date when it is saved), artwork images with `--artwork`, and GitHub releases
  for `update`; don't add others.
- Don't let Go code run on Core Audio's real-time thread.
- Don't run raw `go test` / `golangci-lint` directly in place of `magex`.
- Don't use `--no-verify` on commits.
- Don't author `tech-conventions/` content into this file. Link to it.

<br>

## Keep this file honest

If you add or remove a command, a package, a dependency, or a network call, or
change the build or `/fix-all` flow, update this file in the same change.
