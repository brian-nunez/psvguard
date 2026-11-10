package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/brian-nunez/psv"

	"github.com/brian-nunez/psvguard/internal/coverage"
	"github.com/brian-nunez/psvguard/internal/frame"
)

const testW, testH = 64, 36

func testConfig() Config {
	p := coverage.DefaultParams()

	return Config{
		Display: "1", Width: testW, Height: testH, FPS: 60,
		Thresholds: ScaleThresholds(psv.WCAG22(), 0.1),
		Params:     p,
	}
}

// bgra returns a uniform BGRA buffer.
func bgra(v byte) []byte {
	b := make([]byte, testW*testH*4)
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = v, v, v, 255
	}

	return b
}

// run drives a pipeline for dur, feeding white or black frames from content at each 60 fps tick,
// and returns the first time the covers appeared and the first decided failure's time.
func run(t *testing.T, content func(at time.Duration) byte, dur time.Duration) (covered, failed time.Duration, misses uint64) {
	t.Helper()

	pool := frame.NewPool(poolSize, testW, testH)

	p, err := NewPipeline(testConfig(), pool.Put)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	covered, failed = -1, -1
	tick := time.Second / 60

	var last byte = 1

	for at := time.Duration(0); at < dur; at += tick {
		if v := content(at); v != last {
			s := pool.Get()
			if s == nil {
				t.Fatal("pool exhausted: frames are leaking")
			}

			if err := frame.Convert(&s.Frame, frame.BGRA, bgra(v), testW, testH, testW*4); err != nil {
				t.Fatal(err)
			}

			s.PTS = int64(at)
			p.Offer(s)

			last = v
		}

		r, ok, err := p.Step(at)
		if err != nil {
			t.Fatal(err)
		}

		if !ok {
			continue
		}

		if covered < 0 && r.Decision.Covering() {
			covered = at
		}

		if failed < 0 && r.DecidedFailures > 0 {
			failed = at - time.Second // decided one second after the frame
		}
	}

	return covered, failed, p.Counters().Misses
}

func TestFullScreenFlashingIsCoveredBeforeItFails(t *testing.T) {
	// White and black alternating every 100 ms: five flashes a second.
	flash := func(at time.Duration) byte {
		if (at/(100*time.Millisecond))%2 == 0 {
			return 255
		}

		return 0
	}

	covered, failed, misses := run(t, flash, 3*time.Second)
	if covered < 0 {
		t.Fatal("never covered")
	}

	if failed < 0 {
		t.Fatal("psv never decided a failure")
	}

	if covered >= failed {
		t.Fatalf("covered at %v, after the first failing frame at %v", covered, failed)
	}

	if misses != 0 {
		t.Fatalf("%d decided failures were not covered", misses)
	}

	t.Logf("covered at %v; first failing frame at %v", covered, failed)
}

func TestStillContentIsNotCovered(t *testing.T) {
	covered, _, _ := run(t, func(time.Duration) byte { return 128 }, 2*time.Second)
	if covered >= 0 {
		t.Fatalf("still content covered at %v", covered)
	}
}

func TestSlowChangesAreNotCovered(t *testing.T) {
	// One change a second, like a blinking cursor or a slideshow.
	slow := func(at time.Duration) byte {
		if (at/time.Second)%2 == 0 {
			return 255
		}

		return 0
	}

	covered, _, _ := run(t, slow, 4*time.Second)
	if covered >= 0 {
		t.Fatalf("one change a second covered at %v", covered)
	}
}

func TestCaptureFrameDropsInsteadOfBlocking(t *testing.T) {
	out := make(chan Report, 8)

	d, err := NewDisplay(testConfig(), 1, out)
	if err != nil {
		t.Fatal(err)
	}

	src := bgra(200)

	// Nobody is analysing: the hand-off holds one frame, and every newer frame replaces it.
	for i := range 20 {
		d.CaptureFrame(frame.BGRA, src, testW, testH, testW*4, int64(i))
	}

	st := d.Stats()
	if st.Captured != 20 || st.Dropped != 19 {
		t.Fatalf("captured %d, dropped %d; want 20 and 19", st.Captured, st.Dropped)
	}

	// The waiting frame is the newest.
	s := <-d.in
	if s.PTS != 19 {
		t.Fatalf("waiting frame PTS %d, want 19", s.PTS)
	}

	// A wrongly sized buffer is dropped, not converted.
	d.CaptureFrame(frame.BGRA, src[:100], testW, testH, testW*4, 99)

	if d.Stats().Dropped != 20 {
		t.Fatal("bad buffer not counted as dropped")
	}
}

func TestCaptureFrameAllocatesNothing(t *testing.T) {
	d, err := NewDisplay(testConfig(), 1, make(chan Report, 1))
	if err != nil {
		t.Fatal(err)
	}

	src := bgra(10)

	allocs := testing.AllocsPerRun(100, func() {
		d.CaptureFrame(frame.BGRA, src, testW, testH, testW*4, 1)
	})
	if allocs != 0 {
		t.Fatalf("CaptureFrame allocated %.1f times per call", allocs)
	}
}

func TestDisplayReportsCoverChanges(t *testing.T) {
	out := make(chan Report, 64)

	d, err := NewDisplay(testConfig(), 7, out)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx)

	white, black := bgra(255), bgra(0)
	deadline := time.After(5 * time.Second)
	flip := time.NewTicker(100 * time.Millisecond)

	defer flip.Stop()

	for i := int64(0); ; i++ {
		select {
		case r := <-out:
			if r.Stream != 7 || r.Display != "1" || len(r.Rects) == 0 {
				t.Fatalf("report = %+v", r)
			}

			if st := d.Stats(); st.Frames == 0 || st.WatchTotal == 0 {
				t.Fatalf("stats not updated: %+v", st)
			}

			return
		case <-flip.C:
			src := white
			if i%2 == 1 {
				src = black
			}

			d.CaptureFrame(frame.BGRA, src, testW, testH, testW*4, int64(time.Duration(i)*100*time.Millisecond))
		case <-deadline:
			t.Fatal("no cover report for full-screen flashing within 5 s")
		}
	}
}

func TestScaleThresholds(t *testing.T) {
	th := ScaleThresholds(psv.WCAG22(), 640.0/1512)
	if th.RegionWidth != 144 || th.RegionHeight != 108 {
		t.Fatalf("region %dx%d", th.RegionWidth, th.RegionHeight)
	}

	if b := ScaleThresholds(psv.Broadcast(), 0.5); b.RegionWidth != 0 || b.RegionHeight != 0 {
		t.Fatal("whole-frame regions must stay whole-frame")
	}
}

func TestCaptureSizeKeepsAspectAndEvenSides(t *testing.T) {
	for _, tc := range []struct {
		w, h   float64
		target int
		ww, wh int
	}{
		{1512, 982, 640, 640, 416},
		{1920, 1080, 640, 640, 360},
		{2560, 1440, 641, 640, 360},
		{1080, 1920, 640, 640, 1138},
	} {
		w, h := CaptureSize(tc.w, tc.h, tc.target)
		if w != tc.ww || h != tc.wh {
			t.Errorf("CaptureSize(%v, %v, %d) = %dx%d, want %dx%d", tc.w, tc.h, tc.target, w, h, tc.ww, tc.wh)
		}
	}
}
