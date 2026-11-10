---
status: accepted
date: 2026-11-09
decision-makers: Brian Nunez
---

# Use a one-way, asynchronous event and command bus as the platform contract

## Context and Problem Statement

psvguard's Go core must exchange three kinds of traffic with native code: captured frames (up to 60 per second per display, about 640 px wide), low-rate events (UI actions, display changes, permission state, stream lifecycle) and commands (show or clear covers, update the menu bar, start or stop capture, quit). Native callbacks run on the main thread and on ScreenCaptureKit's capture queue, neither of which may block. bvoice uses one bridge function per purpose; psvguard has more traffic and a second platform coming.

What shape does the Go ↔ native boundary take so it is fast on the frame path, never blocks either side, and can be reimplemented for Linux unchanged?

## Decision Drivers

* Never block the main thread or the capture queue; no call across the bridge waits for the other side.
* Frames cross once, with no intermediate copy and no per-frame allocation.
* Native code makes no decisions and never sees frame data, psv results or analysis state.
* The contract contains no macOS concepts, so a Linux implementation fits it as is.
* The controller is fully testable with a fake.
* With nothing flashing, Go sends native code nothing about frames.

## Considered Options

* One event entry point and one command entry point, plus a frame fast path; asynchronous both ways
* Per-purpose bridge functions, as in bvoice
* Synchronous calls that return results across the bridge
* Pass every frame's analysis result to native code and let it draw covers

## Decision Outcome

Chosen option: "One event entry point and one command entry point, plus a frame fast path; asynchronous both ways", because it keeps the bridge to three functions, rules out round trips by construction, and turns the platform boundary into a small, pure-Go protocol a fake or a Linux layer can implement.

The whole bridge surface:

| Direction | Entry point | Carries |
| --- | --- | --- |
| native → Go | `void goEvent(int kind, const void *data, size_t len)` | capabilities, UI actions, app ready, display list changed, permission state, stream started/stopped/failed |
| native → Go (fast path) | `void goCaptureFrame(uintptr_t handle, int pixelFormat, const uint8_t *base, int width, int height, int bytesPerRow, int64_t ptsNanos)` | frames only |
| Go → native | `void pg_post(int kind, const void *data, size_t len)` | set or clear covers, UI status, controls and stats, start or stop capture for a display, check or request permission, open the permission settings, notify, quit |

Rules:

1. **No round trips.** Go never calls `pg_post` from inside an exported callback; native handlers never call into Go while applying a command. A query is a command plus an answering event (`checkPermission` → `permissionState`).
2. **Callbacks return immediately.** `goEvent` copies its payload onto the controller's event channel. `goCaptureFrame` converts the locked buffer, honouring `bytesPerRow` padding, directly into a pooled `psv.Frame` in one pass, hands it to that display's analysis goroutine through a size-1 channel that drops the older frame when analysis is behind, and returns. With no pooled frame free, it drops the incoming frame instead of allocating. Neither keeps C pointers.
3. **One outbound goroutine** owns every `pg_post` call. It coalesces (latest cover set per display, latest status), posts covers only when they change, and throttles status to a few updates per second.
4. **`pg_post` copies and returns.** Native code copies the payload and applies it asynchronously on the thread the platform requires.
5. **Per-display handles** are `runtime/cgo.Handle` values, passed in the start-capture command and on every frame, deleted when the stream reports it stopped.
6. **Kinds** are int constants defined in `internal/bridge` and mirrored in native code with a "must match" comment.
7. **Payloads:** JSON for low-rate kinds. Covers use a documented packed little-endian layout: display ID (length-prefixed string), fade-in and fade-out seconds, rectangle count, then `x, y, w, h` as float64 in normalised display coordinates (0–1, origin top left). Raw pixels only on the frame path.
8. **Platform-neutral vocabulary:** display IDs are opaque strings, geometry is normalised with a top-left origin, pixel formats are an enum (BGRA today; RGBA, BGRx, RGBx reserved), and permission is `granted`, `denied`, `needsConsent` or `unknown`.

In Go, the controller reads one event channel and writes one command channel behind a small `Bus` interface. The darwin `Bus` is the cgo bridge; tests use a fake bus that injects events and records commands.

### Consequences

* Good, because neither side can deadlock the other: there is no call that waits.
* Good, because in normal use (nothing flashing) the only traffic is frames inbound and occasional status outbound.
* Good, because the entire controller, including permission flow, hot-plug, covering, pause and quit, is tested without macOS.
* Good, because a Linux layer implements the same three entry points with different native code.
* Bad, because request/response interactions become event pairs, which is more code than a direct call.
* Bad, because the binary cover layout is a hand-maintained format; it needs its own encoder/decoder tests on both sides.
* Bad, because frame drops under load are by design; they must be counted and surfaced in stats so they are visible.

### Confirmation

* Pure-Go unit tests for every JSON payload and for the binary cover layout round trip.
* Controller tests run against the fake bus, including under Linux-like capabilities.
* A `--trace-bridge` flag logs every event and command with a timestamp for manual inspection.
* A darwin-only smoke test (skipped without Screen Recording permission) starts a stream, receives frames, posts a cover and stops cleanly.
* Review checks that no exported callback calls `pg_post`, no native command handler calls into Go, and nothing outside `bus_darwin.go` refers to macOS.

## Pros and Cons of the Options

### One event entry point and one command entry point, plus a frame fast path; asynchronous both ways

* Good, because the surface is three functions and two int enums.
* Good, because ordering is guaranteed by a single outbound goroutine.
* Neutral, because adding a capability means adding a kind and a payload rather than a function.
* Bad, because payload decoding replaces typed function signatures, so type errors move from compile time to tests.

### Per-purpose bridge functions, as in bvoice

* Good, because each call is explicit and typed at the C level.
* Bad, because the surface grows with every feature and each function must be reimplemented per platform.
* Bad, because nothing structurally prevents synchronous or re-entrant calls.

### Synchronous calls that return results across the bridge

* Good, because simple queries read naturally.
* Bad, because a Go call that waits on the main thread, or a main-thread call that waits on Go, risks deadlock and frame stalls.

### Pass every frame's analysis result to native code and let it draw covers

* Good, because Go would need no cover diffing.
* Bad, because decisions leak into native code, which then has to be duplicated for Linux.
* Bad, because it posts at frame rate even when nothing flashes.

## More Information

* Capabilities and the Linux plan: [ADR-0004](0004-keep-the-core-platform-neutral-and-add-linux-as-a-second-bus.md).
* Language and file conventions for the native layer: [ADR-0002](0002-keep-logic-in-go-with-objective-c-only-in-cgo-preambles.md).
* The authoritative list of kinds and payload layouts lives in `internal/bridge` and `docs/architecture.md`; this ADR fixes the shape and rules, not each kind.
