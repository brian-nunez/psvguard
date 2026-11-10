---
status: accepted
date: 2026-11-09
decision-makers: Brian Nunez
---

# Keep the core platform-neutral and add Linux as a second bus

## Context and Problem Statement

v1 of psvguard ships on macOS only, but Linux is the next platform. Linux differs from macOS in ways that matter to a screen guard: on X11, captures usually include the app's own cover windows; on Wayland, capture goes through `xdg-desktop-portal` and PipeWire and needs user consent each session, and whether an app may draw over others depends on the compositor (GNOME offers no supported way). If v1 assumes macOS behaviour (covers excluded from capture, overlays always possible, permission granted once), Linux would force a rework of the core.

How is v1 built so that adding Linux means adding one bus implementation, not changing the controller, the monitor or the coverage rules?

## Decision Drivers

* Linux support must be additive: one new native layer behind the existing contract ([ADR-0003](0003-use-a-one-way-event-and-command-bus-as-the-platform-contract.md)).
* The core must not assume capture excludes the app's own covers.
* The app must stay useful where overlays are impossible.
* Platform independence must be checked by the build, not by convention.
* No Linux-specific dependencies are added in v1.

## Considered Options

* Platform-neutral core plus a capabilities event, with the no-exclusion and no-overlay fallbacks implemented and tested in v1
* Build macOS-only now and refactor when Linux work starts
* Abstract every native concern behind fine-grained per-feature interfaces now

## Decision Outcome

Chosen option: "Platform-neutral core plus a capabilities event, with the no-exclusion and no-overlay fallbacks implemented and tested in v1", because it makes Linux a matter of writing one bus, proves the core runs under Linux-like conditions today, and adds no dependencies.

* **Only the bus is platform code.** Every package except the bus imports nothing OS-specific and builds and tests with `GOOS=linux` and `CGO_ENABLED=0`.
* **Capabilities, not assumptions.** On startup the bus sends a `capabilities` event and the controller adapts to it:
  * `excludesOwnWindows`: capture leaves out the app's covers.
  * `canOverlay`: the bus can draw click-through covers over other apps.
  * `perDisplayCapture`: one stream per display rather than one combined stream.
  * `consentPerSession`: the user must approve capture each session.
  * `pixelFormats`: the formats the bus can deliver.
* **No-exclusion fallback.** When `excludesOwnWindows` is false, the core keeps its own covers from feeding back into analysis: cells it is covering are skipped when deciding hot cells, and "calm" is judged from the uncovered surroundings and hold timers.
* **No-overlay fallback.** When `canOverlay` is false, the controller degrades to warnings: a status change and a desktop notification command.
* **Neutral vocabulary** in the contract: opaque string display IDs, normalised top-left geometry, a pixel format enum, and a generic permission state.
* **Paths** come from `os.UserConfigDir()` (`~/Library/Application Support/psvguard` on macOS, `~/.config/psvguard` on Linux).
* **Build tags and names:** `bus_darwin.go` is `//go:build darwin && cgo`; `bus_other.go` is `//go:build !darwin || !cgo` and returns a "not yet supported on this platform" error. A future `bus_linux.go` slots in beside them, narrowing `bus_other.go`'s tag.
* **Packaging is per platform.** The `.app` bundle and `codesign` steps are a macOS-only Makefile target ([ADR-0005](0005-ship-macos-builds-as-an-ad-hoc-signed-app-bundle.md)); Linux targets (a plain binary plus a `.desktop` file) get their own targets later.

What the Linux bus will need (not implemented in v1):

| Session | Capture | Covers | Expected capabilities |
| --- | --- | --- | --- |
| X11 | XShm or XComposite | override-redirect, always-on-top windows with an empty input shape (click-through) | `excludesOwnWindows: false`, `canOverlay: true` |
| Wayland, wlroots-based or KDE | `xdg-desktop-portal` ScreenCast + PipeWire | `wlr-layer-shell` surfaces | `consentPerSession: true`, `canOverlay: true`, `excludesOwnWindows` per compositor |
| Wayland, GNOME | `xdg-desktop-portal` ScreenCast + PipeWire | no supported overlay | `consentPerSession: true`, `canOverlay: false` (warnings only) |

### Consequences

* Good, because the controller, monitor and coverage rules are exercised today under Linux-like capabilities, so the Linux bus inherits tested behaviour.
* Good, because CI catches any OS-specific import that leaks into the core.
* Good, because a platform without overlays still warns the user instead of doing nothing.
* Bad, because v1 carries fallback code that macOS does not use, which must still be maintained and tested.
* Bad, because the no-exclusion fallback is less precise than true exclusion: content under a cover is not analysed while it is covered, so uncovering relies on hold timers and surroundings.
* Neutral, because Linux dependencies (PipeWire, X11 or D-Bus bindings, in Go or C) are deliberately deferred and will need Brian's approval and their own ADR when Linux work starts.

### Confirmation

* `GOOS=linux go build ./...`, `GOOS=linux go vet ./...`, `CGO_ENABLED=0 go build ./...` and the platform-neutral tests with `CGO_ENABLED=0` pass in `make test` or CI.
* Controller tests run with a fake bus under macOS-like capabilities and under Linux-like ones (`excludesOwnWindows: false`, `canOverlay: false`, `consentPerSession: true`).
* Review checks that nothing outside `bus_darwin.go` refers to macOS types, APIs or coordinate conventions.
* `docs/architecture.md` has an "Adding a platform" section that matches this ADR.

## Pros and Cons of the Options

### Platform-neutral core plus a capabilities event, with the no-exclusion and no-overlay fallbacks implemented and tested in v1

* Good, because the hardest Linux differences (feedback from visible covers, no overlays, per-session consent) are solved in portable Go before any Linux code exists.
* Good, because capabilities make platform differences explicit data rather than scattered `runtime.GOOS` checks.
* Bad, because it is more v1 work than macOS needs on its own.

### Build macOS-only now and refactor when Linux work starts

* Good, because v1 ships sooner.
* Bad, because macOS assumptions (excluded covers, overlays always available) would be baked into the controller and coverage rules, and removing them later touches the riskiest code.
* Bad, because there would be no proof the core builds or behaves correctly on Linux until the port.

### Abstract every native concern behind fine-grained per-feature interfaces now

* Good, because each concern could be swapped independently.
* Bad, because it multiplies interfaces and fakes without adding flexibility the single bus contract lacks.
* Bad, because it contradicts the single event/command contract of [ADR-0003](0003-use-a-one-way-event-and-command-bus-as-the-platform-contract.md).

## More Information

* `xdg-desktop-portal` ScreenCast: <https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.ScreenCast.html>
* `wlr-layer-shell`: <https://wayland.app/protocols/wlr-layer-shell-unstable-v1>
* Revisit when Linux work starts: write a new ADR for the Linux bus and its dependencies, linking back here.
