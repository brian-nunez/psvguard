package resample

import (
	"testing"
	"time"

	"github.com/brian-nunez/psvguard/internal/frame"
)

type recorder struct{ released []*frame.Slot }

func (r *recorder) release(s *frame.Slot) { r.released = append(r.released, s) }

func slot(pts int64) *frame.Slot { return &frame.Slot{PTS: pts} }

func TestTickRepeatsUntilANewFrameArrives(t *testing.T) {
	var rec recorder

	r := New(60, rec.release)

	if s, _ := r.Tick(); s != nil {
		t.Fatal("tick before any frame returned a frame")
	}

	a := slot(1)
	r.Offer(a)

	if s, repeat := r.Tick(); s != a || repeat {
		t.Fatalf("first tick = %v, %v; want a, fresh", s, repeat)
	}

	for range 3 {
		if s, repeat := r.Tick(); s != a || !repeat {
			t.Fatalf("idle tick = %v, %v; want a repeated", s, repeat)
		}
	}

	b := slot(2)
	r.Offer(b)

	if s, repeat := r.Tick(); s != b || repeat {
		t.Fatalf("tick after b = %v, %v; want b, fresh", s, repeat)
	}

	if len(rec.released) != 1 || rec.released[0] != a {
		t.Fatalf("released %v; want only a, once b replaced it", rec.released)
	}

	st := r.Stats()
	if st.Ticks != 5 || st.Fresh != 2 || st.Repeats != 3 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestOfferKeepsOnlyTheNewestFrame(t *testing.T) {
	var rec recorder

	r := New(60, rec.release)
	a, b := slot(10), slot(20)

	r.Offer(a)
	r.Offer(b)

	if s, _ := r.Tick(); s != b {
		t.Fatal("tick did not return the newest frame")
	}

	if len(rec.released) != 1 || rec.released[0] != a {
		t.Fatalf("superseded frame not released: %v", rec.released)
	}

	if r.Stats().Superseded != 1 {
		t.Fatalf("stats = %+v", r.Stats())
	}
}

func TestOfferDropsStaleFrames(t *testing.T) {
	var rec recorder

	r := New(60, rec.release)
	a := slot(10)
	r.Offer(a)
	r.Tick()

	old := slot(5)
	r.Offer(old)

	if s, repeat := r.Tick(); s != a || !repeat {
		t.Fatal("a stale frame replaced the current one")
	}

	if len(rec.released) != 1 || rec.released[0] != old || r.Stats().Stale != 1 {
		t.Fatalf("stale frame not dropped: %v %+v", rec.released, r.Stats())
	}
}

func TestDueKeepsAFixedRateAndSkipsBacklog(t *testing.T) {
	r := New(60, func(*frame.Slot) {})
	iv := r.Interval()

	if iv != time.Second/60 {
		t.Fatalf("interval = %v", iv)
	}

	ticks := 0

	for now := time.Duration(0); now < time.Second; now += time.Millisecond {
		if r.Due(now) {
			ticks++
		}
	}

	if ticks < 59 || ticks > 61 {
		t.Fatalf("%d ticks in one second at 60 fps", ticks)
	}

	// A stall of half a second yields one tick, not thirty.
	stalled := time.Second + 500*time.Millisecond
	if !r.Due(stalled) {
		t.Fatal("no tick after a stall")
	}

	if r.Due(stalled + time.Millisecond) {
		t.Fatal("backlog replayed after a stall")
	}
}

func TestResetReleasesEverything(t *testing.T) {
	var rec recorder

	r := New(30, rec.release)
	r.Offer(slot(1))
	r.Tick()
	r.Offer(slot(2))
	r.Reset()

	if len(rec.released) != 2 {
		t.Fatalf("released %d frames, want 2", len(rec.released))
	}

	if s, _ := r.Tick(); s != nil {
		t.Fatal("tick after reset returned a frame")
	}
}
