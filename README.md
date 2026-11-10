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
  <a href="#platforms">Platforms</a> ·
  <a href="#documentation">Documentation</a> ·
  <a href="#license">License</a>
</p>

## Why PSV Guard

- **Early.** It covers flashing as it builds up, before the detector has finished deciding that the content fails.
- **Targeted.** Only the flashing part of the screen is dimmed, so the rest stays usable. When most of a display flashes, the whole display is covered.
- **Calm.** Covers fade in quickly, fade out slowly and stay in place through the gaps between flashes, so the guard never becomes a flashing source itself.
- **Out of the way.** It runs from the menu bar, with no Dock icon, and its covers never take focus or block clicks.
- **Grounded in a real detector.** Detection comes from [psv](https://github.com/brian-nunez/psv), which follows the WCAG 2.2 flash thresholds.

## Quick start

```sh
make app                       # build and sign dist/PSV Guard.app
open "dist/PSV Guard.app"      # start it; it appears in the menu bar
```

On first launch, macOS asks for **Screen Recording** permission. Grant it in System Settings, then turn PSV Guard on from the menu bar.

If the permission is lost after a rebuild, reset it and grant it again:

```sh
tccutil reset ScreenCapture com.briannunez.psvguard
```

Building needs Go, the Xcode Command Line Tools and FFmpeg (used by the tests).

## How it works

1. PSV Guard captures each display continuously.
2. Every display gets its own psv analyser, which reports where flashing is building up.
3. When enough of the screen is flashing, PSV Guard covers those areas with dark, near-opaque panels.
4. Once an area has been calm for a few seconds, its cover fades away.

PSV Guard's own covers are kept out of the analysis, so they never trigger more covering.

## Platforms

| Platform | Status |
| -------- | ------ |
| **macOS** | Supported |
| Linux | Next |

Everything except the native capture and drawing layer is plain Go, so adding a platform means adding that one layer.

## Documentation

- [Architecture decisions](docs/adr/README.md)
- [Guide for coding agents](AGENTS.md)

## Related

- [psv](https://github.com/brian-nunez/psv): the photosensitivity detector PSV Guard is built on.

## License

PSV Guard is licensed under the [MIT License](LICENSE). Copyright (c) 2026 Brian Nunez.
