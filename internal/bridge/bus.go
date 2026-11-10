package bridge

import (
	"errors"
	"fmt"
	"log"
	"time"
)

// Bus carries events from the platform layer to the controller and commands back.
type Bus interface {
	// Events delivers events in the order the platform reported them. The first is a
	// CapabilitiesEvent.
	Events() <-chan Event
	// Post queues a command and returns at once. It is safe from any goroutine.
	Post(Command)
	// Run takes over the calling thread, the process's main thread, for the platform's run loop
	// and returns after a Quit command.
	Run() error
}

// Options configure a bus.
type Options struct {
	// Log receives diagnostics. Nil discards them.
	Log *log.Logger
	// Trace logs every event and command with a timestamp.
	Trace bool
	// Version is shown in the panel.
	Version string
}

// ErrUnsupported is returned by New on platforms without a bus.
var ErrUnsupported = errors.New("psvguard is not yet supported on this platform")

// TraceCommand formats a command for --trace-bridge.
func TraceCommand(c Command) string {
	switch c := c.(type) {
	case SetCovers:
		return fmt.Sprintf("cmd  %-18s display=%s rects=%d fade=%.2fs/%.2fs %v", kindName(c), c.Display, len(c.Rects), c.FadeIn, c.FadeOut, c.Rects)
	case StartCapture:
		return fmt.Sprintf("cmd  %-18s stream=%d display=%s %dx%d@%d %v", kindName(c), c.Stream, c.Display, c.Width, c.Height, c.FPS, c.Format)
	default:
		b, _ := EncodeCommand(c)
		if len(b) > 200 {
			b = append(b[:200:200], "…"...)
		}

		return fmt.Sprintf("cmd  %-18s %s", kindName(c), b)
	}
}

// TraceEvent formats an event for --trace-bridge.
func TraceEvent(e Event) string {
	_, b, _ := EncodeEvent(e)

	return fmt.Sprintf("evt  %-18T %s", e, b)
}

func kindName(c Command) string { return fmt.Sprintf("%T", c) }

// Stamp prefixes a trace line with a millisecond timestamp.
func Stamp(t time.Time, line string) string {
	return t.Format("15:04:05.000") + " " + line
}
