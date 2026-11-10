<p align="center">
  <img src="assets/icon.svg" width="128" height="128" alt="PSV Guard icon: a white letter P on a rose rounded square">
</p>

<h1 align="center">PSV Guard</h1>

<p align="center">
  <b>A screen guard for photosensitive users; it watches your displays and covers flashing as it starts.</b>
</p>

<p align="center">
  PSV Guard lives in the menu bar, checks everything on screen with the <a href="https://github.com/brian-nunez/psv">psv</a> flash detector, and dims the parts that begin to flash.<br>
  <i>By Brian Nunez.</i>
</p>

<p align="center">
  <img alt="Written in Go" src="https://img.shields.io/badge/written%20in-Go-e11d48?style=flat-square&logo=go&logoColor=white">
  <img alt="Runs on macOS" src="https://img.shields.io/badge/runs%20on-macOS-be123c?style=flat-square&logo=apple&logoColor=white">
  <img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-e11d48?style=flat-square">
</p>

<p align="center">
  <a href="https://github.com/brian-nunez/psv"><img alt="psv on GitHub" src="https://img.shields.io/badge/psv%20on%20GitHub-e11d48?style=for-the-badge&logo=github&logoColor=white" height="40"></a>
</p>

<p align="center">
  <a href="https://github.com/brian-nunez/psv"><b>psv</b></a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="docs/adr/README.md">Decisions</a> ·
  <a href="AGENTS.md">AGENTS.md</a>
</p>

> [!WARNING]
> **PSV Guard is an experimental aid.** It reduces exposure to flashing content but does not eliminate it, and it does not detect spatial patterns. Do not rely on it as your only protection.

<p align="center">
  <a href="#why-psv-guard">Why PSV Guard</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#settings">Settings</a> ·
  <a href="#platforms">Platforms</a> ·
  <a href="#development">Development</a> ·
  <a href="#license">License</a>
</p>

## Why PSV Guard

- **Early.** It covers flashing as it builds up, before the detector has finished deciding that the content fails: about a third of a second after full-screen flashing starts, a second and a quarter before psv's verdict.
- **Targeted.** Only the flashing part of the screen is dimmed, so the rest stays usable. When most of a display flashes, the whole display is covered. Scrolling, typing and ordinary video stay uncovered.
- **Calm.** Covers fade in quickly, fade out slowly and stay in place through the gaps between flashes, so the guard never becomes a flashing source itself.
- **Out of the way.** It runs from the menu bar, with no Dock icon, and its covers never take focus or block clicks.
- **Grounded in a real detector.** Detection comes from [psv](https://github.com/brian-nunez/psv), which follows the WCAG 2.2 flash thresholds.

## Quick start

```sh
make app                       # build and sign dist/PSV Guard.app
open "dist/PSV Guard.app"      # start it; it appears in the menu bar
```

On first launch, macOS asks for **Screen Recording** permission. Grant it in System Settings, then quit and reopen PSV Guard. Click the shield in the menu bar for the panel; right-click it for the on/off toggle and Quit.

If the permission is lost after a rebuild, reset it and grant it again:

```sh
tccutil reset ScreenCapture com.briannunez.psvguard
```

Building needs Go 1.25 or later, the Xcode Command Line Tools and FFmpeg (used by the tests).

## How it works

1. PSV Guard captures each display, scaled to about 640 pixels wide, and analyses it 60 times a second.
2. Every display gets its own [psv](https://github.com/brian-nunez/psv) analyser, which reports, in each frame, where pixels are building up transitions.
3. When flashing builds up over enough of the screen, and fills the areas it covers the way flashing does rather than the way scrolling text does, PSV Guard covers those areas with dark, near-opaque panels.
4. Once an area has been calm for three seconds, its cover fades away. Any failure psv later decides that was not covered is covered at once and counted.

PSV Guard's own covers are kept out of the capture, so they never trigger more covering. The details, including the measured timings, are in [docs/architecture.md](docs/architecture.md).

## Settings

Settings are in the panel and saved in `~/Library/Application Support/psvguard/settings.json`. Every setting is also a command-line flag of the same name, which overrides the saved value for that run.

| Setting | Default | Effect |
| ------- | ------- | ------ |
| `enabled` | `true` | Watch the screen and cover flashing. |
| `preset` | `wcag22` | psv thresholds: `wcag22`, `trace24` or `broadcast`. |
| `hot-peak` | `4` | Transitions within a second at which an area counts as building up flashing (WCAG fails above 6). |
| `min-hot-area` | `15` | Share of the display, in percent, that must build up flashing before covering starts. |
| `flashing-area` | `1` | Keep covers up while this share of the display, in percent, already flashes. |
| `full-area` | `40` | Cover the whole display once this share, in percent, is covered. |
| `calm` | `3` | Seconds an area stays covered after it last built up flashing. |
| `fps` | `60` | Frames analysed per second for each display. Flag only. |
| `capture-width` | `640` | Width, in pixels, each display is scaled to for analysis. Flag only. |
| `panel` | `true` | Show the panel when PSV Guard starts. |

Other flags: `--verbose` logs decisions and per-display stats (frame rate, time per frame, hot area, covers, misses) to stderr, `--trace-bridge` logs every bridge event and command, and `--version` prints the version.

## Platforms

| Platform | Status |
| -------- | ------ |
| **macOS** | Supported |
| Linux | Next |

Everything except the native capture and drawing layer is plain Go, so adding a platform means adding that one layer.

## Development

```sh
make test                      # race tests, psv's example videos, synthetic clips, Linux and no-cgo builds
make lint                      # golangci-lint, configured in .golangci.yml
make run                       # build the app and run it with --verbose in the terminal
```

- [Architecture](docs/architecture.md): pipeline, covering policy, bus protocol, adding a platform
- [Architecture decisions](docs/adr/README.md)
- [Guide for coding agents](AGENTS.md)

## Related

- [psv](https://github.com/brian-nunez/psv): the photosensitivity detector PSV Guard is built on.

## License

PSV Guard is licensed under the [MIT License](LICENSE). Copyright (c) 2026 Brian Nunez.
