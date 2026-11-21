---
status: accepted
date: 2026-11-21
decision-makers: Brian Nunez
---

# Add a Wayland bus for Linux, with the screen cast portal, PipeWire and layer-shell covers

## Context and Problem Statement

[ADR-0004](0004-keep-the-core-platform-neutral-and-add-linux-as-a-second-bus.md) made Linux one more bus behind the contract of [ADR-0003](0003-use-a-one-way-event-and-command-bus-as-the-platform-contract.md) and deferred its dependencies to a new record. The Linux desktops PSV Guard targets first are Omarchy, which runs Hyprland, and Ubuntu, whose GNOME desktop is Wayland-only from 25.10 (26.04 LTS has no X11 session). Both are Wayland sessions.

Which session types does the Linux bus support, how does it capture the screen, draw covers and show its menu bar item and panel, and which libraries does that take?

## Decision Drivers

* Omarchy (Hyprland) and Ubuntu (GNOME) work out of the box.
* The contract, controller, monitor and coverage rules stay unchanged ([ADR-0004](0004-keep-the-core-platform-neutral-and-add-linux-as-a-second-bus.md)).
* Native code executes commands and decides nothing; all logic stays in Go ([ADR-0002](0002-keep-logic-in-go-with-objective-c-only-in-cgo-preambles.md)).
* Few libraries, each packaged by Arch and Ubuntu.
* No reliance on X11, which is being retired by the desktops PSV Guard targets.

## Considered Options

* Wayland only: the screen cast portal and PipeWire for capture, `wlr-layer-shell` covers through gtk-layer-shell, a GTK 3 panel and an AppIndicator item
* X11 first (XShm capture, override-redirect covers), Wayland later
* Both X11 and Wayland in the first Linux release
* Talk to the Wayland, D-Bus and PipeWire protocols directly in Go, without C libraries

## Decision Outcome

Chosen option: "Wayland only", because both target desktops are Wayland sessions, X11 is legacy on both, and the portal is the one capture path every Wayland desktop supports.

* **Session.** The bus runs only in a Wayland session (`WAYLAND_DISPLAY` set). Elsewhere `bridge.New` fails with a plain error that says PSV Guard needs a Wayland session; under X11 it does not fall back to Xwayland.
* **Capture.** `org.freedesktop.portal.ScreenCast` over D-Bus (GIO's GDBus) selects monitors, with `persist_mode` 2 so the user approves once; each session's restore token is kept in `os.UserConfigDir()/psvguard/state.json`. Some share dialogs (Hyprland's) offer one monitor at a time, so a display that no session shows gets one more dialog of its own, once per run. Frames arrive from PipeWire (`libpipewire-0.3`) as BGRx or BGRA in shared memory, only when the screen changes, and are passed to `goCaptureFrame` from PipeWire's thread. Portal streams are matched to displays by their reported position and size.
* **Downscaling in Go.** The portal delivers each display at full resolution, so `frame.Convert` area-averages a larger buffer into the analysed frame size, reading it once and allocating nothing.
* **Displays.** GDK's monitors, in logical pixels with their scale factor. GTK 3 does not expose Wayland connector names, so a display's ID is built from its model and position.
* **Covers.** One `wlr-layer-shell` overlay surface per display (gtk-layer-shell), with an empty input region, holding one opaque rectangle per cover; opacity fades with the command's durations. Where the compositor has no layer-shell (GNOME), the bus reports `canOverlay: false` and the controller warns with its status and a notification instead.
* **Capabilities.** `excludesOwnWindows: false` (portal captures include overlays; Hyprland's `no_screen_share` paints them black rather than leaving them out), `canOverlay` from layer-shell support, `perDisplayCapture: true`, `consentPerSession: true`, `pixelFormats` `BGRx` and `BGRA`.
* **Menu bar item and panel.** A StatusNotifierItem through libayatana-appindicator3 (Omarchy's shell and Ubuntu's AppIndicator extension both host them), whose menu shows the status, the on/off toggle, the panel and Quit; the panel is a GTK 3 window that does not take focus when shown. Without a tray host the panel opens at launch.
* **Other events.** Sleep and wake from logind's `PrepareForSleep`, notifications through `org.freedesktop.Notifications`, and on Hyprland, workspace switches from its event socket as transition events. Capture permission maps to the portal: `needsConsent` until the portal has started, then `granted`, or `denied` if the user cancels.
* **Code layout.** `internal/bridge/bus_linux.go` holds the Go side and its `//export`s; `internal/bridge/native_linux.go` holds all C, in its cgo preamble, under `//go:build linux && cgo`. `bus_other.go` narrows to the remaining platforms.
* **Libraries.** GTK 3 (with GLib and GIO), gtk-layer-shell, libayatana-appindicator3 and libpipewire-0.3, linked through cgo. They are build and run dependencies on Linux only; the core still builds with `CGO_ENABLED=0`.
* **Development desktops.** `dev/linux/` holds Docker images of a Hyprland session and an Ubuntu GNOME session, reached through noVNC, in which the Linux bus is built, tested and checked by hand. Docker is a development tool only.

### Consequences

* Good, because both target desktops get capture, and Omarchy gets covers, without changing the contract or the core.
* Good, because the portal and PipeWire are the same on every Wayland desktop, so KDE and other layer-shell compositors need no new code.
* Bad, because on GNOME PSV Guard can only warn: GNOME has no supported way for an app to draw over others on Wayland.
* Bad, because the first run asks the user to share their screen, once per display where the dialog offers one monitor at a time, and a display added later needs another approval.
* Bad, because capture includes the covers, so covered content is not analysed until the hold expires (the no-exclusion fallback of ADR-0004).
* Bad, because full-resolution frames cost more CPU than macOS's GPU-scaled capture: downscaling a 2560×1440 frame takes about 4 ms on the development machine.
* Neutral, because X11 sessions are not supported; users of X11-only desktops run a Wayland session or wait for a separate decision.

### Confirmation

* `GOOS=linux CGO_ENABLED=0` builds and the portable tests keep passing in `make test`; a CI job builds the cgo Linux bus.
* `internal/frame` tests cover downscaling (exact means, uneven ratios, row padding, no allocation); `internal/bridge` tests on Linux cover the coordinate conversion and event payloads.
* The changes are checked by hand in both development desktops: tray item and panel, consent and restore, covers on psv's failing example videos on Hyprland, warnings on GNOME, passing content left uncovered, click-through, workspace switches, monitor changes and quit.

## Pros and Cons of the Options

### Wayland only

* Good, because it matches what Omarchy and Ubuntu ship.
* Good, because one capture path serves every Wayland desktop.
* Bad, because GNOME gets warnings only.

### X11 first, Wayland later

* Good, because X11 capture and overlays are simple and need no consent.
* Bad, because neither target desktop runs X11 by default, so it would serve neither.

### Both X11 and Wayland now

* Good, because it covers the most desktops.
* Bad, because it doubles the native code and its testing for a session type both targets are leaving.

### Protocols directly in Go

* Good, because it adds no C libraries.
* Bad, because it means reimplementing D-Bus, PipeWire's native protocol, Wayland surfaces and a widget toolkit for the panel: far more code to own than the libraries it avoids.

## More Information

* Screen cast portal: <https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.ScreenCast.html>
* `wlr-layer-shell`: <https://wayland.app/protocols/wlr-layer-shell-unstable-v1>
* Ubuntu drops GNOME on Xorg: <https://discourse.ubuntu.com/t/ubuntu-25-10-drops-support-for-gnome-on-xorg/62538>
* Revisit if GNOME gains a supported overlay, if X11 support is requested, or if a portal adds capture that excludes the app's own surfaces.
