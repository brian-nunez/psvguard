# psvguard

PSV Guard (`psvguard`) is a menu bar app that captures every display in real time, streams the frames through Brian's photosensitivity detector [psv](https://github.com/brian-nunez/psv) (`Analyzer.Watch`), and covers the parts of the screen that start flashing, to reduce exposure for photosensitive users. It is written in Go, with thin Objective-C bindings for macOS. macOS is the first platform; Linux (X11 and Wayland) is planned next, and v1 is built so that Linux is one more bus implementation, not a rework. Stage: v1 on macOS, run locally from a source build.

It is an experimental aid: it reduces exposure but does not eliminate it, and it does not detect spatial patterns. Keep that wording in user-facing text.

## Invariants

Product requirements. Do not break them; stop and ask Brian before working around one.

1. **psv is a library, used as is.** Depend on a tagged psv release `>= v0.2.0`, never a commit or pseudo-version. Use only its public API, only `Analyzer.Watch` (one analyser per display), and never modify psv. psvguard owns *when and where to cover*; it never reimplements detection. ([ADR-0001](docs/adr/0001-detect-flashing-with-psv-watch.md))
2. **Dependencies:** the psv module and FFmpeg (system tool, tests and verification only). Otherwise the Go standard library, cgo and Apple system frameworks. Any other Go module, C library or tool needs Brian's approval and an ADR.
3. **Go first.** All logic is Go: state, settings, resampling, frame conversion, trigger rules, cover geometry and fades. Objective-C exists only in cgo preambles of `*_darwin.go` files. No Swift, `.m`, `.h`, Xcode project or storyboard. ([ADR-0002](docs/adr/0002-keep-logic-in-go-with-objective-c-only-in-cgo-preambles.md))
4. **The bus is the platform contract.** `goEvent` and `goCaptureFrame` in, `pg_post` out; one-way and asynchronous both ways; no round trips; callbacks only copy and enqueue; native code executes commands and decides nothing. ([ADR-0003](docs/adr/0003-use-a-one-way-event-and-command-bus-as-the-platform-contract.md))
5. **Platform-neutral core, Linux next.** Nothing outside the bus implementation may know it runs on macOS. The core must build, vet and test with `GOOS=linux` and `CGO_ENABLED=0`. The controller adapts to the bus's `capabilities` event and must keep working when `excludesOwnWindows`, `canOverlay` are false or `consentPerSession` is true. ([ADR-0004](docs/adr/0004-keep-the-core-platform-neutral-and-add-linux-as-a-second-bus.md))
6. **Covers never feed back into analysis.** On macOS the capture filter excludes psvguard's own windows; where a platform cannot, Go skips the cells it is covering.
7. **Fades stay far below flash thresholds.** A cover must never itself become a flashing stimulus: fade in quickly, fade out slowly, never toggle rapidly.
8. **Never block the main thread or the capture queue.** The frame path allocates nothing per frame and drops frames rather than queueing them.
9. **Packaged as a signed `.app`** with the stable bundle ID `com.briannunez.psvguard`, so Screen Recording permission belongs to psvguard. ([ADR-0005](docs/adr/0005-ship-macos-builds-as-an-ad-hoc-signed-app-bundle.md))

## Layout

```
cmd/psvguard/       flags, settings, wiring; bridge.New picks the platform's bus
internal/app/       controller (events in, commands out); tests use the fake bus in app_test.go
internal/bridge/    contract.go (kinds, payloads, binary cover layout), outbox.go, queue.go, bus.go;
                    bus_darwin.go (Go side, //export) + native_darwin.go (all Objective-C); bus_other.go
internal/monitor/   per-display Pipeline (resample → Watch → coverage) and Display (goroutine + fast path)
internal/coverage/  the covering policy; pure functions on psv.Building
internal/resample/  internal/frame/  internal/settings/  internal/ui/
packaging/macos/    Info.plist template
docs/whitepaper/    LaTeX white paper; figdata/ (build tag whitepaper) regenerates its data and figures
```

- Only `internal/bridge/*_darwin.go` may know it runs on macOS. Every other package is pure Go and must build with `GOOS=linux` and `CGO_ENABLED=0`.
- Darwin files use `//go:build darwin && cgo`; `bus_other.go` uses `//go:build !darwin || !cgo` and returns `ErrUnsupported`. A Linux bus goes in `bus_linux.go`, narrowing `bus_other.go`'s tag.
- cgo forbids C definitions in a preamble next to `//export`, so Objective-C definitions go in `native_darwin.go` and exports in `bus_darwin.go`.
- Event and command kinds live in `contract.go`; `native_darwin.go` mirrors them in enums marked "Must match". Change both together, plus the tables in `docs/architecture.md`.
- Geometry crossing the bus is normalised (0–1, top-left origin per display). `pg_norm_to_points` converts it on macOS and is tested through `normToPoints` in `bus_darwin_test.go`.
- New settings go in `settings.Fields` only; flags, the saved file and panel controls follow from it.

## Commands

Toolchain: Go 1.25+ (psv requires it), Xcode Command Line Tools, FFmpeg (`ffmpeg -version`), golangci-lint v2.

| Task | Command |
| --- | --- |
| Build the binary | `make build` (writes `build/psvguard`) |
| Build and ad-hoc sign the app | `make app` (writes `dist/PSV Guard.app`) |
| Run with logs in the terminal | `make run` (the app with `--verbose`) |
| All tests | `make test` (race tests, example videos, synthetic clips, Linux and no-cgo builds; about a minute) |
| Quick tests | `make test-short` |
| Capture smoke test | `make smoke` (needs Screen Recording permission for the terminal) |
| Vet, lint, format | `make vet`, `make lint`, `make fmt` |
| Sweep thresholds | `PSVGUARD_TUNE=1 go test ./internal/monitor -run TestTuneThresholds -v` |
| Check a video for false covers | `PSVGUARD_CLIP=<file> go test ./internal/monitor -run TestClip -v` |

Running `dist/PSV Guard.app/Contents/MacOS/psvguard` from a terminal uses the terminal's Screen Recording permission; opening the bundle uses the app's own. Lost permission after a rebuild: `tccutil reset ScreenCapture com.briannunez.psvguard`, then open the bundle and grant it again.

### Before finishing a change

1. `make fmt` leaves no changes.
2. `make lint` reports 0 issues, and `make vet` passes.
3. `make test` passes: `go test -race -short ./...`, the example-video and synthetic-clip tests (without `-race`, which makes them too slow), and the Linux and `CGO_ENABLED=0` builds.
4. After a change to the bridge or `native_darwin.go`, `make smoke` passes and `make run` shows covers on psv's failing example videos.
5. Docs (`README.md`, `docs/architecture.md`, ADRs) are updated if behaviour, flags, settings, the bus protocol, build steps or layout changed.

## Conventions

- **cgo:** `#cgo CFLAGS: -x objective-c -fobjc-arc`, frameworks in `#cgo LDFLAGS`. C functions use the `pg_` prefix. Go callbacks are `//export`ed and declared `extern` in the preamble that calls them, not taken from `_cgo_export.h`. Native code never knows Go types; low-rate payloads are JSON.
- **Bus rules:** never call `pg_post` from an exported callback; one outbound goroutine owns every `pg_post`, coalesces, and posts covers only when they change. A query is a command plus an answering event.
- **Frame path:** `goCaptureFrame` converts the locked buffer, honouring `bytesPerRow`, straight into a pooled `psv.Frame`; no intermediate copy, no `C.CString`, `C.GoBytes` or new slices per frame. Count every dropped frame in stats.
- **Main thread:** `runtime.LockOSThread()` in `init`; the AppKit run loop owns the main thread until quit. All AppKit work goes through the main dispatch queue.
- **Cover panels:** borderless, click-through, on all Spaces, full-screen auxiliary, never key, never activating. The settings panel never steals focus.
- **Settings** resolve default → saved → flag, and live under `os.UserConfigDir()/psvguard`.
- **Tests:** controller tests use the fake bus and run under both macOS-like and Linux-like capabilities. Extend the fakes rather than adding real dependencies. Tests that need Screen Recording permission or FFmpeg skip cleanly when it is missing.
- **Go style:** follow `.golangci.yml` (configured like bvoice's). Fix findings rather than adding `//nolint`. Wrap errors with context; user-facing errors say what failed in plain words. Logging goes to an injected logger, to stderr only with `--verbose`.
- **Comments and docs** describe the code as it is: no references to planning chats, who decided what, or roadmaps outside ADRs.
- Report measured numbers (latency, CPU, memory, drops, false triggers) honestly, including ones that miss expectations.

## Known pitfalls

- **Covering policy changes** need the example-video and synthetic-clip tests, not just unit tests: plain peak and area thresholds cover scrolling text and film grain. Check new content with `TestClip`.
- **Boxing booleans in Objective-C:** `@(x != 0)` becomes a JSON number; use `x ? @YES : @NO`. Event decoding fails otherwise.
- **Handle lifetime:** a stream's `cgo.Handle` is deleted on its stopped event. That event must be emitted only after the stream's sample queue drains (`PGStream reportStopped`), or a late frame callback panics on an invalid handle.
- **Memory:** psv's region search allocates several MB per frame while the screen flashes. That is psv's, not a leak; `main` sets a soft memory limit.
- **Driving QuickTime with `osascript`** needs Automation consent and hangs until granted; `ffplay -fs -autoexit <file>` plays a clip full-screen without it.

## Commits

Every commit follows [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/).

```text
<type>(<scope>): <summary>

<body>

<footers>
```

*   **Type**: `feat` (new behavior), `fix` (corrected behavior), `docs`, `refactor` (no behavior change), `perf`, `test`, `build`, `ci`, `style` (formatting only), `chore`, `revert`.
*   **Scope**: the area changed, named after the package or area: `app`, `bridge`, `monitor`, `coverage`, `resample`, `frame`, `settings`, `ui`, `cmd`, `build`, `docs`, `adr`. Omit it only when the change spans the whole repository.
*   **Summary**: imperative mood, lowercase, no trailing period, 72 characters or fewer. It names the change in behavior: `fix(coverage): hold cells through gaps between flashes`, not `fix(coverage): update hold.go`.
*   **Body**: required unless the summary alone tells a reviewer everything. Wrap at 72 columns. State:
    *   **Why**: the problem, bug or need that prompted the change.
    *   **What**: the approach, and what now behaves differently.
    *   **Impact**: anything a reader must know: settings or bus protocol changes, follow-ups, trade-offs taken.

    Every sentence carries information a reviewer cannot get from the diff. A body with nothing to add is left out.
*   **Footers**: `BREAKING CHANGE: <what breaks and how to migrate>` for incompatible changes (and `!` after the type or scope); `Refs: #<issue>` or `Closes: #<issue>` for tracked work.
*   One logical change per commit.
*   No AI tools as author or co-author, and no attribution trailers or generated-by lines in commits or pull requests.

## Decisions

Decisions costly to reverse live in [`docs/adr/`](docs/adr/README.md). Any such decision (a dependency, a bus protocol change, a platform, packaging) gets a MADR record before implementation, per `docs/adr/README.md`.

## Boundaries

- Never modify psv, and never pin it to a commit or pseudo-version.
- Never commit `build/`, `dist/`, `*.app`, `go.work` or `go.work.sum`. Commit `go.sum`.
- Brian's approval is required for: any new dependency; changing the bus contract (entry points, kinds' meaning, the binary cover layout); changing default trigger thresholds or calm time; changing the bundle ID or signing identity; anything that would make psvguard's windows capturable by its own stream.
- Do not commit, push, create the GitHub remote or publish unless Brian asks.
