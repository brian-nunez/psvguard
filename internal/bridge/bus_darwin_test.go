//go:build darwin && cgo

package bridge

import (
	"log"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brian-nunez/psvguard/internal/frame"
)

// smokeBus is set when the smoke test can run: Screen Recording permission is granted and
// PSVGUARD_SMOKE=1, because the test puts an icon in the menu bar and a cover on screen.
var smokeBus Bus

// TestMain hands the main thread to AppKit when the smoke test runs, as the app does.
func TestMain(m *testing.M) {
	if os.Getenv("PSVGUARD_SMOKE") != "1" || !screenCaptureGranted() {
		os.Exit(m.Run())
	}

	b, err := New(Options{Version: "test", Trace: os.Getenv("PSVGUARD_TRACE") == "1", Log: log.New(os.Stderr, "", log.Lmicroseconds)})
	if err != nil {
		panic(err)
	}

	smokeBus = b
	code := 0

	go func() {
		code = m.Run()

		b.Post(Quit{})
	}()

	_ = b.Run()

	os.Exit(code)
}

func near(a, b [4]float64) bool {
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}

	return true
}

func TestNormToPointsFlipsAndScales(t *testing.T) {
	// A 1512×982 point display at the origin (a 14-inch MacBook Pro at 2×).
	f := [4]float64{0, 0, 1512, 982}

	if got := normToPoints(Rect{X: 0, Y: 0, W: 1, H: 1}, f, 2); !near(got, f) {
		t.Fatalf("whole display = %v", got)
	}

	// The top-left quarter sits at the top: bottom-left origin y = 491.
	if got := normToPoints(Rect{X: 0, Y: 0, W: 0.5, H: 0.5}, f, 2); !near(got, [4]float64{0, 491, 756, 491}) {
		t.Fatalf("top-left quarter = %v", got)
	}

	// The bottom-right quarter.
	if got := normToPoints(Rect{X: 0.5, Y: 0.5, W: 0.5, H: 0.5}, f, 2); !near(got, [4]float64{756, 0, 756, 491}) {
		t.Fatalf("bottom-right quarter = %v", got)
	}
}

func TestNormToPointsRoundsOutwardToDevicePixels(t *testing.T) {
	f := [4]float64{0, 0, 1512, 982}

	// One grid cell: 1512/64 = 23.625 points wide, 982/36 = 27.28 points high.
	got := normToPoints(Rect{X: 1.0 / 64, Y: 1.0 / 36, W: 1.0 / 64, H: 1.0 / 36}, f, 2)

	for _, v := range got {
		if v*2 != math.Trunc(v*2) {
			t.Fatalf("%v is not on a device pixel at 2×", got)
		}
	}

	if got[0] > 23.625 || got[0]+got[2] < 47.25 {
		t.Fatalf("x range %v does not contain the cell", got)
	}

	// At 1× the same cell lands on whole points.
	got1 := normToPoints(Rect{X: 1.0 / 64, Y: 1.0 / 36, W: 1.0 / 64, H: 1.0 / 36}, f, 1)
	for _, v := range got1 {
		if v != math.Trunc(v) {
			t.Fatalf("%v is not on a device pixel at 1×", got1)
		}
	}
}

func TestNormToPointsOnASecondDisplay(t *testing.T) {
	// An external display to the left of and above the main one, as macOS reports it.
	f := [4]float64{-1920, 982, 1920, 1080}

	got := normToPoints(Rect{X: 0.25, Y: 0, W: 0.5, H: 0.25}, f, 1)
	if !near(got, [4]float64{-1440, 1792, 960, 270}) {
		t.Fatalf("got %v", got)
	}

	// Nothing spills outside the display.
	got = normToPoints(Rect{X: 0.9, Y: 0.9, W: 0.2, H: 0.2}, f, 1)
	if got[0]+got[2] > 0 || got[1] < 982 {
		t.Fatalf("spilled: %v", got)
	}
}

type countingTarget struct {
	n      atomic.Int64
	w, h   atomic.Int64
	format atomic.Int64
}

func (c *countingTarget) CaptureFrame(f frame.Format, src []byte, width, height, stride int, _ int64) {
	if len(src) < height*stride {
		return
	}

	c.n.Add(1)
	c.w.Store(int64(width))
	c.h.Store(int64(height))
	c.format.Store(int64(f))
}

// TestSmokeCaptureAndCover starts a stream for the main display, receives frames, posts a cover
// and stops cleanly. It needs Screen Recording permission and PSVGUARD_SMOKE=1.
func TestSmokeCaptureAndCover(t *testing.T) {
	if smokeBus == nil {
		t.Skip("set PSVGUARD_SMOKE=1 and grant Screen Recording permission to run")
	}

	b := smokeBus
	events := b.Events()

	var (
		mu       sync.Mutex
		displays []Display
	)

	wait := func(what string, ok func(Event) bool) Event {
		t.Helper()

		timeout := time.After(10 * time.Second)

		for {
			select {
			case e := <-events:
				if d, isD := e.(DisplaysEvent); isD {
					mu.Lock()
					displays = d.Displays
					mu.Unlock()
				}

				if ok(e) {
					return e
				}
			case <-timeout:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	caps := wait("capabilities", func(e Event) bool {
		_, ok := e.(CapabilitiesEvent)

		return ok
	}).(CapabilitiesEvent)
	if !caps.ExcludesOwnWindows || !caps.CanOverlay {
		t.Fatalf("capabilities %+v", caps)
	}

	wait("ready", func(e Event) bool {
		_, ok := e.(ReadyEvent)

		return ok
	})

	mu.Lock()

	var main Display
	for _, d := range displays {
		if d.Main || main.ID == "" {
			main = d
		}
	}
	mu.Unlock()

	if main.ID == "" {
		t.Fatal("no displays reported")
	}

	target := &countingTarget{}
	b.Post(StartCapture{Stream: 1, Display: main.ID, Width: 640, Height: 400, FPS: 30, Format: frame.BGRA, Target: target})
	wait("stream started", func(e Event) bool {
		s, ok := e.(StreamEvent)
		if ok && s.Kind == EvStreamFailed {
			t.Fatalf("stream failed: %s", s.Error)
		}

		return ok && s.Kind == EvStreamStarted && s.Stream == 1
	})

	// The screen may be still, so idle frames are not delivered; covering a corner makes content
	// change.
	b.Post(SetCovers{Display: main.ID, FadeIn: 0.1, FadeOut: 0.2, Rects: []Rect{{X: 0.9, Y: 0.9, W: 0.1, H: 0.1}}})
	wait("covers shown", func(e Event) bool {
		c, ok := e.(CoversShown)

		return ok && c.Rects == 1
	})

	deadline := time.Now().Add(5 * time.Second)
	for target.n.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	if target.n.Load() < 1 {
		t.Fatal("no frames received")
	}

	if target.w.Load() != 640 || target.h.Load() != 400 || frame.Format(target.format.Load()) != frame.BGRA {
		t.Fatalf("frames %dx%d format %d", target.w.Load(), target.h.Load(), target.format.Load())
	}

	b.Post(ClearCovers{Display: main.ID})
	b.Post(StopCapture{Stream: 1, Display: main.ID})
	wait("stream stopped", func(e Event) bool {
		s, ok := e.(StreamEvent)

		return ok && s.Kind == EvStreamStopped && s.Stream == 1
	})

	t.Logf("received %d frames from display %s", target.n.Load(), main.ID)
}
