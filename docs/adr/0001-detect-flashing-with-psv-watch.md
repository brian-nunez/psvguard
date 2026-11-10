---
status: accepted
date: 2026-11-09
decision-makers: Brian Nunez
---

# Detect flashing with psv's Analyzer.Watch and own only the covering policy

## Context and Problem Statement

psvguard covers parts of the screen that start flashing. Whether a region of video flashes hazardously is the job of Brian's detector, [`github.com/brian-nunez/psv`](https://github.com/brian-nunez/psv), which implements WCAG 2.2 general and red flash detection and is tested against the Trace benchmark. Release `v0.2.0` added `Analyzer.Watch` ([psv ADR-0004](https://github.com/brian-nunez/psv/blob/v0.2.0/docs/adr/0004-watch-live-streams-with-psv-watch-v1.md)): a history-free, per-frame `psv.watch/v1` `Update` whose `Building` measurement (`Peak`, `Flashing`, `Area`, a 64×36 `Mask`) arrives in the frame flashing happens, about a second before `Decided` gives the final verdict.

How does psvguard get live flash information for every display without owning or forking detection logic, and which dependencies may it take on?

## Decision Drivers

* Detection results must be the ones psv produces and tests; psvguard must not drift from them.
* Covers must appear before a failure is decided, so the live signal has to arrive within a frame or two.
* Memory must stay flat for sessions lasting days.
* Small dependency surface: psvguard is a safety aid that runs all day on Brian's machine.
* psv must not be changed for psvguard's sake.

## Considered Options

* Depend on psv `v0.2.0` or later and use only `Analyzer.Watch`; psvguard owns the trigger and covering policy
* Use `Analyzer.Analyze` and act on decided failures
* Implement a simplified detector inside psvguard
* Vendor or fork psv's detector into psvguard

## Decision Outcome

Chosen option: "Depend on psv `v0.2.0` or later and use only `Analyzer.Watch`; psvguard owns the trigger and covering policy", because it is the only option that gives a signal in the frame flashing happens, with flat memory, while every detection result stays psv's own.

* `go.mod` requires a tagged psv release, `v0.2.0` or later, never a commit or pseudo-version. psvguard uses only psv's public API and never modifies psv.
* One `psv.Analyzer` per display, used only with `Watch` (mixing with `Analyze` returns `ErrMode`); `Stop` ends a stream, and a resolution change stops and recreates that display's analyser.
* psvguard decides **when and where to cover** from `Update.Building` only: hot `Mask` cells, margins, hold timers, rectangle merging and the full-display cut-over. It never re-derives transitions, flashes or region failures.
* `Update.Decided` is a backstop and a diagnostic: a decided failure that was not covered is counted as a miss.
* External dependencies are limited to the psv module and FFmpeg (a system tool, used by psv's file analysis in tests and verification; live capture does not need it). Everything else is the Go standard library, cgo and the platform's system frameworks. Any other dependency needs Brian's approval and its own ADR.

### Consequences

* Good, because detection fixes and threshold presets arrive by bumping one tagged dependency.
* Good, because `Building` reports flashing a full second before `Decided`, which is what makes covering before exposure possible.
* Good, because `Watch` keeps no history, so per-display memory is constant regardless of session length.
* Bad, because `Building` ignores the area threshold, so it also reports flashing too small to fail; psvguard's own thresholds (minimum hot share, early Peak) must filter it, and they need tuning against passing videos.
* Bad, because `Building` only reports pixels that transition in the current frame and reads zero between flashes, so psvguard must hold covers with its own calm timer.
* Neutral, because a future incompatible `psv.watch/v2` would require a deliberate upgrade and a new ADR here.

### Confirmation

* `go.mod` lists `github.com/brian-nunez/psv` at a tagged version `>= v0.2.0` and no other external module; review rejects anything else without an approved ADR.
* No package in psvguard calls `Analyze`; review and a grep check (`grep -rn '\.Analyze(' --include='*.go'` outside tests that compare against `psv.AnalyzeFile`) confirm it.
* An end-to-end test streams psv's example videos through the monitor and checks that failing clips are covered before their first decided failure and passing clips stay uncovered within the agreed tolerance.

## Pros and Cons of the Options

### Depend on psv `v0.2.0` or later and use only `Analyzer.Watch`; psvguard owns the trigger and covering policy

* Good, because results are psv's, verified frame by frame by psv's own conformance suite.
* Good, because the live signal is available in the frame, at about 1.5 ms per 640×360 frame on an Apple M3 Pro (psv's measurement).
* Good, because the split is clean: psv measures, psvguard decides what to cover.
* Bad, because psvguard's trigger policy is a second layer of thresholds that has to be tuned and explained.

### Use `Analyzer.Analyze` and act on decided failures

* Good, because only final, area-thresholded verdicts would trigger covers, so no false positives from small flashing.
* Bad, because every decision arrives `round(fps) − 1` frames late: the user has already seen a full failing second before anything is covered.
* Bad, because `Analysis().Frames` grows without bound in an all-day session.

### Implement a simplified detector inside psvguard

* Good, because it could be tuned specifically for covering.
* Bad, because it duplicates and diverges from tested detection logic, with no benchmark conformance.
* Bad, because every psv improvement would have to be re-implemented.

### Vendor or fork psv's detector into psvguard

* Good, because internals could be adapted freely.
* Bad, because the copy drifts from upstream and loses psv's tests and releases.
* Bad, because it breaks the rule that psv is only consumed through its public API.

## More Information

* psv `v0.2.0`: README "Library" section, `docs/how-it-works.md`, `watch.go`.
* Default trigger thresholds (early Peak, minimum hot share, full-display cut-over, calm time) are set from spike measurements against psv's passing and failing example videos, and are Brian's decision.
* Revisit if psv adds a region-aware `Watch` method that applies the area threshold live.
