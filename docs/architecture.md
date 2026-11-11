# Architecture

PSV Guard captures every display, watches each one with [psv](https://github.com/brian-nunez/psv)'s
live `Analyzer.Watch`, and covers the parts of the screen where flashing builds up. All logic is
Go. The platform layer (on macOS, Objective-C in cgo preambles) only executes commands and reports
events.

Decisions that shape this design are recorded in [`docs/adr/`](adr/README.md).

## Design constraints

- **psv measures, PSV Guard decides.** Detection is psv's, through its public API and only
  `Watch`. PSV Guard owns the covering policy: which grid cells count, when to start, how long
  to hold, what rectangles to draw ([ADR-0001](adr/0001-detect-flashing-with-psv-watch.md)).
- **Go first.** State, settings, resampling, frame conversion, the trigger rules and every cover
  rectangle and fade are decided in Go ([ADR-0002](adr/0002-keep-logic-in-go-with-objective-c-only-in-cgo-preambles.md)).
- **One-way, asynchronous bridge.** Events in, commands out; nothing waits for the other side
  ([ADR-0003](adr/0003-use-a-one-way-event-and-command-bus-as-the-platform-contract.md)).
- **Platform-neutral core.** Only the bus knows the platform. Everything else builds, vets and
  tests on Linux and without cgo ([ADR-0004](adr/0004-keep-the-core-platform-neutral-and-add-linux-as-a-second-bus.md)).
- **Covers never become a stimulus.** They fade in over 0.15 s and out over 1 s, and hold for a
  calm time, so they change brightness at most twice in several seconds.

## Pipeline

```
ScreenCaptureKit ──BGRA, ~640 px wide, on change──▶ goCaptureFrame ──one pass──▶ pooled psv.Frame
                                                                                   │ size-1 hand-off
                       per display goroutine ◀────────────────────────────────────┘ (older frame dropped)
                       │
                       ├─ resample to a fixed 60 fps (newest frame, or the previous one again)
                       ├─ psv Analyzer.Watch ──▶ Update.Building (live)  ──▶ coverage.Tracker ──▶ cover rects
                       │                     └─▶ Update.Decided (1 s later) ─▶ misses, backstop
                       └─ Report (only when the covers change) ──▶ controller ──▶ Outbox ──▶ pg_post
```

1. **Capture.** One ScreenCaptureKit stream per display, scaled by the GPU to about 640 px wide
   with the display's aspect ratio, in BGRA and sRGB. The filter excludes PSV Guard's own
   windows, so covers never reach the analysis.
2. **Hand-off.** `goCaptureFrame` runs on the capture queue while the pixel buffer is locked. It
   converts the buffer straight into a pooled `psv.Frame` (honouring row padding) and puts it in a
   channel of size one, replacing a frame analysis has not taken yet. No pooled frame free means
   the new frame is dropped. Nothing is allocated per frame and no C pointer is kept.
3. **Resampling.** ScreenCaptureKit sends frames only when the screen changes; psv assumes a fixed
   rate. Each display's goroutine ticks at 60 fps and watches the newest frame, or the previous
   one again. Frames older than the one held are dropped by presentation time.
4. **Watching.** One `psv.Analyzer` per display, used only with `Watch`. Region sizes in the
   threshold preset are given in reference pixels (points), so they are scaled to analysed pixels:
   a WCAG 2.2 region of 341×256 points is 126×95 pixels on a 1728-point-wide display analysed at
   640 px.
5. **Deciding.** `coverage.Tracker` turns each `Update.Building` into covered grid cells (below).
6. **Reporting.** The monitor reports only changes of the cover rectangles. The controller posts
   them; the outbox drops repeats and coalesces per display.

## Covering policy

psv's `Building.Mask` is a 64×36 grid; each cell holds the highest transition count, within the
last second, of its pixels that transition in the current frame.

| Rule | Default | Why |
| --- | --- | --- |
| A cell is **hot** at this many transitions within a second | 4 | Two flashes; WCAG fails above 6, so covers come before failure |
| Hot cells must cover at least this share of the display to **start** covering | 15% | Small blinking elements stay uncovered |
| **Fill** must reach this value in at least 2 frames, and in at least 30% of the frames with any transition (of at least 8), within the last second | 0.5 | Tells flashing from moving detail (below) |
| A covered cell is **held** this long after it was last hot | 3 s | Bridges the gaps between flashes |
| An existing cover is **kept** while psv reports at least this share already flashing | 1% | Patterned flashing whose fill is low |
| Above this covered share, cover the **whole display** | 40% | Fewer, simpler covers when most of the screen flashes |
| A **margin** of cells is added around hot cells | 1 cell | Covers the edges psv's grid rounds off |

**Fill.** A frame's fill is the share of the display transitioning in it divided by the share of
grid cells with any transition in it. Flashing switches whole areas at once, so its fill is close
to 1 (0.46 to 1.0 in psv's failing example videos). Scrolling or sliding text, film grain and fine
moving texture make many pixels transition just as often, but only a fraction of the pixels in
each cell at a time, so their fill stays low (0.25 to 0.37 for scrolling text). When the capture
skips a step, moving text briefly reaches 0.46 to 0.55, so the share of filled frames matters as
well as their number: flashing fills most of its transitioning frames, moving text a few. psv itself tells
them apart in its decided results by requiring a quarter of a region to flash in the same
direction in the same frame; `Building` has no direction, and fill is the closest live signal.

**Keeping without spreading.** While a cover is up, psv's `Flashing` share keeps it alive even when
the fill is low, but only cells covered already or next to the cover are refreshed, so a cover
cannot spread across scrolling text elsewhere.

**System animations.** Switching Spaces and entering or leaving full screen slide the whole
display and can pass the fill test. The platform reports them as a transition event (on macOS,
`NSWorkspaceActiveSpaceDidChangeNotification`, which arrives about 0.1 s after the animation
starts). The tracker then withdraws covers begun within the last second, unless a decided failure
forced them, forgets the frames seen so far, and starts no cover for 0.6 s. Such a cover is gone
before its 0.15 s fade-in completes; flashing that continues is covered again after the quiet
time.

**Backstop.** Every decided failure is checked against whether its frame was covered when it was
watched. One that was not is counted as a miss and its flashing cells (from the decided result's
mask) are covered at once.

**Without capture exclusion** (`excludesOwnWindows: false`), covered cells are ignored when finding
hot cells, since the capture shows the cover rather than the content. A covered area whose
uncovered surroundings flash is refreshed whole, so it does not develop holes.

The defaults were chosen against psv's four example videos and synthetic clips (see
[Testing](#testing)); all of them are settings.

## Code layout

```
cmd/psvguard/         main.go: flags, settings, wiring; gets the Bus from internal/bridge
internal/app/         controller: events in, commands out; one monitor per display; tested with a fake Bus
internal/bridge/      the platform contract: event and command kinds, payload codecs, Outbox, EventQueue
                      bus_darwin.go     the cgo bridge: goEvent, goCaptureFrame, pg_post wrappers
                      native_darwin.go  all Objective-C: menu bar, panel, ScreenCaptureKit, cover panels
                      bus_other.go      "not yet supported on this platform"
internal/monitor/     one display's analysis: Pipeline (resample → Watch → coverage) and Display (goroutine + fast path)
internal/coverage/    the covering policy: hot cells, fill, hold, merging, full-display cut-over, grid → normalised rects
internal/resample/    fixed-rate resampler
internal/frame/       BGRA/RGBA/BGRx/RGBx → psv.Frame conversion and the frame pool
internal/settings/    option table: defaults, validation, flags, JSON file under os.UserConfigDir()/psvguard
internal/ui/          UI view models and actions, sent as JSON through the bridge
packaging/macos/      Info.plist template for the .app bundle
```

`bus_darwin.go` and `native_darwin.go` are split because cgo forbids C definitions in a preamble
that sits next to `//export`.

## Bus protocol

Three entry points carry everything:

| Direction | Entry point | Carries |
| --- | --- | --- |
| native → Go | `void goEvent(int kind, const void *data, size_t len)` | events, JSON |
| native → Go | `void goCaptureFrame(uintptr_t handle, int pixelFormat, const uint8_t *base, int width, int height, int bytesPerRow, int64_t ptsNanos)` | frames |
| Go → native | `void pg_post(int kind, const void *data, size_t len)` | commands, JSON or binary |

Events (`EventKind` in `internal/bridge/contract.go`, mirrored in `native_darwin.go`):

| Kind | Payload |
| --- | --- |
| 1 capabilities | `{"excludesOwnWindows","canOverlay","perDisplayCapture","consentPerSession","pixelFormats"}` |
| 2 ready | none |
| 3 action | `{"kind":"toggle"|"setting"|"resetSettings"|"requestPermission"|"openPermission"|"quit","key","value"}` |
| 4 displays | `[{"id","name","width","height","pixelWidth","pixelHeight","main"}]`, sizes in points |
| 5 permission | `{"state":"granted"|"denied"|"needsConsent"|"unknown"}` |
| 6 streamStarted, 7 streamStopped, 8 streamFailed | `{"stream","display","error"}`; a stopped stream's frame target is released |
| 9 power | `{"sleeping":bool}` |
| 10 coversShown | `{"display","rects","at"}`, `at` in nanoseconds on the frame clock |
| 11 transition | `{"at"}`: a system animation moved whole displays (workspace switch, full screen) |

Commands (`CommandKind`):

| Kind | Payload |
| --- | --- |
| 1 setCovers | binary, below |
| 2 clearCovers | `{"display"}`; empty clears every display |
| 3 status, 4 controls, 5 stats | `ui.Status`, `[]ui.Control`, `ui.Stats` |
| 6 startCapture | `{"stream","display","width","height","fps","format","handle"}` |
| 7 stopCapture | `{"stream","display"}` |
| 8 checkPermission, 9 requestPermission | none; answered by a permission event |
| 10 openPermissionPane, 12 showPanel, 13 quit | none |
| 11 notify | `{"title","body"}` |

The cover layout, little-endian: `u16` display ID length, the ID bytes, `f64` fade-in seconds,
`f64` fade-out seconds, `u32` rectangle count, then per rectangle `x, y, w, h` as `f64` in
normalised display coordinates (0 to 1, origin top left). The platform converts them to its own
coordinates; on macOS `pg_norm_to_points` multiplies by the display's size in points, flips the
y axis, and rounds outward to device pixels.

Rules that keep the bridge safe:

- Callbacks only decode or copy and enqueue. `goEvent` pushes onto an unbounded queue that never
  blocks; a single goroutine forwards events to the controller in order.
- Go never calls `pg_post` from a callback. One goroutine, the outbox, owns every `pg_post`: it
  sends only the latest cover set per display and only when it changed, and throttles status to
  four updates a second and stats to one.
- `pg_post` copies the payload into an `NSData` and dispatches to the main queue, where one switch
  applies the command. Native handlers never call into Go while applying a command; outcomes are
  emitted later through a serial event queue.
- Each stream's frame target is a `runtime/cgo.Handle`, created when the start command is posted
  and deleted when the stream reports it stopped. The stopped event is emitted only after the
  stream's serial sample queue drains, so no frame callback can outlive its handle.

`--trace-bridge` logs every event and command with a timestamp.

## UI contract

The controller sends `ui.Status` (state, title, detail, whether protection is on, whether to show
the permission button, the disclaimer, the version), `[]ui.Control` (settings from the option
table) and `ui.Stats` (per-display frame rate, time per `Watch`, hot area, covers, dropped and
repeated frames, misses). The UI reports `ui.Action`s. The native side renders whatever it gets and
never interprets it.

On macOS the menu bar item is an `NSStatusItem` (left click toggles the panel, right click shows a
menu with the status, the on/off toggle and Quit). The panel is a non-activating `NSPanel` on all
Spaces that never steals focus. There is no Dock icon. Cover windows are borderless, click-through
panels at screen-saver level on every Space and over full-screen apps; they never become key and
hold one layer per rectangle, whose opacity Core Animation fades with the durations in the command.

## Threads

| Thread | Work |
| --- | --- |
| main (AppKit run loop) | status item, panel, cover layers, applying commands, starting and stopping streams |
| ScreenCaptureKit sample queue (one per stream) | `goCaptureFrame`: one conversion into a pooled frame |
| serial event queue | calls `goEvent` |
| one goroutine per display | resampling, `Watch`, coverage |
| controller goroutine | events, reports, settings, status |
| outbox goroutine | every `pg_post` |

Neither the main thread nor a capture queue ever blocks on Go.

## Lifecycle

- **Permission.** On ready the controller checks capture permission. When consent is needed it asks
  once per session; while not granted it checks again every two seconds and shows a button that
  opens the system's permission settings. A failed stream also triggers a check, which is how a
  revoked permission is noticed.
- **Displays.** Every displays event is reconciled: new displays start a stream, removed ones stop,
  and a display whose size changed gets a new stream and a new analyser (psv's analyser has a
  fixed frame size).
- **Sleep and wake.** Streams stop on sleep; on wake the permission is checked and streams restart.
- **Settings.** Policy settings apply to running monitors at once; the threshold preset, frame rate
  and capture width restart the streams.
- **Quit.** Streams stop, every cover is cleared, then the run loop ends.

## Adding a platform

A new OS needs one bus: a `bridge.New` behind a build tag (for Linux, `bus_linux.go`, with
`bus_other.go`'s tag narrowed to exclude it) that

1. sends a capabilities event first, then displays, a permission state and ready;
2. delivers frames as `FrameTarget.CaptureFrame` calls in one of the `frame.Format`s, from the
   handle in each start-capture command, and reports stream started, failed and stopped, releasing
   the handle only after the last frame callback has returned;
3. applies the commands, converting normalised top-left rectangles to its own coordinates;
4. owns the process's main thread in `Run` if its toolkit needs it.

Nothing else changes. The controller already handles capture that includes the covers
(`excludesOwnWindows: false`), no overlays (`canOverlay: false`, warnings and a notification
instead) and consent every session (`consentPerSession: true`); its tests run under those
capabilities. [ADR-0004](adr/0004-keep-the-core-platform-neutral-and-add-linux-as-a-second-bus.md)
lists what X11 and Wayland will need. Packaging gets its own Makefile target (for Linux, a binary
and a `.desktop` file).

## Settings and data

Settings resolve default, then saved, then flag. They are saved as JSON in
`os.UserConfigDir()/psvguard/settings.json`: `~/Library/Application Support/psvguard` on macOS and
`~/.config/psvguard` on Linux. Nothing else is stored.

## Performance

Measured on this project's development Mac (Apple M1-class GPU, built-in 1728×1117-point display
analysed at 640×414, 60 fps):

| Measure | Value |
| --- | --- |
| Cover applied after the first flash on screen | 323–332 ms (floor: 300 ms, the fourth transition at 5 flashes/s) |
| Cover applied after the frame that triggered it | 15–24 ms, then a 150 ms fade-in |
| Lead before psv's first failing frame / decided failure | about 270 ms / 1.27 s (5 flashes/s, full screen) |
| Time per `Watch` | 1.5–2.5 ms on ordinary content, about 4.5 ms with the whole screen flashing |
| Capture callback (conversion into a pooled frame) | 0.55–1.0 ms mean live, 0.2 ms in a warm benchmark |
| CPU, one display, ordinary use (60 fps, 640 px) | 61–85% of one core, mostly psv's `Watch` (2.8 ms of CPU per static frame, spread over cores) |
| Memory | 140–320 MB resident, flat over 10 minutes |

psv's decided-region search allocates about 3.5 MB per frame while a whole 640×414 frame flashes
(about 14 KB per frame otherwise). The memory is garbage at once, so PSV Guard sets a 256 MiB soft
memory limit (`GOMEMLIMIT` overrides it) to keep the heap from growing to many times its live
size, about 50 MB per display.

## Testing

- Pure-Go unit tests for every package, run under `-race`, on Linux builds and with
  `CGO_ENABLED=0` (`make test-portable`).
- `internal/app` tests the controller with a fake bus under macOS-like and Linux-like
  capabilities: permission flow, hot-plug and resolution change, covering and uncovering, pause,
  settings, sleep and wake, quit, warnings without overlays, and covers that cannot sustain
  themselves without capture exclusion.
- `internal/monitor` streams psv's example videos (from the psv module in the Go module cache,
  decoded by FFmpeg at the capture size) through the pipeline at 60 fps: failing videos must be
  covered before psv's first failing frame with no misses, the passing video must stay uncovered,
  and failures decided at 640 px must match psv's native-resolution reports within 100 ms.
  Synthetic clips generated by FFmpeg add full-screen flashing (covered before failing) and fast,
  slow and dark-mode scrolling text and a sliding page (never covered). Failing videos must have
  no misses.
- `internal/bridge` tests the payloads, the binary cover layout, the outbox and the event queue;
  on macOS also `pg_norm_to_points` at 1× and 2× and on a second display, and a smoke test
  (`make smoke`, needs Screen Recording permission) that captures the main display, shows a cover
  and stops cleanly.
- `PSVGUARD_TUNE=1 go test ./internal/monitor -run TestTuneThresholds -v` sweeps thresholds over
  the example videos; `PSVGUARD_CLIP=<file> go test ./internal/monitor -run TestClip -v` checks
  any video.

## Not supported

- Spatial patterns (stripes, checkerboards); psv does not detect them.
- Flashing smaller than the minimum area, or whose fill stays low before psv decides it fails:
  the decided backstop covers it about a second after its first failing frame.
- Platforms other than macOS (Linux is designed for but not implemented).
- Distribution, notarisation, auto-update and launch at login.
