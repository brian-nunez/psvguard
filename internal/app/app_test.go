package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/brian-nunez/psvguard/internal/bridge"
	"github.com/brian-nunez/psvguard/internal/frame"
	"github.com/brian-nunez/psvguard/internal/settings"
	"github.com/brian-nunez/psvguard/internal/ui"
)

// fakeBus records commands and injects events.
type fakeBus struct {
	events chan bridge.Event

	mu   sync.Mutex
	cmds []bridge.Command
	seen int // commands already matched by next
}

func newFakeBus() *fakeBus { return &fakeBus{events: make(chan bridge.Event, 64)} }

func (b *fakeBus) Events() <-chan bridge.Event { return b.events }
func (b *fakeBus) Run() error                  { return nil }

func (b *fakeBus) Post(c bridge.Command) {
	b.mu.Lock()
	b.cmds = append(b.cmds, c)
	b.mu.Unlock()
}

func (b *fakeBus) send(e bridge.Event) { b.events <- e }

// next waits for the first command after the last match that satisfies ok.
func (b *fakeBus) next(t *testing.T, what string, ok func(bridge.Command) bool) bridge.Command {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		b.mu.Lock()
		for i := b.seen; i < len(b.cmds); i++ {
			if ok(b.cmds[i]) {
				c := b.cmds[i]
				b.seen = i + 1
				b.mu.Unlock()

				return c
			}
		}
		b.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s; commands: %v", what, b.all())

	return nil
}

// none fails if a command satisfying ok was posted after the last match.
func (b *fakeBus) none(t *testing.T, what string, ok func(bridge.Command) bool) {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	for _, c := range b.cmds[b.seen:] {
		if ok(c) {
			t.Fatalf("unexpected %s: %#v", what, c)
		}
	}
}

func (b *fakeBus) all() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, len(b.cmds))
	for i, c := range b.cmds {
		out[i] = bridge.TraceCommand(c)
	}

	return out
}

func is[T bridge.Command](c bridge.Command) bool {
	_, ok := c.(T)

	return ok
}

func status(state ui.State) func(bridge.Command) bool {
	return func(c bridge.Command) bool {
		s, ok := c.(bridge.SetStatus)

		return ok && s.State == state
	}
}

type memStore struct {
	mu    sync.Mutex
	saved []settings.Settings
}

func (m *memStore) Save(s settings.Settings) error {
	m.mu.Lock()
	m.saved = append(m.saved, s)
	m.mu.Unlock()

	return nil
}

var (
	macCaps = bridge.Capabilities{
		ExcludesOwnWindows: true, CanOverlay: true, PerDisplayCapture: true,
		PixelFormats: []string{"BGRA"},
	}
	linuxCaps = bridge.Capabilities{
		ExcludesOwnWindows: false, CanOverlay: false, PerDisplayCapture: true, ConsentPerSession: true,
		PixelFormats: []string{"BGRx"},
	}
	display1 = bridge.Display{ID: "1", Name: "Built-in", Width: 640, Height: 360, PixelWidth: 1280, PixelHeight: 720, Main: true}
	display2 = bridge.Display{ID: "2", Name: "External", Width: 1920, Height: 1080, PixelWidth: 1920, PixelHeight: 1080}
)

func testSettings() settings.Settings {
	s := settings.Defaults()
	s.CaptureWidth = 64 // tiny frames keep the tests fast
	s.CalmSeconds = 1
	s.ShowPanel = false

	return s
}

type harness struct {
	bus    *fakeBus
	store  *memStore
	done   chan error
	cancel context.CancelFunc
}

func start(t *testing.T, caps bridge.Capabilities) *harness {
	t.Helper()

	h := &harness{bus: newFakeBus(), store: &memStore{}, done: make(chan error, 1)}
	c := New(Options{Bus: h.bus, Settings: testSettings(), Store: h.store, StatsEvery: 50 * time.Millisecond, RetryEvery: 100 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel

	go func() { h.done <- c.Run(ctx) }()

	t.Cleanup(func() {
		cancel()

		if err := <-h.done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	})

	h.bus.send(bridge.CapabilitiesEvent{Capabilities: caps})
	h.bus.send(bridge.ReadyEvent{})

	return h
}

// grant completes the permission flow with the given displays and returns their StartCaptures.
func (h *harness) grant(t *testing.T, displays ...bridge.Display) map[string]bridge.StartCapture {
	t.Helper()

	h.bus.next(t, "permission check", is[bridge.CheckPermission])
	h.bus.send(bridge.DisplaysEvent{Displays: displays})
	h.bus.send(bridge.PermissionEvent{State: bridge.PermissionGranted})

	out := map[string]bridge.StartCapture{}

	for range displays {
		sc := h.bus.next(t, "start capture", is[bridge.StartCapture]).(bridge.StartCapture)
		out[sc.Display] = sc
		h.bus.send(bridge.StreamEvent{Kind: bridge.EvStreamStarted, Stream: sc.Stream, Display: sc.Display})
	}

	return out
}

// flash feeds full-frame white/black flashing at five flashes a second until stop is closed.
func flash(sc bridge.StartCapture, stop <-chan struct{}) {
	n := sc.Width * sc.Height * 4
	white, black := make([]byte, n), make([]byte, n)

	for i := range white {
		white[i] = 255
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for i := int64(0); ; i++ {
		select {
		case <-stop:
			return
		case <-tick.C:
			src := white
			if i%2 == 1 {
				src = black
			}

			sc.Target.CaptureFrame(sc.Format, src, sc.Width, sc.Height, sc.Width*4, i*int64(100*time.Millisecond))
		}
	}
}

func TestPermissionFlowAsksOnceThenStartsCapture(t *testing.T) {
	h := start(t, macCaps)

	h.bus.next(t, "controls", is[bridge.SetControls])
	h.bus.next(t, "permission check", is[bridge.CheckPermission])
	h.bus.send(bridge.DisplaysEvent{Displays: []bridge.Display{display1}})
	h.bus.send(bridge.PermissionEvent{State: bridge.PermissionNeedsConsent})

	h.bus.next(t, "permission request", is[bridge.RequestPermission])
	h.bus.next(t, "permission status", status(ui.Permission))
	h.bus.none(t, "capture before permission", is[bridge.StartCapture])

	// The user asks for the settings pane, then grants permission.
	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.OpenPermission}})
	h.bus.next(t, "open permission pane", is[bridge.OpenPermissionPane])
	h.bus.send(bridge.PermissionEvent{State: bridge.PermissionNeedsConsent})
	h.bus.send(bridge.PermissionEvent{State: bridge.PermissionGranted})

	sc := h.bus.next(t, "start capture", is[bridge.StartCapture]).(bridge.StartCapture)
	if sc.Display != "1" || sc.Width != 64 || sc.Height != 36 || sc.FPS != 60 || sc.Format != frame.BGRA || sc.Target == nil {
		t.Fatalf("start capture = %+v", sc)
	}

	h.bus.next(t, "watching status", status(ui.Watching))

	// Asked once only.
	h.bus.mu.Lock()
	asks := 0

	for _, c := range h.bus.cmds {
		if is[bridge.RequestPermission](c) {
			asks++
		}
	}
	h.bus.mu.Unlock()

	if asks != 1 {
		t.Fatalf("asked for permission %d times", asks)
	}
}

func TestRevokedPermissionStopsCapture(t *testing.T) {
	h := start(t, macCaps)
	sc := h.grant(t, display1)["1"]

	h.bus.send(bridge.StreamEvent{Kind: bridge.EvStreamFailed, Stream: sc.Stream, Display: "1", Error: "user declined"})
	h.bus.next(t, "permission recheck", is[bridge.CheckPermission])
	h.bus.send(bridge.PermissionEvent{State: bridge.PermissionDenied})
	h.bus.next(t, "permission status", status(ui.Permission))
}

func TestFlashingIsCoveredThenUncovered(t *testing.T) {
	h := start(t, macCaps)
	sc := h.grant(t, display1)["1"]

	stop := make(chan struct{})
	go flash(sc, stop)

	c := h.bus.next(t, "covers", func(c bridge.Command) bool {
		s, ok := c.(bridge.SetCovers)

		return ok && len(s.Rects) > 0
	}).(bridge.SetCovers)

	if c.Display != "1" || c.FadeIn != FadeIn || c.FadeOut != FadeOut || c.Rects[0] != (bridge.Rect{X: 0, Y: 0, W: 1, H: 1}) {
		t.Fatalf("covers = %+v", c)
	}

	h.bus.next(t, "covering status", status(ui.Covering))
	close(stop)

	// After the calm time the covers go.
	h.bus.next(t, "uncover", func(c bridge.Command) bool {
		s, ok := c.(bridge.SetCovers)

		return ok && len(s.Rects) == 0
	})
	h.bus.next(t, "watching status", status(ui.Watching))
}

func TestDisplayTransitionWithdrawsCovers(t *testing.T) {
	h := start(t, macCaps)
	sc := h.grant(t, display1)["1"]

	stop := make(chan struct{})
	go flash(sc, stop)

	h.bus.next(t, "covers", func(c bridge.Command) bool {
		s, ok := c.(bridge.SetCovers)

		return ok && len(s.Rects) > 0
	})
	close(stop)

	// A workspace switch right after: the cover it may have caused is withdrawn at once.
	h.bus.send(bridge.Transition{At: 1})
	h.bus.next(t, "withdrawn", func(c bridge.Command) bool {
		s, ok := c.(bridge.SetCovers)

		return ok && len(s.Rects) == 0
	})
}

func TestHotPlugAndResolutionChange(t *testing.T) {
	h := start(t, macCaps)
	first := h.grant(t, display1)["1"]

	// A second display arrives.
	h.bus.send(bridge.DisplaysEvent{Displays: []bridge.Display{display1, display2}})

	sc2 := h.bus.next(t, "start capture for display 2", is[bridge.StartCapture]).(bridge.StartCapture)
	if sc2.Display != "2" {
		t.Fatalf("started %+v", sc2)
	}

	// Display 1 changes shape: its analyser is replaced.
	portrait := display1
	portrait.Width, portrait.Height = 360, 640
	h.bus.send(bridge.DisplaysEvent{Displays: []bridge.Display{portrait, display2}})

	stop := h.bus.next(t, "stop old stream", is[bridge.StopCapture]).(bridge.StopCapture)
	if stop.Stream != first.Stream {
		t.Fatalf("stopped %+v; want stream %d", stop, first.Stream)
	}

	restarted := h.bus.next(t, "restart display 1", is[bridge.StartCapture]).(bridge.StartCapture)
	if restarted.Display != "1" || restarted.Width != 64 || restarted.Height != 114 {
		t.Fatalf("restarted %+v", restarted)
	}

	h.bus.send(bridge.StreamEvent{Kind: bridge.EvStreamStopped, Stream: first.Stream, Display: "1"})

	// Display 2 goes away.
	h.bus.send(bridge.DisplaysEvent{Displays: []bridge.Display{portrait}})

	gone := h.bus.next(t, "stop display 2", is[bridge.StopCapture]).(bridge.StopCapture)
	if gone.Stream != sc2.Stream {
		t.Fatalf("stopped %+v; want display 2's stream", gone)
	}
}

func TestPauseAndResume(t *testing.T) {
	h := start(t, macCaps)
	sc := h.grant(t, display1)["1"]

	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.Toggle}})

	if stop := h.bus.next(t, "stop capture", is[bridge.StopCapture]).(bridge.StopCapture); stop.Stream != sc.Stream {
		t.Fatalf("stopped %+v", stop)
	}

	h.bus.next(t, "paused status", status(ui.Paused))

	h.store.mu.Lock()
	if n := len(h.store.saved); n != 1 || h.store.saved[0].Enabled {
		t.Fatalf("saved %+v", h.store.saved)
	}
	h.store.mu.Unlock()

	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.Toggle}})
	h.bus.next(t, "capture restarted", is[bridge.StartCapture])
}

func TestSettingsChangesApplyLive(t *testing.T) {
	h := start(t, macCaps)
	sc := h.grant(t, display1)["1"]

	// A policy setting keeps the stream.
	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.SetSetting, Key: "calm", Value: "5"}})
	h.bus.next(t, "controls", is[bridge.SetControls])
	h.bus.none(t, "restart for a policy setting", is[bridge.StopCapture])

	// A threshold preset restarts the analyser.
	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.SetSetting, Key: "preset", Value: "trace24"}})

	if stop := h.bus.next(t, "restart", is[bridge.StopCapture]).(bridge.StopCapture); stop.Stream != sc.Stream {
		t.Fatalf("stopped %+v", stop)
	}

	h.bus.next(t, "new stream", is[bridge.StartCapture])

	// An invalid value is reported and changes nothing.
	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.SetSetting, Key: "hot-peak", Value: "99"}})
	h.bus.next(t, "error detail", func(c bridge.Command) bool {
		s, ok := c.(bridge.SetStatus)

		return ok && s.Detail != "" && contains(s.Detail, "hot-peak")
	})
}

func TestResetSettingsRestoresDefaultsAndKeepsOnOff(t *testing.T) {
	h := start(t, macCaps)
	h.grant(t, display1)

	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.SetSetting, Key: "calm", Value: "7"}})
	h.bus.next(t, "controls", is[bridge.SetControls])
	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.Toggle}})
	h.bus.next(t, "capture stopped", is[bridge.StopCapture])

	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.ResetSettings}})
	h.bus.next(t, "controls", is[bridge.SetControls])
	h.bus.none(t, "capture while off", is[bridge.StartCapture])

	want := settings.Defaults()
	want.Enabled = false

	h.store.mu.Lock()
	got := h.store.saved[len(h.store.saved)-1]
	h.store.mu.Unlock()

	if got != want {
		t.Fatalf("saved %+v, want %+v", got, want)
	}

	// Resetting again changes nothing but still refreshes the panel.
	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.ResetSettings}})
	h.bus.next(t, "controls", is[bridge.SetControls])

	h.store.mu.Lock()
	n := len(h.store.saved)
	h.store.mu.Unlock()

	if n != 3 {
		t.Fatalf("saved %d times, want 3", n)
	}
}

func TestSleepStopsAndWakeRestarts(t *testing.T) {
	h := start(t, macCaps)
	h.grant(t, display1)

	h.bus.send(bridge.PowerEvent{Sleeping: true})
	h.bus.next(t, "stop on sleep", is[bridge.StopCapture])
	h.bus.next(t, "paused status", status(ui.Paused))

	h.bus.send(bridge.PowerEvent{Sleeping: false})
	h.bus.next(t, "permission check on wake", is[bridge.CheckPermission])
	h.bus.send(bridge.PermissionEvent{State: bridge.PermissionGranted})
	h.bus.next(t, "restart on wake", is[bridge.StartCapture])
}

func TestQuitStopsEverythingThenQuits(t *testing.T) {
	h := start(t, macCaps)
	h.grant(t, display1, display2)

	h.bus.send(bridge.ActionEvent{Action: ui.Action{Kind: ui.Quit}})

	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}

		h.done <- nil // for Cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after quit")
	}

	h.bus.next(t, "stop 1", is[bridge.StopCapture])
	h.bus.next(t, "stop 2", is[bridge.StopCapture])
	h.bus.next(t, "clear all covers", func(c bridge.Command) bool {
		cc, ok := c.(bridge.ClearCovers)

		return ok && cc.Display == ""
	})
	h.bus.next(t, "quit", is[bridge.Quit])
}

func TestStatsAreReported(t *testing.T) {
	h := start(t, macCaps)
	sc := h.grant(t, display1)["1"]

	stop := make(chan struct{})
	defer close(stop)

	go flash(sc, stop)

	h.bus.next(t, "stats with frames", func(c bridge.Command) bool {
		s, ok := c.(bridge.SetStats)

		return ok && len(s.Displays) == 1 && s.Displays[0].FPS > 0 && s.Displays[0].WatchMs > 0
	})
}

// Linux-like platforms: covers show up in captures, overlays may be impossible and consent is
// needed every session.

func TestLinuxLikeAsksForConsentEverySession(t *testing.T) {
	h := start(t, linuxCaps)

	h.bus.next(t, "permission check", is[bridge.CheckPermission])
	h.bus.send(bridge.DisplaysEvent{Displays: []bridge.Display{display1}})
	h.bus.send(bridge.PermissionEvent{State: bridge.PermissionNeedsConsent})
	h.bus.next(t, "consent request", is[bridge.RequestPermission])
	h.bus.send(bridge.PermissionEvent{State: bridge.PermissionGranted})

	sc := h.bus.next(t, "start capture", is[bridge.StartCapture]).(bridge.StartCapture)
	if sc.Format != frame.BGRX {
		t.Fatalf("format %v; want the platform's BGRx", sc.Format)
	}
}

func TestLinuxLikeWarnsWhenItCannotOverlay(t *testing.T) {
	h := start(t, linuxCaps)
	sc := h.grant(t, display1)["1"]

	stop := make(chan struct{})
	defer close(stop)

	go flash(sc, stop)

	n := h.bus.next(t, "notification", is[bridge.Notify]).(bridge.Notify)
	if n.Title == "" || n.Body == "" {
		t.Fatalf("notification %+v", n)
	}

	h.bus.next(t, "warning status", status(ui.Warning))
	h.bus.none(t, "covers without overlay support", is[bridge.SetCovers])
}

func TestLinuxLikeCoversDoNotKeepThemselvesAlive(t *testing.T) {
	caps := linuxCaps
	caps.CanOverlay = true // X11-like: overlays work, but captures include them
	h := start(t, caps)
	sc := h.grant(t, display1)["1"]

	stop := make(chan struct{})
	go flash(sc, stop)

	h.bus.next(t, "covers", func(c bridge.Command) bool {
		s, ok := c.(bridge.SetCovers)

		return ok && len(s.Rects) > 0
	})
	close(stop)

	// The captured frames would now show the cover; feed a still dark frame like a cover would
	// produce and expect the cover to leave after the calm time rather than sustain itself.
	dark := make([]byte, sc.Width*sc.Height*4)
	sc.Target.CaptureFrame(sc.Format, dark, sc.Width, sc.Height, sc.Width*4, int64(time.Hour))

	h.bus.next(t, "uncover", func(c bridge.Command) bool {
		s, ok := c.(bridge.SetCovers)

		return ok && len(s.Rects) == 0
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}
