---
status: accepted
date: 2026-11-09
decision-makers: Brian Nunez
---

# Keep all logic in Go, with Objective-C only in cgo preambles

## Context and Problem Statement

psvguard needs native macOS APIs: AppKit for the menu bar item, the settings panel and the cover windows; ScreenCaptureKit for capture; CoreMedia and CoreVideo for frame buffers; CoreGraphics for displays and Screen Recording permission; QuartzCore for cover fades. Those APIs are Objective-C (and Swift). The rest of the app (state, settings, resampling, frame conversion, trigger rules and cover geometry) has no inherent platform tie, and Linux is the next platform to support.

Which language owns which part, and how is the native code built?

## Decision Drivers

* The core must build and test on Linux and with `CGO_ENABLED=0`, so Linux support means adding native code, not porting logic.
* One toolchain and one build command: `go build` plus a Makefile, no IDE project files.
* Decisions must be testable with plain `go test` and fakes, without a Mac or a GUI session.
* Brian's existing app, [bvoice](https://github.com/brian-nunez/bvoice), already proves the pattern and should be mirrored.
* The native layer stays small enough to review line by line.

## Considered Options

* All logic in Go; thin Objective-C bindings inside cgo preambles of `*_darwin.go` files
* A Swift (or SwiftUI) app that embeds the Go logic as a library
* Separate `.m`/`.h` Objective-C files compiled by cgo alongside the Go package
* A pure-Go Objective-C runtime bridge (purego/objc-style bindings)

## Decision Outcome

Chosen option: "All logic in Go; thin Objective-C bindings inside cgo preambles of `*_darwin.go` files", because it keeps every decision in testable, portable Go, builds with the Go toolchain alone, and matches bvoice.

* Objective-C lives only in cgo preambles of `*_darwin.go` files, built with `#cgo CFLAGS: -x objective-c -fobjc-arc` and the frameworks in `#cgo LDFLAGS`. No `.m`, `.h`, `.swift`, Xcode project or storyboard.
* Darwin files carry `//go:build darwin && cgo`; their counterparts carry `//go:build !darwin || !cgo` and return a clear "not yet supported on this platform" error.
* C functions use the `pg_` prefix. Go callbacks are `//export`ed and declared `extern` in the preamble that calls them.
* The Objective-C side holds only AppKit and ScreenCaptureKit objects and executes commands; it makes no decisions and knows no Go types (see [ADR-0003](0003-use-a-one-way-event-and-command-bus-as-the-platform-contract.md)).
* `func init() { runtime.LockOSThread() }`, and the AppKit run loop owns the main thread until quit.
* Dependencies stay as in [ADR-0001](0001-detect-flashing-with-psv-watch.md): no third-party Go modules for bindings.

### Consequences

* Good, because thresholds, hold timers, rectangle merging and the controller state machine are unit-tested on any OS.
* Good, because the build is `go build` plus `codesign`; no Xcode project to keep in sync.
* Good, because a Linux port reuses all Go logic and adds only a native layer.
* Bad, because Objective-C inside Go string-like preambles has weaker editor support and no separate compilation; mistakes surface at `go build` time.
* Bad, because cgo adds call overhead and pointer-passing rules; the frame path must be designed so each frame crosses once with no per-frame allocation.
* Bad, because SwiftUI conveniences are unavailable; the panel is built with AppKit directly.

### Confirmation

* `find . -name '*.m' -o -name '*.h' -o -name '*.swift' -o -name '*.xcodeproj'` returns nothing.
* `GOOS=linux go build ./...`, `GOOS=linux go vet ./...` and `CGO_ENABLED=0 go build ./...` pass (run by `make test` or CI).
* Review checks that Objective-C code only applies commands and reports events, with no thresholds or geometry decisions.

## Pros and Cons of the Options

### All logic in Go; thin Objective-C bindings inside cgo preambles of `*_darwin.go` files

* Good, because it is a single-language core with a single toolchain.
* Good, because it is a proven pattern in bvoice (`internal/platform/platform_darwin.go`, `internal/ui/appkit`).
* Neutral, because the Objective-C surface is small but must still be reviewed for main-thread and ARC correctness.
* Bad, because debugging crashes across the cgo boundary is harder than in a single-language app.

### A Swift (or SwiftUI) app that embeds the Go logic as a library

* Good, because it gives first-class access to Apple frameworks and modern UI.
* Bad, because it needs Xcode or a Swift package build next to the Go build, and two languages for app logic tend to split decisions between them.
* Bad, because none of the Swift side carries over to Linux.

### Separate `.m`/`.h` Objective-C files compiled by cgo alongside the Go package

* Good, because Objective-C gets proper editor support.
* Neutral, because cgo compiles them without extra tooling.
* Bad, because it departs from bvoice's convention and makes it easier for native code to grow beyond thin bindings.

### A pure-Go Objective-C runtime bridge (purego/objc-style bindings)

* Good, because it could avoid cgo.
* Bad, because it is a new third-party dependency, which [ADR-0001](0001-detect-flashing-with-psv-watch.md) rules out without approval.
* Bad, because blocks, delegates and ScreenCaptureKit's protocol callbacks are awkward and fragile through runtime messaging.

## More Information

* Reference implementation: bvoice `docs/architecture.md`, `internal/platform/platform_darwin.go`, `internal/ui/appkit/bridge_darwin.go`.
* The bridge contract and its threading rules are in [ADR-0003](0003-use-a-one-way-event-and-command-bus-as-the-platform-contract.md); the Linux plan is in [ADR-0004](0004-keep-the-core-platform-neutral-and-add-linux-as-a-second-bus.md).
