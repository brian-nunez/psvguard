package bridge

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/brian-nunez/psvguard/internal/frame"
	"github.com/brian-nunez/psvguard/internal/ui"
)

func TestCoversLayout(t *testing.T) {
	c := SetCovers{Display: "69733378", FadeIn: 0.15, FadeOut: 1.5, Rects: []Rect{{X: 0.25, Y: 0.5, W: 0.125, H: 0.0625}}}

	b, err := EncodeCovers(c)
	if err != nil {
		t.Fatal(err)
	}

	// Check the documented layout byte by byte.
	if n := binary.LittleEndian.Uint16(b); n != 8 {
		t.Fatalf("id length %d", n)
	}

	if string(b[2:10]) != "69733378" {
		t.Fatalf("id %q", b[2:10])
	}

	f := func(off int) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(b[off:])) }

	if f(10) != 0.15 || f(18) != 1.5 {
		t.Fatalf("fades %v %v", f(10), f(18))
	}

	if k := binary.LittleEndian.Uint32(b[26:]); k != 1 {
		t.Fatalf("count %d", k)
	}

	if f(30) != 0.25 || f(38) != 0.5 || f(46) != 0.125 || f(54) != 0.0625 || len(b) != 62 {
		t.Fatalf("rect bytes wrong: %v", b[30:])
	}

	got, err := DecodeCovers(b)
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
}

func TestDecodeCoversRejectsTruncation(t *testing.T) {
	b, _ := EncodeCovers(SetCovers{Display: "1", Rects: []Rect{{X: 0, Y: 0, W: 1, H: 1}, {X: 0, Y: 0, W: 0.5, H: 0.5}}})

	for _, n := range []int{0, 1, 5, 20, len(b) - 1} {
		if _, err := DecodeCovers(b[:n]); !errors.Is(err, ErrCovers) {
			t.Errorf("%d bytes: got %v", n, err)
		}
	}
}

func TestEventsRoundTrip(t *testing.T) {
	events := []Event{
		CapabilitiesEvent{Capabilities{ExcludesOwnWindows: true, CanOverlay: true, PerDisplayCapture: true, PixelFormats: []string{"BGRA"}}},
		ReadyEvent{},
		ActionEvent{ui.Action{Kind: ui.SetSetting, Key: "calm", Value: "4"}},
		DisplaysEvent{[]Display{{ID: "1", Name: "Built-in", Width: 1512, Height: 982, PixelWidth: 3024, PixelHeight: 1964, Main: true}}},
		PermissionEvent{State: PermissionNeedsConsent},
		StreamEvent{Kind: EvStreamStarted, Stream: 3, Display: "1"},
		StreamEvent{Kind: EvStreamFailed, Stream: 3, Display: "1", Error: "user declined"},
		StreamEvent{Kind: EvStreamStopped, Stream: 3, Display: "1"},
		PowerEvent{Sleeping: true},
		CoversShown{Display: "1", Rects: 2, At: 123456789},
		Transition{At: 42},
	}

	for _, e := range events {
		kind, b, err := EncodeEvent(e)
		if err != nil {
			t.Fatal(err)
		}

		got, err := DecodeEvent(kind, b)
		if err != nil {
			t.Fatalf("%T: %v", e, err)
		}

		if !reflect.DeepEqual(got, e) {
			t.Errorf("round trip %T: got %+v, want %+v", e, got, e)
		}
	}

	if _, err := DecodeEvent(99, nil); err == nil {
		t.Error("unknown kind decoded")
	}

	if _, err := DecodeEvent(EvDisplays, []byte("{")); err == nil {
		t.Error("bad JSON decoded")
	}
}

func TestCommandPayloads(t *testing.T) {
	b, err := EncodeCommand(StartCapture{Stream: 7, Display: "2", Width: 640, Height: 360, FPS: 60, Format: frame.BGRA, Handle: 42})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"stream":7,"display":"2","width":640,"height":360,"fps":60,"format":1,"handle":42}`
	if string(b) != want {
		t.Fatalf("start capture = %s", b)
	}

	if b, _ := EncodeCommand(Quit{}); b != nil {
		t.Fatalf("quit payload %q", b)
	}

	if b, _ := EncodeCommand(SetControls{}); string(b) != "[]" {
		t.Fatalf("empty controls = %s", b)
	}
}

func TestCapabilitiesFormat(t *testing.T) {
	if f, ok := (Capabilities{PixelFormats: []string{"NV12", "BGRx"}}).Format(); !ok || f != frame.BGRX {
		t.Fatalf("format = %v %v", f, ok)
	}

	if _, ok := (Capabilities{PixelFormats: []string{"NV12"}}).Format(); ok {
		t.Fatal("unsupported format accepted")
	}
}

type sent struct {
	kind    CommandKind
	payload []byte
}

func newTestOutbox() (*Outbox, *[]sent) {
	var out []sent

	o := NewOutbox(func(k CommandKind, p []byte) { out = append(out, sent{k, append([]byte(nil), p...)}) }, nil)

	return o, &out
}

func TestOutboxCoalescesCoversPerDisplay(t *testing.T) {
	o, out := newTestOutbox()
	now := time.Unix(1000, 0)

	o.Post(SetCovers{Display: "1", Rects: []Rect{{X: 0, Y: 0, W: 0.1, H: 0.1}}})
	o.Post(SetCovers{Display: "2", Rects: []Rect{{X: 0, Y: 0, W: 0.2, H: 0.2}}})
	o.Post(SetCovers{Display: "1", Rects: []Rect{{X: 0, Y: 0, W: 0.3, H: 0.3}}})
	o.Drain(now)

	if len(*out) != 2 {
		t.Fatalf("sent %d commands, want 2", len(*out))
	}

	c, _ := DecodeCovers((*out)[0].payload)
	if c.Display != "1" || c.Rects[0].W != 0.3 {
		t.Fatalf("first = %+v; want display 1's latest covers", c)
	}
}

func TestOutboxSkipsUnchangedCovers(t *testing.T) {
	o, out := newTestOutbox()
	now := time.Unix(1000, 0)
	r := []Rect{{X: 0, Y: 0, W: 1, H: 1}}

	o.Post(SetCovers{Display: "1", Rects: r})
	o.Drain(now)
	o.Post(SetCovers{Display: "1", Rects: r})
	o.Drain(now)

	if len(*out) != 1 {
		t.Fatalf("sent %d, want 1: unchanged covers resent", len(*out))
	}

	// Clearing a display that shows nothing sends nothing; clearing one that shows covers does.
	o.Post(ClearCovers{Display: "9"})
	o.Post(SetCovers{Display: "1"})
	o.Drain(now)

	if len(*out) != 2 || (*out)[1].kind != CmdClearCovers {
		t.Fatalf("sent %+v", *out)
	}
}

func TestOutboxThrottlesStatus(t *testing.T) {
	o, out := newTestOutbox()
	now := time.Unix(1000, 0)

	o.Post(SetStatus{ui.Status{Title: "a"}})
	o.Drain(now)
	o.Post(SetStatus{ui.Status{Title: "b"}})
	o.Post(SetStatus{ui.Status{Title: "c"}})

	next := o.Drain(now.Add(100 * time.Millisecond))

	if len(*out) != 1 {
		t.Fatalf("status not throttled: %d sent", len(*out))
	}

	if want := now.Add(o.StatusGap); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}

	o.Drain(next)

	if len(*out) != 2 || string((*out)[1].payload) == "" {
		t.Fatalf("throttled status not sent: %d", len(*out))
	}

	if s := string((*out)[1].payload); !contains(s, `"title":"c"`) {
		t.Fatalf("sent %s; want the latest status", s)
	}
}

func TestOutboxKeepsOrderedCommandsInOrder(t *testing.T) {
	o, out := newTestOutbox()

	o.Post(StopCapture{Stream: 1, Display: "1"})
	o.Post(ClearCovers{})
	o.Post(Quit{})
	o.Drain(time.Unix(1000, 0))

	kinds := []CommandKind{}
	for _, s := range *out {
		kinds = append(kinds, s.kind)
	}

	if !reflect.DeepEqual(kinds, []CommandKind{CmdStopCapture, CmdClearCovers, CmdQuit}) {
		t.Fatalf("order = %v", kinds)
	}
}

func TestOutboxRunDeliversFromOneGoroutine(t *testing.T) {
	got := make(chan CommandKind, 10)
	o := NewOutbox(func(k CommandKind, _ []byte) { got <- k }, nil)
	done := make(chan struct{})

	go o.Run(done)

	o.Post(ShowPanel{})

	select {
	case k := <-got:
		if k != CmdShowPanel {
			t.Fatalf("got %d", k)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not deliver")
	}

	close(done)
}

func TestEventQueuePreservesOrderWithoutBlocking(t *testing.T) {
	q := NewEventQueue()

	for i := range 1000 {
		q.Push(StreamEvent{Kind: EvStreamStarted, Stream: uint64(i)})
	}

	for i := range 1000 {
		e := (<-q.Events()).(StreamEvent)
		if e.Stream != uint64(i) {
			t.Fatalf("event %d out of order: %d", i, e.Stream)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}
