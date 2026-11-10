package coverage

import (
	"math"
	"testing"
	"time"

	"github.com/brian-nunez/psv"
)

// building returns a luminance measurement whose mask holds peak in the given cells.
func building(peak byte, cellsXY ...[2]int) psv.Building {
	if len(cellsXY) == 0 {
		return psv.Building{}
	}

	mask := make([]byte, cells)
	for _, c := range cellsXY {
		mask[c[1]*Columns+c[0]] = peak
	}

	area := float64(len(cellsXY)) * 100 / cells

	return psv.Building{Luminance: psv.Activity{Peak: int(peak), Mask: mask, Area: area}}
}

// block returns the cells of a w×h block at (x, y).
func block(x, y, w, h int) [][2]int {
	var out [][2]int

	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			out = append(out, [2]int{xx, yy})
		}
	}

	return out
}

func params() Params {
	p := DefaultParams()
	p.MinHotArea = 0.5
	p.DenseFrames = 1
	p.DenseShare = 0

	return p
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestHotCellsNeedThePeakThreshold(t *testing.T) {
	tr := NewTracker(params())

	if d := tr.Update(0, building(3, block(10, 10, 6, 6)...)); d.Covering() {
		t.Fatal("peak 3 covered with a hot threshold of 4")
	}

	if d := tr.Update(time.Millisecond, building(4, block(10, 10, 6, 6)...)); !d.Covering() {
		t.Fatal("peak 4 did not cover")
	}
}

func TestRedCountsLikeLuminance(t *testing.T) {
	tr := NewTracker(params())
	b := building(5, block(0, 0, 6, 6)...)
	b.Red, b.Luminance = b.Luminance, psv.Activity{}

	if d := tr.Update(0, b); !d.Covering() || d.Peak != 5 {
		t.Fatalf("red flashing not covered: %+v", d)
	}
}

func TestMarginGrowsTheCoveredArea(t *testing.T) {
	tr := NewTracker(params())

	d := tr.Update(0, building(6, block(10, 10, 4, 4)...))
	if len(d.Rects) != 1 {
		t.Fatalf("rects = %v", d.Rects)
	}

	want := Rect{X: 9.0 / Columns, Y: 9.0 / Rows, W: 6.0 / Columns, H: 6.0 / Rows}
	if got := d.Rects[0]; !near(got.X, want.X) || !near(got.Y, want.Y) || !near(got.W, want.W) || !near(got.H, want.H) {
		t.Fatalf("rect = %+v, want %+v", got, want)
	}

	if !near(d.HotArea, 36*100.0/cells) {
		t.Fatalf("hot area = %v", d.HotArea)
	}
}

func TestSmallFlashingStaysUncoveredBelowTheMinimumArea(t *testing.T) {
	p := params()
	p.MinHotArea = 2 // 46 cells; one hot cell with margin is 9
	tr := NewTracker(p)

	if d := tr.Update(0, building(6, [2]int{30, 20})); d.Covering() {
		t.Fatal("a single blinking cell was covered")
	}
}

func TestFlashingKeepsACoverWithoutTheFillTest(t *testing.T) {
	tr := NewTracker(params())
	area := block(20, 10, 8, 8)

	tr.Update(0, building(6, area...))

	// Later the flashing fills its cells less (a patterned object), but psv reports 2% of the
	// display already flashing: the cover stays beyond the calm time.
	sparse := building(8, area...)
	sparse.Luminance.Area, sparse.Luminance.Flashing = 0.5, 2

	for at := 500 * time.Millisecond; at <= 5*time.Second; at += 500 * time.Millisecond {
		tr.Update(at, sparse)
	}

	if d := tr.Update(5100*time.Millisecond, psv.Building{}); !d.Covering() {
		t.Fatal("cover dropped while psv still reported flashing")
	}
}

func TestKeepingACoverDoesNotSpreadIt(t *testing.T) {
	tr := NewTracker(params())
	tr.Update(0, building(6, block(2, 2, 4, 4)...))

	// Sparse flashing far away keeps the cover's own cells but adds no cover over there.
	cs := append(block(2, 2, 4, 4), block(40, 20, 6, 6)...)

	far := building(8, cs...)
	far.Luminance.Area, far.Luminance.Flashing = 0.5, 2

	d := tr.Update(1500*time.Millisecond, far)
	for _, r := range d.Rects {
		if r.X > 0.5 {
			t.Fatalf("cover spread to %+v", r)
		}
	}
}

func TestSparseFlickerIsNotCovered(t *testing.T) {
	p := DefaultParams()
	tr := NewTracker(p)

	// Film grain: a few pixels in each of many cells transition, so 20% of the cells are hot but
	// only 2% of the display transitions in any frame.
	b := building(6, block(0, 0, 32, 14)...)
	b.Luminance.Area = 2

	for at := time.Duration(0); at < 3*time.Second; at += time.Second / 30 {
		if d := tr.Update(at, b); d.Covering() {
			t.Fatalf("sparse flicker covered at %v", at)
		}
	}
}

func TestDenseFlashingNeedsSeveralFrames(t *testing.T) {
	tr := NewTracker(DefaultParams())
	hot := building(5, block(10, 10, 30, 20)...)

	// Two dense frames are too few to judge by; the third, as a flash's fourth transition
	// arrives, starts the cover.
	for i, want := range []bool{false, false, true} {
		if d := tr.Update(time.Duration(i)*time.Second/10, hot); d.Covering() != want {
			t.Fatalf("after %d dense frames covering = %v, want %v", i+1, d.Covering(), want)
		}
	}
}

func TestDenseFramesMustBeRecent(t *testing.T) {
	tr := NewTracker(DefaultParams())
	hot := building(5, block(10, 10, 30, 20)...)

	tr.Update(0, hot)

	if d := tr.Update(1500*time.Millisecond, hot); d.Covering() {
		t.Fatal("a dense frame 1.5 s old counted")
	}
}

func TestASingleSceneChangeIsNotFlashing(t *testing.T) {
	tr := NewTracker(DefaultParams())

	// One cut changes the whole display once; afterwards nothing transitions.
	cut := building(4, block(0, 0, Columns, Rows)...)
	if d := tr.Update(0, cut); d.Covering() {
		t.Fatal("a single cut was covered")
	}

	if d := tr.Update(time.Second/30, psv.Building{}); d.Covering() {
		t.Fatal("covered after a single cut")
	}
}

func TestScrollingTextIsNotCovered(t *testing.T) {
	tr := NewTracker(DefaultParams())

	// Scrolling text: most cells hold pixels that transition every frame, but only about a
	// third of the pixels in them do.
	b := building(9, block(0, 0, Columns, 30)...)
	b.Luminance.Area *= 0.3
	b.Luminance.Flashing = 20

	for at := time.Duration(0); at < 3*time.Second; at += time.Second / 60 {
		if d := tr.Update(at, b); d.Covering() {
			t.Fatalf("scrolling text covered at %v", at)
		}
	}
}

func TestForceCoversADecidedFailure(t *testing.T) {
	tr := NewTracker(DefaultParams())

	mask := make([]byte, cells)
	mask[10*Columns+30] = 40

	tr.Force(0, mask)

	d := tr.Update(time.Millisecond, psv.Building{})
	if len(d.Rects) != 1 {
		t.Fatalf("rects = %v", d.Rects)
	}

	r := d.Rects[0]
	if x, y := 30.5/Columns, 10.5/Rows; x < r.X || x > r.X+r.W || y < r.Y || y > r.Y+r.H {
		t.Fatalf("forced cell not covered: %+v", r)
	}

	tr.Force(0, nil) // no cells: nothing happens
}

func TestSparseFlashingPixelsDoNotStartACover(t *testing.T) {
	tr := NewTracker(DefaultParams())

	// 1.2% of the display flashes, but spread over cells covering 20% of it.
	b := building(8, block(0, 0, 32, 15)...)
	b.Luminance.Flashing, b.Luminance.Area = 1.2, 1.5

	for at := time.Duration(0); at < time.Second; at += time.Second / 30 {
		if d := tr.Update(at, b); d.Covering() {
			t.Fatal("sparse flashing pixels started a cover")
		}
	}
}

func TestHoldBridgesTheGapsBetweenFlashes(t *testing.T) {
	tr := NewTracker(params())
	hot := building(5, block(20, 10, 8, 8)...)

	tr.Update(0, hot)

	// Between flashes psv reports nothing; the cover holds for the calm time.
	for _, at := range []time.Duration{100 * time.Millisecond, time.Second, 2900 * time.Millisecond} {
		if d := tr.Update(at, psv.Building{}); !d.Covering() {
			t.Fatalf("uncovered %v after the last flash", at)
		}
	}

	if d := tr.Update(3*time.Second, psv.Building{}); d.Covering() {
		t.Fatal("still covered after the calm time")
	}
}

func TestFlashingAgainRefreshesTheHold(t *testing.T) {
	tr := NewTracker(params())
	hot := building(5, block(20, 10, 8, 8)...)

	tr.Update(0, hot)
	tr.Update(2*time.Second, hot)

	if d := tr.Update(4500*time.Millisecond, psv.Building{}); !d.Covering() {
		t.Fatal("a refreshed cell was uncovered early")
	}
}

func TestLargeAreasCoverTheWholeDisplay(t *testing.T) {
	tr := NewTracker(params())

	d := tr.Update(0, building(5, block(1, 1, 40, 20)...)) // 42×22 with margin: about 40%
	if !d.Full || len(d.Rects) != 1 || d.Rects[0] != Full {
		t.Fatalf("decision = %+v; want the whole display", d)
	}
}

func TestSeparateAreasGetSeparateRects(t *testing.T) {
	tr := NewTracker(params())

	var cs [][2]int

	cs = append(cs, block(2, 2, 4, 4)...)
	cs = append(cs, block(40, 20, 4, 4)...)

	d := tr.Update(0, building(5, cs...))
	if len(d.Rects) != 2 {
		t.Fatalf("rects = %v; want 2", d.Rects)
	}
}

func TestMergeKeepsAtMostMaxRects(t *testing.T) {
	p := params()
	p.MaxRects = 3
	p.Margin = 0
	p.MinHotArea = 0.01
	tr := NewTracker(p)

	var cs [][2]int
	for i := range 10 {
		cs = append(cs, [2]int{i * 6, (i * 3) % Rows})
	}

	d := tr.Update(0, building(5, cs...))
	if len(d.Rects) > 3 || len(d.Rects) == 0 {
		t.Fatalf("%d rects; want 1 to 3", len(d.Rects))
	}

	// Every hot cell must stay covered after merging.
	for _, c := range cs {
		x, y := (float64(c[0])+0.5)/Columns, (float64(c[1])+0.5)/Rows
		in := false

		for _, r := range d.Rects {
			if x >= r.X && x <= r.X+r.W && y >= r.Y && y <= r.Y+r.H {
				in = true
			}
		}

		if !in {
			t.Fatalf("cell %v not covered by %v", c, d.Rects)
		}
	}
}

func TestRunsStackIdenticalSpans(t *testing.T) {
	var set [cells]bool

	for _, c := range block(3, 4, 5, 6) {
		set[c[1]*Columns+c[0]] = true
	}

	b := runs(&set)
	if len(b) != 1 || b[0] != (box{3, 4, 8, 10}) {
		t.Fatalf("runs = %v", b)
	}
}

func TestCoversDoNotFeedBackWithoutExclusion(t *testing.T) {
	p := params()
	p.ExcludesOwnWindows = false
	tr := NewTracker(p)

	area := block(20, 10, 6, 6)
	tr.Update(0, building(5, area...))

	// The capture now shows the cover fading in over the same cells. Those transitions must not
	// keep the cover alive on their own.
	for at := 100 * time.Millisecond; at < 3*time.Second; at += 100 * time.Millisecond {
		tr.Update(at, building(5, area...))
	}

	if d := tr.Update(3*time.Second, psv.Building{}); d.Covering() {
		t.Fatal("the cover kept itself alive from its own pixels")
	}
}

func TestSurroundingsKeepACoverWholeWithoutExclusion(t *testing.T) {
	p := params()
	p.ExcludesOwnWindows = false
	tr := NewTracker(p)

	tr.Update(0, building(5, block(20, 10, 6, 6)...))

	// Flashing appears just outside the cover's edge at 2.5 s: the whole covered area is
	// refreshed, interior included, judged from those surroundings.
	tr.Update(2500*time.Millisecond, building(5, [2]int{27, 13}))

	d := tr.Update(4*time.Second, psv.Building{})
	if !d.Covering() {
		t.Fatal("cover dropped although its surroundings flashed")
	}

	// The original interior (cell 22,12) is still covered: no holes.
	x, y := 22.5/Columns, 12.5/Rows
	in := false

	for _, r := range d.Rects {
		if x >= r.X && x <= r.X+r.W && y >= r.Y && y <= r.Y+r.H {
			in = true
		}
	}

	if !in {
		t.Fatalf("interior uncovered: %v", d.Rects)
	}
}

func TestResetUncovers(t *testing.T) {
	tr := NewTracker(params())
	tr.Update(0, building(5, block(0, 0, 8, 8)...))
	tr.Reset()

	if d := tr.Update(time.Millisecond, psv.Building{}); d.Covering() {
		t.Fatal("covered after Reset")
	}
}

func TestEqual(t *testing.T) {
	a := []Rect{{X: 0, Y: 0, W: 0.5, H: 0.5}}

	if !Equal(a, []Rect{{X: 0, Y: 0, W: 0.5, H: 0.5}}) || Equal(a, nil) || Equal(a, []Rect{{X: 0, Y: 0, W: 0.5, H: 0.6}}) {
		t.Fatal("Equal is wrong")
	}
}

func TestTransitionWithdrawsRecentCoversAndQuietsStarts(t *testing.T) {
	tr := NewTracker(params())
	hot := building(6, block(10, 10, 30, 20)...)

	tr.Update(0, hot)
	tr.Transition(100*time.Millisecond, time.Second, 600*time.Millisecond)

	if d := tr.Recompute(100 * time.Millisecond); d.Covering() {
		t.Fatal("a cover begun during the animation was kept")
	}

	if d := tr.Update(300*time.Millisecond, hot); d.Covering() {
		t.Fatal("a cover started during the quiet time")
	}

	if d := tr.Update(800*time.Millisecond, hot); !d.Covering() {
		t.Fatal("flashing that continues was not covered after the quiet time")
	}
}

func TestTransitionKeepsOlderAndForcedCovers(t *testing.T) {
	tr := NewTracker(params())
	tr.Update(0, building(6, block(2, 2, 4, 4)...))

	// Held for the calm time, the old cover is more than a second old at the transition.
	tr.Update(1500*time.Millisecond, building(6, block(2, 2, 4, 4)...))

	mask := make([]byte, cells)
	mask[30*Columns+50] = 50
	tr.Force(1900*time.Millisecond, mask)
	tr.Recompute(1900 * time.Millisecond)

	tr.Transition(2*time.Second, time.Second, 600*time.Millisecond)

	d := tr.Recompute(2 * time.Second)
	if len(d.Rects) != 2 {
		t.Fatalf("rects = %v; want the old cover and the forced one", d.Rects)
	}
}
