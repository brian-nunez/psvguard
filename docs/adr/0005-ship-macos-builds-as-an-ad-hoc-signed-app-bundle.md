---
status: accepted
date: 2026-11-09
decision-makers: Brian Nunez
---

# Ship macOS builds as an ad-hoc signed .app bundle

## Context and Problem Statement

psvguard captures the screen with ScreenCaptureKit, which requires Screen Recording permission. macOS grants that permission (TCC) to the *responsible* application: a bare binary launched from a terminal inherits the terminal's identity, so permission would be granted to Terminal or the IDE instead of psvguard, and could break whenever the binary changes. psvguard also runs as a menu bar app with no Dock icon. It only needs to run locally on Brian's Mac; there is no distribution, notarisation or auto-update.

How is the macOS build packaged and signed so Screen Recording permission is attributed to psvguard and survives rebuilds where possible?

## Decision Drivers

* Screen Recording permission must belong to psvguard itself, not a terminal or IDE.
* Permission should survive rebuilds as often as macOS allows.
* Menu bar only: no Dock icon, no main window.
* Build with `make`; no Xcode project ([ADR-0002](0002-keep-logic-in-go-with-objective-c-only-in-cgo-preambles.md)).
* No Apple Developer account, notarisation or distribution pipeline.

## Considered Options

* A `.app` bundle assembled by the Makefile and signed ad hoc, with a stable bundle ID
* A bare Go binary run from the terminal
* A `.app` bundle signed with an Apple Developer ID and notarised

## Decision Outcome

Chosen option: "A `.app` bundle assembled by the Makefile and signed ad hoc, with a stable bundle ID", because it gives psvguard its own TCC identity without any account or Xcode tooling, and it is all a local-only app needs.

* `make app` assembles `dist/PSV Guard.app`:
  * `Contents/MacOS/psvguard`: the Go binary.
  * `Contents/Info.plist`: bundle ID `com.briannunez.psvguard`, `LSUIElement` true (no Dock icon), version from `git describe`.
  * `Contents/Resources/`: the app icon.
* The bundle is signed ad hoc: `codesign --force --sign - "dist/PSV Guard.app"`.
* The bundle ID and signing identity stay stable across builds.
* If permission is lost, it is reset with `tccutil reset ScreenCapture com.briannunez.psvguard` and granted again.
* This is a macOS-only Makefile target; Linux packaging gets separate targets ([ADR-0004](0004-keep-the-core-platform-neutral-and-add-linux-as-a-second-bus.md)).

### Consequences

* Good, because System Settings lists "PSV Guard" under Screen Recording, and the permission is tied to the app.
* Good, because the bundle is produced by plain files and `make`, with no Xcode project.
* Good, because `LSUIElement` keeps the app out of the Dock and the app switcher.
* Bad, because ad-hoc signatures change with every build, so macOS may ask for Screen Recording permission again after a rebuild; the reset command is documented for that case.
* Bad, because the app cannot be distributed to other Macs without Gatekeeper warnings; that is out of scope.
* Neutral, because development runs of the bare binary (`go run`) still work for logic, but capture permission is only reliable when launched as the bundle.

### Confirmation

* `make app` produces `dist/PSV Guard.app`, and `codesign --verify --verbose "dist/PSV Guard.app"` passes.
* `plutil -p "dist/PSV Guard.app/Contents/Info.plist"` shows `CFBundleIdentifier` `com.briannunez.psvguard` and `LSUIElement` true.
* Manual check: launching the bundle shows "PSV Guard" in System Settings → Privacy & Security → Screen Recording.

## Pros and Cons of the Options

### A `.app` bundle assembled by the Makefile and signed ad hoc, with a stable bundle ID

* Good, because it gives psvguard its own TCC identity at no cost.
* Good, because it needs only `codesign`, which ships with the Command Line Tools.
* Bad, because permission may need re-granting after rebuilds.

### A bare Go binary run from the terminal

* Good, because there is no packaging step.
* Bad, because Screen Recording permission is attributed to the terminal or IDE, so granting it exposes every program run from there.
* Bad, because a menu bar app without `LSUIElement` shows a Dock icon, or needs activation-policy workarounds.

### A `.app` bundle signed with an Apple Developer ID and notarised

* Good, because the signing identity is stable, so permission survives rebuilds reliably, and the app can be shared.
* Bad, because it needs a paid Apple Developer account, certificates and a notarisation workflow, none of which a local-only app needs.

## More Information

* `tccutil reset ScreenCapture com.briannunez.psvguard` clears psvguard's Screen Recording grant.
* Revisit if psvguard is ever distributed; that would need a Developer ID, notarisation and a new ADR.
