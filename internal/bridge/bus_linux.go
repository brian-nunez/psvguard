//go:build linux && cgo

package bridge

/*
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>

// Defined in native_linux.go. Declarations only: a file with //export may not define C code.
typedef struct { double x, y, w, h; } PGRect;

int pg_run(const char *version, const char *statePath);
void pg_post(int kind, const void *data, size_t len);
PGRect pg_norm_to_px(double nx, double ny, double nw, double nh, double width, double height, double scale);
char *pg_json_roundtrip(const char *data, size_t len);
void g_free(void *p);
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"

	"github.com/brian-nunez/psvguard/internal/frame"
)

// GTK needs the thread it was initialised on. Package initialisation runs on the main thread, so
// pin the main goroutine there; Run then hands the thread to the GTK main loop.
func init() { runtime.LockOSThread() }

// ErrNoWayland is returned by New outside a Wayland session.
var ErrNoWayland = errors.New("PSV Guard needs a Wayland session; X11 sessions are not supported")

// linuxBus is the cgo bridge to GTK, the screen cast portal and PipeWire.
type linuxBus struct {
	q         *EventQueue
	out       *Outbox
	log       *log.Logger
	trace     bool
	version   string
	statePath string

	mu      sync.Mutex
	handles map[uint64]cgo.Handle
}

// The exported callbacks have no receiver, so they reach the bus through this.
var (
	theBus   *linuxBus
	theBusMu sync.Mutex
)

// New returns the Linux bus. There can be only one per process.
func New(o Options) (Bus, error) {
	theBusMu.Lock()
	defer theBusMu.Unlock()

	if theBus != nil {
		return nil, errors.New("bridge: the bus already exists")
	}

	if os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, ErrNoWayland
	}

	if o.Log == nil {
		o.Log = log.New(io.Discard, "", 0)
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("find the settings folder: %w", err)
	}

	b := &linuxBus{
		q:         NewEventQueue(),
		log:       o.Log,
		trace:     o.Trace,
		version:   o.Version,
		statePath: filepath.Join(dir, "psvguard", "state.json"),
		handles:   map[uint64]cgo.Handle{},
	}

	var trace func(Command)
	if o.Trace {
		trace = func(c Command) { b.log.Print(Stamp(time.Now(), TraceCommand(c))) }
	}

	b.out = NewOutbox(post, trace)
	theBus = b

	return b, nil
}

// post is the Outbox's sender: pg_post copies the payload and returns.
func post(kind CommandKind, payload []byte) {
	var p unsafe.Pointer
	if len(payload) > 0 {
		p = unsafe.Pointer(&payload[0])
	}

	C.pg_post(C.int(kind), p, C.size_t(len(payload)))
}

func (b *linuxBus) Events() <-chan Event { return b.q.Events() }

func (b *linuxBus) Post(c Command) {
	if sc, ok := c.(StartCapture); ok {
		h := cgo.NewHandle(sc.Target)
		sc.Handle = uint64(h)

		b.mu.Lock()
		b.handles[sc.Stream] = h
		b.mu.Unlock()

		c = sc
	}

	b.out.Post(c)
}

// Run hands the main thread to the GTK main loop until a Quit command.
func (b *linuxBus) Run() error {
	done := make(chan struct{})
	go b.out.Run(done)

	v, s := C.CString(b.version), C.CString(b.statePath)
	defer C.free(unsafe.Pointer(v))
	defer C.free(unsafe.Pointer(s))

	rc := C.pg_run(v, s)
	close(done)

	if rc != 0 {
		return ErrNoWayland
	}

	return nil
}

func (b *linuxBus) push(e Event) {
	if b.trace {
		b.log.Print(Stamp(time.Now(), TraceEvent(e)))
	}

	if s, ok := e.(StreamEvent); ok && s.Kind == EvStreamStopped {
		// The platform sends no more frames for a stopped stream, so its handle can go.
		b.mu.Lock()
		if h, ok := b.handles[s.Stream]; ok {
			h.Delete()
			delete(b.handles, s.Stream)
		}
		b.mu.Unlock()
	}

	b.q.Push(e)
}

// goEvent receives every event from the C side. It decodes, queues and returns.
//
//export goEvent
func goEvent(kind C.int, data unsafe.Pointer, n C.size_t) {
	b := theBus
	if b == nil {
		return
	}

	var payload []byte
	if n > 0 && data != nil {
		payload = C.GoBytes(data, C.int(n))
	}

	e, err := DecodeEvent(EventKind(kind), payload)
	if err != nil {
		b.log.Printf("%v", err)

		return
	}

	b.push(e)
}

// goCaptureFrame is the frame fast path, called on the PipeWire thread while the buffer is
// dequeued. It wraps the buffer without copying and lets the stream's frame target convert it into
// a pooled frame; nothing here allocates or keeps the pointer.
//
//export goCaptureFrame
func goCaptureFrame(handle C.uintptr_t, pixelFormat C.int, base *C.uint8_t, width, height, bytesPerRow C.int, ptsNanos C.int64_t) {
	if base == nil || height <= 0 || bytesPerRow <= 0 {
		return
	}

	t, ok := cgo.Handle(handle).Value().(FrameTarget)
	if !ok {
		return
	}

	src := unsafe.Slice((*byte)(unsafe.Pointer(base)), int(height)*int(bytesPerRow))
	t.CaptureFrame(frame.Format(pixelFormat), src, int(width), int(height), int(bytesPerRow), int64(ptsNanos))
}

// normToPx converts a normalised top-left rectangle on a display of size logical pixels to logical
// pixels, rounded outward to whole device pixels at scale. It is the conversion the covers use.
func normToPx(r Rect, size [2]float64, scale float64) [4]float64 {
	p := C.pg_norm_to_px(C.double(r.X), C.double(r.Y), C.double(r.W), C.double(r.H),
		C.double(size[0]), C.double(size[1]), C.double(scale))

	return [4]float64{float64(p.x), float64(p.y), float64(p.w), float64(p.h)}
}

// jsonRoundTrip parses a payload as the C side parses commands and writes it back as it writes
// events.
func jsonRoundTrip(s string) string {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))

	out := C.pg_json_roundtrip(cs, C.size_t(len(s)))
	defer C.g_free(unsafe.Pointer(out))

	return C.GoString(out)
}
