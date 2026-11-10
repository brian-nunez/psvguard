//go:build darwin && cgo

package bridge

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework ScreenCaptureKit -framework CoreMedia -framework CoreVideo -framework CoreGraphics -framework QuartzCore
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>

// Defined in native_darwin.go. Declarations only: a file with //export may not define C code.
typedef struct { double x, y, w, h; } PGRect;

void pg_run(const char *version);
void pg_post(int kind, const void *data, size_t len);
int pg_screen_capture_granted(void);
PGRect pg_norm_to_points(double nx, double ny, double nw, double nh,
                         double fx, double fy, double fw, double fh, double scale);
*/
import "C"

import (
	"errors"
	"io"
	"log"
	"runtime"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"

	"github.com/brian-nunez/psvguard/internal/frame"
)

// AppKit needs the main thread. Package initialisation runs on it, so pin the main goroutine there;
// Run then hands the thread to the AppKit run loop.
func init() { runtime.LockOSThread() }

// darwinBus is the cgo bridge to AppKit and ScreenCaptureKit.
type darwinBus struct {
	q       *EventQueue
	out     *Outbox
	log     *log.Logger
	trace   bool
	version string

	mu      sync.Mutex
	handles map[uint64]cgo.Handle
}

// The exported callbacks have no receiver, so they reach the bus through this.
var (
	theBus   *darwinBus
	theBusMu sync.Mutex
)

// New returns the macOS bus. There can be only one per process.
func New(o Options) (Bus, error) {
	theBusMu.Lock()
	defer theBusMu.Unlock()

	if theBus != nil {
		return nil, errors.New("bridge: the bus already exists")
	}

	if o.Log == nil {
		o.Log = log.New(io.Discard, "", 0)
	}

	b := &darwinBus{
		q:       NewEventQueue(),
		log:     o.Log,
		trace:   o.Trace,
		version: o.Version,
		handles: map[uint64]cgo.Handle{},
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

func (b *darwinBus) Events() <-chan Event { return b.q.Events() }

func (b *darwinBus) Post(c Command) {
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

// Run hands the main thread to AppKit until a Quit command.
func (b *darwinBus) Run() error {
	done := make(chan struct{})
	go b.out.Run(done)

	v := C.CString(b.version)
	defer C.free(unsafe.Pointer(v))

	C.pg_run(v)
	close(done)

	return nil
}

func (b *darwinBus) push(e Event) {
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

// goEvent receives every event from the Objective-C side. It decodes, queues and returns.
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

// goCaptureFrame is the frame fast path, called on a ScreenCaptureKit queue while the pixel buffer
// is locked. It wraps the buffer without copying and lets the stream's frame target convert it
// into a pooled frame; nothing here allocates or keeps the pointer.
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

// screenCaptureGranted reports the Screen Recording permission without prompting.
func screenCaptureGranted() bool { return C.pg_screen_capture_granted() != 0 }

// normToPoints converts a normalised top-left rectangle on a display whose frame, in points with a
// bottom-left origin, is f, to points, rounded outward to whole device pixels at scale. It is the
// conversion the cover panels use.
func normToPoints(r Rect, f [4]float64, scale float64) [4]float64 {
	p := C.pg_norm_to_points(C.double(r.X), C.double(r.Y), C.double(r.W), C.double(r.H),
		C.double(f[0]), C.double(f[1]), C.double(f[2]), C.double(f[3]), C.double(scale))

	return [4]float64{float64(p.x), float64(p.y), float64(p.w), float64(p.h)}
}
