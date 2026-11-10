// Package coverage decides which parts of a display to cover from psv's live measurement. It owns
// psvguard's covering policy: hot cells, margins, hold timers, rectangle merging and the switch to
// covering the whole display. It never measures flashing itself; that is psv's job.
package coverage

import (
	"time"

	"github.com/brian-nunez/psv"
)

// Grid size: psv's mask grid.
const (
	Columns = psv.MaskColumns
	Rows    = psv.MaskRows
	cells   = Columns * Rows
)

// Rect is an area of a display in normalised coordinates: 0 to 1 across the display, origin at its
// top left.
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Full is the whole display.
var Full = Rect{X: 0, Y: 0, W: 1, H: 1}

// Params is the covering policy.
type Params struct {
	// HotPeak is the transitions within the last second at which a grid cell is hot. psv's
	// WCAG 2.2 limit is 6, so 4 (two flashes) acts before flashing fails.
	HotPeak int
	// Margin is the number of cells added around hot cells.
	Margin int
	// MinHotArea is the share of the display, in percent, that hot cells must cover before
	// covering starts.
	MinHotArea float64
	// MinFill and DenseFrames tell flashing apart from other content that makes pixels transition
	// often: scrolling or sliding text, film grain, fine moving texture. A frame's fill is the
	// share of the display transitioning in it divided by the share of grid cells with any
	// transition in it. Flashing switches whole areas at once, so its fill is high; moving
	// detail switches a fraction of the pixels in many cells. Covering starts only when, within the
	// last second, at least DenseFrames frames, and at least DenseShare of the frames with any
	// transition, reached MinFill.
	MinFill     float64
	DenseFrames int
	// DenseShare is the least share of the last second's transitioning frames that must reach
	// MinFill.
	DenseShare float64
	// FlashingArea keeps an existing cover up, without the fill test, while psv reports at least
	// this share of the display, in percent, already flashing.
	FlashingArea float64
	// FullArea switches to covering the whole display when the covered share, in percent, is at
	// least this.
	FullArea float64
	// Calm is how long a cell stays covered after it was last hot.
	Calm time.Duration
	// MaxRects is the most rectangles a decision may hold; more are merged.
	MaxRects int
	// ExcludesOwnWindows reports that captured frames leave out the covers. When false, covered
	// cells are ignored when finding hot cells, so the covers cannot feed back into the decision.
	ExcludesOwnWindows bool
}

// DefaultParams returns the default policy.
func DefaultParams() Params {
	return Params{
		HotPeak:            4,
		Margin:             1,
		MinHotArea:         15,
		MinFill:            0.5,
		DenseFrames:        2,
		DenseShare:         0.3,
		FlashingArea:       1,
		FullArea:           40,
		Calm:               3 * time.Second,
		MaxRects:           8,
		ExcludesOwnWindows: true,
	}
}

// Decision is what to cover after one frame.
type Decision struct {
	// Rects are the areas to cover, merged; empty when nothing is covered.
	Rects []Rect
	// Full reports that the whole display is covered.
	Full bool
	// HotArea is the share of the display, in percent, of this frame's hot cells and margin.
	HotArea float64
	// CoveredArea is the share of the display, in percent, of covered cells.
	CoveredArea float64
	// Peak is the higher of psv's luminance and red Peak for the frame.
	Peak int
	// Fill is the frame's fill: the share of the display transitioning divided by the share of
	// cells with any transition.
	Fill float64
}

// Covering reports whether anything is covered.
func (d Decision) Covering() bool { return len(d.Rects) > 0 }

// Tracker applies the policy to one display's stream of psv updates. It is not safe for concurrent
// use.
type Tracker struct {
	p Params

	lastHot [cells]time.Duration
	seen    [cells]bool
	covered [cells]bool
	nCov    int

	// since is when each covered cell's cover began, and forced marks covers begun by Force.
	since  [cells]time.Duration
	forced [cells]bool
	// quietUntil disables starting covers until then.
	quietUntil time.Duration

	// fills is a ring of the fill of the recent frames.
	fills  [fillRing]sample
	head   int
	filled int
}

// sample is the fill of the frame seen at t.
type sample struct {
	t    time.Duration
	fill float64
}

// fillRing holds a second of frames at up to 240 fps.
const fillRing = 256

// dense reports whether, within the second before now, at least DenseFrames frames had a fill of at
// least MinFill and those frames were at least DenseShare of the frames with any transition.
// Flashing fills most of its transitioning frames; moving detail only occasionally, when the
// capture skips a step.
func (t *Tracker) dense(now time.Duration) bool {
	if t.p.DenseFrames < 1 {
		return true
	}

	full, moving := 0, 0

	for i := 1; i <= t.filled; i++ {
		smp := t.fills[(t.head-i+fillRing)%fillRing]
		if now-smp.t >= time.Second {
			break
		}

		if smp.fill <= 0 {
			continue
		}

		moving++

		if smp.fill >= t.p.MinFill {
			full++
		}
	}

	// A few frames right after a quiet spell are too few to judge a share by.
	return full >= t.p.DenseFrames && float64(full) >= t.p.DenseShare*float64(max(moving, minShareFrames))
}

// minShareFrames is the least number of frames DenseShare is taken of.
const minShareFrames = 8

// NewTracker returns a tracker for p.
func NewTracker(p Params) *Tracker {
	if p.MaxRects < 1 {
		p.MaxRects = 1
	}

	return &Tracker{p: p}
}

// Params returns the policy in use.
func (t *Tracker) Params() Params { return t.p }

// SetParams changes the policy and keeps the covered cells and their timers.
func (t *Tracker) SetParams(p Params) {
	if p.MaxRects < 1 {
		p.MaxRects = 1
	}

	t.p = p
}

// Reset uncovers everything and forgets every timer.
func (t *Tracker) Reset() { *t = Tracker{p: t.p} }

// Update applies psv's live measurement of the frame seen at now and returns what to cover.
func (t *Tracker) Update(now time.Duration, b psv.Building) Decision {
	var hot [cells]bool

	nHot, nActive := 0, 0

	for i := range cells {
		v := cellPeak(b.Luminance.Mask, i)
		if r := cellPeak(b.Red.Mask, i); r > v {
			v = r
		}

		if v > 0 {
			nActive++
		}

		if int(v) < t.p.HotPeak || t.p.HotPeak < 1 {
			continue
		}

		// Without exclusion, covered cells show the cover itself, not the content beneath it.
		if !t.p.ExcludesOwnWindows && t.covered[i] {
			continue
		}

		hot[i] = true
		nHot++
	}

	fill := 0.0
	if nActive > 0 {
		fill = max(b.Luminance.Area, b.Red.Area) / percent(nActive)
	}

	t.fills[t.head] = sample{t: now, fill: fill}
	t.head = (t.head + 1) % fillRing
	t.filled = min(t.filled+1, fillRing)

	var grown [cells]bool

	nGrown := 0
	if nHot > 0 {
		nGrown = dilate(&hot, &grown, t.p.Margin)
	}

	dense := t.dense(now)
	flashing := max(b.Luminance.Flashing, b.Red.Flashing)

	// Covering starts when enough of the display builds up flashing that fills its area. An
	// existing cover also stays while psv reports enough of the display already flashing, but
	// may then only grow into its neighbourhood, so it cannot spread across moving detail.
	start := nHot > 0 && percent(nHot) >= t.p.MinHotArea && dense && now >= t.quietUntil
	keep := nHot > 0 && t.nCov > 0 && (dense || (flashing > 0 && flashing >= t.p.FlashingArea))

	switch {
	case start:
		t.mark(&grown, now)
	case keep:
		t.markNear(&grown, now)
	}

	if (start || keep) && !t.p.ExcludesOwnWindows {
		t.refreshTouched(&grown, now)
	}

	d := t.decide(now, percent(nGrown), max(b.Luminance.Peak, b.Red.Peak))
	d.Fill = fill

	return d
}

// Force covers the cells set in mask, a MaskColumns×MaskRows grid such as psv's Flashes.Mask of a
// decided failure, with the margin, from now. It is the backstop for flashing the live policy did
// not cover.
func (t *Tracker) Force(now time.Duration, mask []byte) {
	var set, grown [cells]bool

	n := 0

	for i := range cells {
		if cellPeak(mask, i) > 0 {
			set[i] = true
			n++
		}
	}

	if n == 0 {
		return
	}

	dilate(&set, &grown, t.p.Margin)
	t.mark(&grown, now)

	for i := range cells {
		if grown[i] {
			t.forced[i] = true
		}
	}
}

// Transition handles a system animation that moved the whole display, such as switching
// workspaces: the animation can look like flashing, so covers begun within window before now are
// withdrawn, unless a decided failure forced them, the frames seen so far stop counting toward the
// fill test, and no cover starts for quiet after now. Real flashing that continues is covered
// again once the quiet time ends.
func (t *Tracker) Transition(now, window, quiet time.Duration) {
	for i := range cells {
		if t.covered[i] && !t.forced[i] && t.since[i] >= now-window {
			t.seen[i] = false
		}
	}

	t.quietUntil = now + quiet

	// The animation's own frames must not count toward starting a cover afterwards.
	t.filled = 0
}

func (t *Tracker) mark(cellsSet *[cells]bool, now time.Duration) {
	for i := range cells {
		if cellsSet[i] {
			t.lastHot[i], t.seen[i] = now, true
		}
	}
}

// markNear marks the set cells that are covered or within the margin of a covered cell.
func (t *Tracker) markNear(cellsSet *[cells]bool, now time.Duration) {
	m := max(t.p.Margin, 1)

	for y := range Rows {
		for x := range Columns {
			i := y*Columns + x
			if !cellsSet[i] {
				continue
			}

			near := false

			for yy := max(0, y-m); yy <= min(Rows-1, y+m) && !near; yy++ {
				for xx := max(0, x-m); xx <= min(Columns-1, x+m); xx++ {
					if t.covered[yy*Columns+xx] {
						near = true

						break
					}
				}
			}

			if near {
				t.lastHot[i], t.seen[i] = now, true
			}
		}
	}
}

// Recompute returns the decision at now without a new measurement, for example after Transition.
func (t *Tracker) Recompute(now time.Duration) Decision { return t.decide(now, 0, 0) }

// decide recomputes the covered cells at now and returns the decision.
func (t *Tracker) decide(now time.Duration, hotArea float64, peak int) Decision {
	t.nCov = 0

	for i := range cells {
		was := t.covered[i]
		t.covered[i] = t.seen[i] && now-t.lastHot[i] < t.p.Calm

		switch {
		case t.covered[i] && !was:
			t.since[i] = now
		case !t.covered[i]:
			t.forced[i] = false
		}

		if t.covered[i] {
			t.nCov++
		}
	}

	d := Decision{HotArea: hotArea, CoveredArea: percent(t.nCov), Peak: peak}

	switch {
	case t.nCov == 0:
	case d.CoveredArea >= t.p.FullArea:
		d.Rects, d.Full = []Rect{Full}, true
	default:
		d.Rects = merge(&t.covered, t.p.MaxRects)
		if len(d.Rects) == 1 && d.Rects[0] == Full {
			d.Full = true
		}
	}

	return d
}

// refreshTouched keeps a covered area whole: when hot cells or their margin touch it, every cell of
// that covered area is refreshed, judged from the uncovered surroundings since the covered cells
// themselves cannot be measured.
func (t *Tracker) refreshTouched(grown *[cells]bool, now time.Duration) {
	var visited [cells]bool

	stack := make([]int, 0, 64)

	for i := range cells {
		if !t.covered[i] || visited[i] || !grown[i] {
			continue
		}

		stack = append(stack[:0], i)
		visited[i] = true

		for len(stack) > 0 {
			c := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			t.lastHot[c], t.seen[c] = now, true

			cx, cy := c%Columns, c/Columns

			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					x, y := cx+dx, cy+dy
					if x < 0 || y < 0 || x >= Columns || y >= Rows {
						continue
					}

					n := y*Columns + x
					if t.covered[n] && !visited[n] {
						visited[n] = true
						stack = append(stack, n)
					}
				}
			}
		}
	}
}

func cellPeak(mask []byte, i int) byte {
	if i < len(mask) {
		return mask[i]
	}

	return 0
}

func percent(n int) float64 { return float64(n) * 100 / cells }

// dilate marks in out every cell within margin cells of a set cell of in, and returns how many.
func dilate(in, out *[cells]bool, margin int) int {
	n := 0

	for y := range Rows {
		for x := range Columns {
			if !in[y*Columns+x] {
				continue
			}

			for yy := max(0, y-margin); yy <= min(Rows-1, y+margin); yy++ {
				for xx := max(0, x-margin); xx <= min(Columns-1, x+margin); xx++ {
					if !out[yy*Columns+xx] {
						out[yy*Columns+xx] = true
						n++
					}
				}
			}
		}
	}

	return n
}

// box is a rectangle of cells, x0 and y0 inclusive, x1 and y1 exclusive.
type box struct{ x0, y0, x1, y1 int }

func (b box) area() int { return (b.x1 - b.x0) * (b.y1 - b.y0) }

func (b box) union(o box) box {
	return box{min(b.x0, o.x0), min(b.y0, o.y0), max(b.x1, o.x1), max(b.y1, o.y1)}
}

func (b box) rect() Rect {
	return Rect{
		X: float64(b.x0) / Columns,
		Y: float64(b.y0) / Rows,
		W: float64(b.x1-b.x0) / Columns,
		H: float64(b.y1-b.y0) / Rows,
	}
}

// merge covers the set cells with at most maxRects rectangles: exact horizontal runs stacked
// vertically, then, if there are too many, the bounding boxes of connected areas, then the pairs
// whose union adds the least uncovered area.
func merge(set *[cells]bool, maxRects int) []Rect {
	boxes := runs(set)
	if len(boxes) > maxRects {
		boxes = components(set)
	}

	for len(boxes) > maxRects {
		bi, bj, best := 0, 1, -1

		for i := range boxes {
			for j := i + 1; j < len(boxes); j++ {
				cost := boxes[i].union(boxes[j]).area() - boxes[i].area() - boxes[j].area()
				if best < 0 || cost < best {
					bi, bj, best = i, j, cost
				}
			}
		}

		boxes[bi] = boxes[bi].union(boxes[bj])
		boxes = append(boxes[:bj], boxes[bj+1:]...)
	}

	out := make([]Rect, len(boxes))
	for i, b := range boxes {
		out[i] = b.rect()
	}

	return out
}

// runs returns the set cells as horizontal runs, with runs of identical span on consecutive rows
// stacked into one box.
func runs(set *[cells]bool) []box {
	var (
		done []box
		open []box // boxes that end on the previous row
	)

	for y := range Rows {
		var next []box

		for x := 0; x < Columns; {
			if !set[y*Columns+x] {
				x++

				continue
			}

			x0 := x
			for x < Columns && set[y*Columns+x] {
				x++
			}

			extended := false

			for i, b := range open {
				if b.x0 == x0 && b.x1 == x {
					b.y1 = y + 1
					next = append(next, b)
					open = append(open[:i], open[i+1:]...)
					extended = true

					break
				}
			}

			if !extended {
				next = append(next, box{x0, y, x, y + 1})
			}
		}

		done = append(done, open...)
		open = next
	}

	return append(done, open...)
}

// components returns the bounding box of each 8-connected area of set cells.
func components(set *[cells]bool) []box {
	var (
		visited [cells]bool
		out     []box
	)

	stack := make([]int, 0, 64)

	for i := range cells {
		if !set[i] || visited[i] {
			continue
		}

		b := box{Columns, Rows, 0, 0}

		stack = append(stack[:0], i)
		visited[i] = true

		for len(stack) > 0 {
			c := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			cx, cy := c%Columns, c/Columns
			b = b.union(box{cx, cy, cx + 1, cy + 1})

			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					x, y := cx+dx, cy+dy
					if x < 0 || y < 0 || x >= Columns || y >= Rows {
						continue
					}

					n := y*Columns + x
					if set[n] && !visited[n] {
						visited[n] = true
						stack = append(stack, n)
					}
				}
			}
		}

		out = append(out, b)
	}

	return out
}

// Equal reports whether two rectangle sets are the same, in order.
func Equal(a, b []Rect) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
