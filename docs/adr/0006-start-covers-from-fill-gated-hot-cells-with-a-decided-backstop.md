---
status: proposed
date: 2026-11-09
decision-makers: Brian Nunez
---

# Start covers from fill-gated hot cells, with psv's decided results as a backstop

## Context and Problem Statement

PSV Guard covers flashing from psv's live `Update.Building` ([ADR-0001](0001-detect-flashing-with-psv-watch.md)). `Building` is pixel-level: its 64×36 `Mask` holds, for each cell, the most transitions within the last second of any pixel in it that transitions in the current frame. It applies no area threshold and has no transition direction; psv only applies those in `Update.Decided`, a second later.

The first policy, cover when cells with a peak of 4 or more (plus a one-cell margin) reach a minimum share of the display, failed in both directions during the spike on this Mac:

- psv's passing example video `video-ad-sample-2` was covered for its whole length: film grain and fine texture put a few high-count pixels in hundreds of cells.
- Scrolling text (fast, slow, dark mode), a sliding page and macOS's full-screen Space animation were covered within 70 ms to 1.2 s. psv passes all of them: their pixels transition often but out of step, so no region reaches 25% in one direction.

Which live rule, using `Building` only, covers flashing before psv's first failing frame without covering ordinary moving content?

## Decision Drivers

* Cover failing content before psv's first failing frame, and never let a decided failure pass uncovered for long.
* Do not cover ordinary use: scrolling, typing, video, window and Space changes.
* Use only `Building` and `Decided`; never reimplement detection.
* Cheap per frame and per display at 60 fps.
* Explainable defaults that are settings.

## Considered Options

* Fill-gated hot cells, a flashing keep-alive, and a decided backstop
* Hot cells with a minimum area only
* Hot cells plus a density test of the transitioning share against the hot share
* Cover only from `Flashing` (pixels already past the limit) or only from `Decided`

## Decision Outcome

Chosen option: "Fill-gated hot cells, a flashing keep-alive, and a decided backstop", because it is the only option tested that covers every failing clip before its first failing frame with no misses while leaving every passing and moving-text clip uncovered.

* A cell is **hot** at 4 or more transitions within a second.
* **Fill** of a frame = the share of the display transitioning in it ÷ the share of cells with any transition in it.
* **Start** covering when hot cells cover at least 15% of the display and, within the last second, the fill reached 0.5 in at least 2 frames and in at least 30% of the frames with any transition (counting at least 8).
* **System animations** (a transition event from the platform) withdraw covers begun in the last second, unless forced by a decided failure, and hold off new ones for 0.6 s.
* **Keep** an existing cover while hot cells continue and either the fill test still passes or psv reports at least 1% of the display already flashing; keeping only refreshes cells covered already or next to the cover.
* **Hold** each covered cell for 3 s after it was last hot; cover the whole display above 40% covered.
* **Backstop:** a decided failure whose frame was not covered is counted as a miss, and its decided mask is covered immediately.

### Consequences

* Good, because on psv's examples the failing videos are covered 0.17 s, 0.23 s and 2.8 s before their first failing frame with no misses, and the passing video stays uncovered; synthetic full-screen flashing is covered 0.28 s before its first failing frame, and fast, slow and dark-mode scrolling text and a sliding page stay uncovered.
* Good, because live, on this Mac, full-screen flashing is covered 323–352 ms after it first appears, 6–35 ms after the triggering frame; full-screen enter and exit animations are withdrawn within about 0.1 s, before the cover's fade-in completes.
* Bad, because flashing smaller than 15% of the display, or flashing whose fill stays low from the start, is only covered by the backstop, about a second after its first failing frame.
* Bad, because the defaults rest on four real videos and five synthetic clips; other content may need tuning.
* Neutral, because each threshold is a setting and `TestTuneThresholds` and `TestClip` re-check them.

### Confirmation

* `TestExampleVideos` and `TestSyntheticClips` in `internal/monitor/e2e_test.go` run in `make test` and fail if a failing clip is covered late or missed, or a passing clip is covered.
* `internal/coverage` unit tests cover fill, keep-alive without spreading, hold, scene changes, scrolling-like input and `Force`.

## Pros and Cons of the Options

### Fill-gated hot cells, a flashing keep-alive, and a decided backstop

* Good, because fill measures what tells flashing apart from moving detail in `Building`: flashing switches whole areas (fill 0.46–1.0 in the failing examples), moving detail switches a fraction of each cell (0.25–0.37 for scrolling text, with occasional frames up to 0.55 when the capture skips a step, which the share requirement absorbs).
* Good, because the keep-alive catches patterned flashing once a cover is up (porygon's later segments, fill about 0.25 with 10% of the display flashing).
* Bad, because a flashing object that starts with low fill waits for the backstop.

### Hot cells with a minimum area only

* Good, because it is the simplest rule and the earliest.
* Bad, because no threshold tested (peak 4–6, area 2–30%) kept the passing example uncovered, and scrolling text reaches 85% hot cells.

### Hot cells plus a density test of the transitioning share against the hot share

* Good, because it removed most of the film-grain covers.
* Bad, because while hot cells are still ramping up the ratio looks dense, so scrolling text and scene-cut sequences still started covers.

### Cover only from `Flashing` or only from `Decided`

* Good, because both follow psv's own definitions more closely.
* Bad, because `Flashing` is also high for scrolling text (20–26% of the display), and `Decided` arrives a second after the first failing frame, so the viewer has already seen it.

## More Information

* Data and simulations: `TestTuneThresholds` and `TestClip` in `internal/monitor/e2e_test.go`; the policy is described in `docs/architecture.md` ("Covering policy").
* Revisit if psv adds a live measurement with direction or region area, or when new content shows false covers or late covers.
