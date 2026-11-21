//go:build linux && cgo

package bridge

import (
	"encoding/json"
	"errors"
	"log"
	"math"
	"os"
	"reflect"
	"testing"
	"time"
)

// smokeBus is set when the smoke test can run: PSVGUARD_SMOKE=1 in a Wayland session, because the
// test puts an item in the tray and a cover on screen.
var smokeBus Bus

// TestMain hands the main thread to GTK when the smoke test runs, as the app does.
func TestMain(m *testing.M) {
	if os.Getenv("PSVGUARD_SMOKE") != "1" || os.Getenv("WAYLAND_DISPLAY") == "" {
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

	if err := b.Run(); err != nil {
		panic(err)
	}

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

func TestNormToPx(t *testing.T) {
	size := [2]float64{1600, 900}

	for _, tc := range []struct {
		name  string
		r     Rect
		scale float64
		want  [4]float64
	}{
		{"whole display", Rect{X: 0, Y: 0, W: 1, H: 1}, 1, [4]float64{0, 0, 1600, 900}},
		{"top-left origin", Rect{X: 0.25, Y: 0.5, W: 0.5, H: 0.25}, 1, [4]float64{400, 450, 800, 225}},
		{"rounds outward at 1x", Rect{X: 0.1001, Y: 0.1001, W: 0.1, H: 0.1}, 1, [4]float64{160, 90, 161, 91}},
		{"rounds outward to half pixels at 2x", Rect{X: 0.1001, Y: 0.1001, W: 0.1, H: 0.1}, 2, [4]float64{160, 90, 160.5, 90.5}},
		{"clipped to the display", Rect{X: 0.9, Y: 0.9, W: 0.5, H: 0.5}, 1, [4]float64{1440, 810, 160, 90}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normToPx(tc.r, size, tc.scale); !near(got, tc.want) {
				t.Errorf("normToPx(%+v) = %v, want %v", tc.r, got, tc.want)
			}
		})
	}
}

// The C side parses commands and writes events with its own JSON code. Payloads must survive it
// with their types intact; booleans above all, which the event decoder rejects as numbers.
func TestJSONRoundTrip(t *testing.T) {
	for _, in := range []string{
		`{"state":"granted"}`,
		`{"sleeping":true,"other":false}`,
		`{"title":"Watching","enabled":true,"detail":"quote \" backslash \\ newline \n tab \t é ✓ 😀"}`,
		`[{"key":"enabled","min":0,"max":1.5,"choices":[{"value":"a","label":"A"}]},{"key":"x","value":"","choices":[]}]`,
		`{"stream":18446744073709,"handle":42,"neg":-3.25e-2}`,
		`{"nested":{"deep":[[1,2],[true,false],{}]}}`,
		`[]`,
		`{}`,
	} {
		got := jsonRoundTrip(in)

		var a, b any
		if err := json.Unmarshal([]byte(in), &a); err != nil {
			t.Fatalf("test input %s: %v", in, err)
		}

		if err := json.Unmarshal([]byte(got), &b); err != nil {
			t.Errorf("round trip of %s gave invalid JSON %s: %v", in, got, err)

			continue
		}

		if !reflect.DeepEqual(a, b) {
			t.Errorf("round trip of %s = %s", in, got)
		}
	}
}

func TestJSONMalformedIsEmpty(t *testing.T) {
	for _, in := range []string{``, `{`, `{"a":}`, `[1,`, `"unterminated`, `nonsense`} {
		if got := jsonRoundTrip(in); got != "{}" {
			t.Errorf("jsonRoundTrip(%q) = %s, want {}", in, got)
		}
	}
}

func TestNewNeedsWayland(t *testing.T) {
	if smokeBus != nil {
		t.Skip("the smoke test owns the bus")
	}

	t.Setenv("WAYLAND_DISPLAY", "")

	if _, err := New(Options{}); !errors.Is(err, ErrNoWayland) {
		t.Fatalf("New outside Wayland = %v, want ErrNoWayland", err)
	}
}

// TestSmoke starts the bus in the session, checks the startup events, shows a cover and quits. It
// does not start capture, which needs the user's approval in the share dialog.
func TestSmoke(t *testing.T) {
	if smokeBus == nil {
		t.Skip("set PSVGUARD_SMOKE=1 in a Wayland session")
	}

	events := smokeBus.Events()
	next := func(what string) Event {
		select {
		case e := <-events:
			return e
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s event", what)

			return nil
		}
	}

	caps, ok := next("capabilities").(CapabilitiesEvent)
	if !ok || caps.ExcludesOwnWindows || !caps.ConsentPerSession || len(caps.PixelFormats) == 0 {
		t.Fatalf("capabilities = %+v", caps)
	}

	ds, ok := next("displays").(DisplaysEvent)
	if !ok || len(ds.Displays) == 0 || ds.Displays[0].Width <= 0 || ds.Displays[0].ID == "" {
		t.Fatalf("displays = %+v", ds)
	}

	if _, ok := next("ready").(ReadyEvent); !ok {
		t.Fatal("no ready event")
	}

	smokeBus.Post(CheckPermission{})

	if p, ok := next("permission").(PermissionEvent); !ok || p.State == "" {
		t.Fatalf("permission = %+v", p)
	}

	d := ds.Displays[0].ID
	smokeBus.Post(SetCovers{Display: d, FadeIn: 0.15, FadeOut: 0.3, Rects: []Rect{{X: 0.25, Y: 0.25, W: 0.5, H: 0.5}}})

	// Without overlays (GNOME) the command is answered with no covers shown.
	want := 0
	if caps.CanOverlay {
		want = 1
	}

	shown, ok := next("covers shown").(CoversShown)
	if !ok || shown.Display != d || shown.Rects != want || shown.At <= 0 {
		t.Fatalf("covers shown = %+v, want %d rects", shown, want)
	}

	time.Sleep(500 * time.Millisecond)
	smokeBus.Post(ClearCovers{})

	// Clearing reports each display that had covers; without overlays there were none.
	if caps.CanOverlay {
		if shown, ok := next("covers cleared").(CoversShown); !ok || shown.Rects != 0 {
			t.Fatalf("covers cleared = %+v", shown)
		}
	}

	// A stop for a stream that never started is answered, so its handle is released.
	smokeBus.Post(StopCapture{Stream: 99, Display: d})

	if s, ok := next("stream stopped").(StreamEvent); !ok || s.Kind != EvStreamStopped || s.Stream != 99 {
		t.Fatalf("stream stopped = %+v", s)
	}
}
