// Package bridge is the platform contract: the events a platform layer reports, the commands the
// controller posts back, their payload formats, and the Bus that carries them. The contract has no
// platform concepts in it: display IDs are opaque strings, geometry is normalised with a top-left
// origin, pixel formats are an enum and permission is a generic state.
//
// Traffic is one-way and asynchronous in both directions. A platform layer reports events through
// goEvent (and frames through goCaptureFrame); the controller decides and posts commands through
// pg_post. Nothing waits for the other side and nothing returns a result across the bridge.
//
// Low-rate payloads are JSON. Cover commands use the packed little-endian layout documented on
// EncodeCovers. Pixels cross only on the frame path.
package bridge

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/brian-nunez/psvguard/internal/coverage"
	"github.com/brian-nunez/psvguard/internal/frame"
	"github.com/brian-nunez/psvguard/internal/ui"
)

// EventKind identifies an event. The values must match every platform layer
// (the PGEvent enum in native_darwin.go).
type EventKind int

// Event kinds.
const (
	EvCapabilities  EventKind = 1  // JSON Capabilities
	EvReady         EventKind = 2  // no payload: the platform can take commands
	EvAction        EventKind = 3  // JSON ui.Action
	EvDisplays      EventKind = 4  // JSON []Display: the connected displays
	EvPermission    EventKind = 5  // JSON {"state": Permission}
	EvStreamStarted EventKind = 6  // JSON StreamEvent
	EvStreamStopped EventKind = 7  // JSON StreamEvent; the stream's frame target is released
	EvStreamFailed  EventKind = 8  // JSON StreamEvent with Error; followed by EvStreamStopped
	EvPower         EventKind = 9  // JSON {"sleeping": bool}
	EvCoversShown   EventKind = 10 // JSON CoversShown: a cover command took effect on screen
	EvTransition    EventKind = 11 // JSON Transition: a system animation moves the whole display
)

// CommandKind identifies a command. The values must match every platform layer
// (the PGCommand enum in native_darwin.go).
type CommandKind int

// Command kinds.
const (
	CmdSetCovers          CommandKind = 1  // binary, see EncodeCovers
	CmdClearCovers        CommandKind = 2  // JSON {"display": id}; "" clears every display
	CmdStatus             CommandKind = 3  // JSON ui.Status
	CmdControls           CommandKind = 4  // JSON []ui.Control
	CmdStats              CommandKind = 5  // JSON ui.Stats
	CmdStartCapture       CommandKind = 6  // JSON StartCapture
	CmdStopCapture        CommandKind = 7  // JSON StopCapture
	CmdCheckPermission    CommandKind = 8  // no payload; answered by EvPermission
	CmdRequestPermission  CommandKind = 9  // no payload; answered by EvPermission
	CmdOpenPermissionPane CommandKind = 10 // no payload
	CmdNotify             CommandKind = 11 // JSON Notify
	CmdShowPanel          CommandKind = 12 // no payload
	CmdQuit               CommandKind = 13 // no payload
)

// Permission is the state of the platform's screen capture permission.
type Permission string

// Permission states.
const (
	PermissionUnknown      Permission = "unknown"
	PermissionGranted      Permission = "granted"
	PermissionDenied       Permission = "denied"
	PermissionNeedsConsent Permission = "needsConsent"
)

// Capabilities describe what a platform layer can do. The controller adapts to them instead of
// assuming one platform's behaviour.
type Capabilities struct {
	// ExcludesOwnWindows reports that capture leaves out psvguard's own windows, the covers.
	ExcludesOwnWindows bool `json:"excludesOwnWindows"`
	// CanOverlay reports that click-through covers can be drawn over other apps.
	CanOverlay bool `json:"canOverlay"`
	// PerDisplayCapture reports one stream per display rather than one combined stream.
	PerDisplayCapture bool `json:"perDisplayCapture"`
	// ConsentPerSession reports that the user must approve capture every session.
	ConsentPerSession bool `json:"consentPerSession"`
	// PixelFormats lists the formats the layer can deliver, by frame.Format name.
	PixelFormats []string `json:"pixelFormats"`
}

// Format returns the first pixel format psvguard can convert, or false.
func (c Capabilities) Format() (frame.Format, bool) {
	for _, name := range c.PixelFormats {
		if f, err := frame.ParseFormat(name); err == nil {
			return f, true
		}
	}

	return 0, false
}

// Display is one connected display.
type Display struct {
	// ID is opaque and stable while the display stays connected.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Width and Height are the display's size in the platform's logical units (points on macOS),
	// the units in which WCAG's reference pixels are measured.
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	// PixelWidth and PixelHeight are its size in device pixels.
	PixelWidth  int  `json:"pixelWidth"`
	PixelHeight int  `json:"pixelHeight"`
	Main        bool `json:"main"`
}

// Event is something the platform layer reports.
type Event interface{ EventKind() EventKind }

// CapabilitiesEvent reports the platform's capabilities. It is the first event a bus sends.
type CapabilitiesEvent struct{ Capabilities }

// ReadyEvent reports that the platform can take commands.
type ReadyEvent struct{}

// ActionEvent is a user action from the UI.
type ActionEvent struct{ ui.Action }

// DisplaysEvent lists the connected displays.
type DisplaysEvent struct{ Displays []Display }

// PermissionEvent reports the capture permission.
type PermissionEvent struct {
	State Permission `json:"state"`
}

// StreamEvent reports a change in one capture stream.
type StreamEvent struct {
	Kind    EventKind `json:"-"`
	Stream  uint64    `json:"stream"`
	Display string    `json:"display"`
	Error   string    `json:"error,omitempty"`
}

// PowerEvent reports that the system is going to sleep or has woken.
type PowerEvent struct {
	Sleeping bool `json:"sleeping"`
}

// CoversShown reports that a cover command took effect, at At nanoseconds on the same clock as
// frame presentation times.
type CoversShown struct {
	Display string `json:"display"`
	Rects   int    `json:"rects"`
	At      int64  `json:"at"`
}

// Transition reports a system animation that moves whole displays, such as switching workspaces
// or entering and leaving full screen, at At nanoseconds on the frame clock.
type Transition struct {
	At int64 `json:"at"`
}

// EventKind implements Event.
func (Transition) EventKind() EventKind { return EvTransition }

// EventKind implements Event.
func (CapabilitiesEvent) EventKind() EventKind { return EvCapabilities }

// EventKind implements Event.
func (ReadyEvent) EventKind() EventKind { return EvReady }

// EventKind implements Event.
func (ActionEvent) EventKind() EventKind { return EvAction }

// EventKind implements Event.
func (DisplaysEvent) EventKind() EventKind { return EvDisplays }

// EventKind implements Event.
func (PermissionEvent) EventKind() EventKind { return EvPermission }

// EventKind implements Event.
func (e StreamEvent) EventKind() EventKind { return e.Kind }

// EventKind implements Event.
func (PowerEvent) EventKind() EventKind { return EvPower }

// EventKind implements Event.
func (CoversShown) EventKind() EventKind { return EvCoversShown }

// DecodeEvent decodes one event payload.
func DecodeEvent(kind EventKind, data []byte) (Event, error) {
	switch kind {
	case EvCapabilities:
		var c Capabilities

		err := json.Unmarshal(data, &c)

		return CapabilitiesEvent{c}, wrap(kind, err)
	case EvReady:
		return ReadyEvent{}, nil
	case EvAction:
		var a ui.Action

		err := json.Unmarshal(data, &a)

		return ActionEvent{a}, wrap(kind, err)
	case EvDisplays:
		var d []Display

		err := json.Unmarshal(data, &d)

		return DisplaysEvent{d}, wrap(kind, err)
	case EvPermission:
		var p PermissionEvent

		err := json.Unmarshal(data, &p)

		return p, wrap(kind, err)
	case EvStreamStarted, EvStreamStopped, EvStreamFailed:
		s := StreamEvent{Kind: kind}
		err := json.Unmarshal(data, &s)

		return s, wrap(kind, err)
	case EvPower:
		var p PowerEvent

		err := json.Unmarshal(data, &p)

		return p, wrap(kind, err)
	case EvTransition:
		var tr Transition

		err := json.Unmarshal(data, &tr)

		return tr, wrap(kind, err)
	case EvCoversShown:
		var c CoversShown

		err := json.Unmarshal(data, &c)

		return c, wrap(kind, err)
	default:
		return nil, fmt.Errorf("bridge: unknown event kind %d", kind)
	}
}

// EncodeEvent encodes an event payload, as a platform layer would. Tests and fake buses use it.
func EncodeEvent(e Event) (EventKind, []byte, error) {
	var v any

	switch e := e.(type) {
	case CapabilitiesEvent:
		v = e.Capabilities
	case ReadyEvent:
		return EvReady, nil, nil
	case ActionEvent:
		v = e.Action
	case DisplaysEvent:
		v = e.Displays
	default:
		v = e
	}

	b, err := json.Marshal(v)

	return e.EventKind(), b, err
}

func wrap(kind EventKind, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("bridge: decode event %d: %w", kind, err)
}

// FrameTarget receives the frames of one capture stream. CaptureFrame runs on the platform's
// capture thread: it must convert src before returning, keep no reference to it, never block and
// allocate nothing.
type FrameTarget interface {
	CaptureFrame(format frame.Format, src []byte, width, height, stride int, pts int64)
}

// Command is something the controller asks the platform layer to do.
type Command interface{ CommandKind() CommandKind }

// Rect is a normalised display area, origin top left.
type Rect = coverage.Rect

// SetCovers replaces the covers on one display. No rects removes them.
type SetCovers struct {
	Display string
	// FadeIn and FadeOut are the opacity animation times, in seconds, for covers appearing and
	// disappearing.
	FadeIn, FadeOut float64
	Rects           []Rect
}

// ClearCovers removes the covers of one display, or of every display when Display is empty.
type ClearCovers struct {
	Display string `json:"display"`
}

// SetStatus updates the menu bar and panel header.
type SetStatus struct{ ui.Status }

// SetControls rebuilds the panel's settings.
type SetControls struct{ Controls []ui.Control }

// SetStats updates the panel's debug view.
type SetStats struct{ ui.Stats }

// StartCapture starts capturing one display at Width×Height pixels, at most FPS frames per second.
type StartCapture struct {
	Stream  uint64       `json:"stream"`
	Display string       `json:"display"`
	Width   int          `json:"width"`
	Height  int          `json:"height"`
	FPS     int          `json:"fps"`
	Format  frame.Format `json:"format"`
	// Handle identifies Target on the platform side; the bus fills it in.
	Handle uint64      `json:"handle"`
	Target FrameTarget `json:"-"`
}

// StopCapture stops one stream. The platform answers with EvStreamStopped.
type StopCapture struct {
	Stream  uint64 `json:"stream"`
	Display string `json:"display"`
}

// CheckPermission asks for the capture permission state.
type CheckPermission struct{}

// RequestPermission asks the user for capture permission.
type RequestPermission struct{}

// OpenPermissionPane opens the system settings where capture permission is granted.
type OpenPermissionPane struct{}

// Notify shows a desktop notification.
type Notify struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// ShowPanel brings up the status panel.
type ShowPanel struct{}

// Quit ends the platform's run loop, which ends the program.
type Quit struct{}

// CommandKind implements Command.
func (SetCovers) CommandKind() CommandKind { return CmdSetCovers }

// CommandKind implements Command.
func (ClearCovers) CommandKind() CommandKind { return CmdClearCovers }

// CommandKind implements Command.
func (SetStatus) CommandKind() CommandKind { return CmdStatus }

// CommandKind implements Command.
func (SetControls) CommandKind() CommandKind { return CmdControls }

// CommandKind implements Command.
func (SetStats) CommandKind() CommandKind { return CmdStats }

// CommandKind implements Command.
func (StartCapture) CommandKind() CommandKind { return CmdStartCapture }

// CommandKind implements Command.
func (StopCapture) CommandKind() CommandKind { return CmdStopCapture }

// CommandKind implements Command.
func (CheckPermission) CommandKind() CommandKind { return CmdCheckPermission }

// CommandKind implements Command.
func (RequestPermission) CommandKind() CommandKind { return CmdRequestPermission }

// CommandKind implements Command.
func (OpenPermissionPane) CommandKind() CommandKind { return CmdOpenPermissionPane }

// CommandKind implements Command.
func (Notify) CommandKind() CommandKind { return CmdNotify }

// CommandKind implements Command.
func (ShowPanel) CommandKind() CommandKind { return CmdShowPanel }

// CommandKind implements Command.
func (Quit) CommandKind() CommandKind { return CmdQuit }

// EncodeCommand encodes a command payload for the platform layer.
func EncodeCommand(c Command) ([]byte, error) {
	switch c := c.(type) {
	case SetCovers:
		return EncodeCovers(c)
	case SetStatus:
		return json.Marshal(c.Status)
	case SetControls:
		if c.Controls == nil {
			c.Controls = []ui.Control{}
		}

		return json.Marshal(c.Controls)
	case SetStats:
		if c.Displays == nil {
			c.Displays = []ui.DisplayStats{}
		}

		return json.Marshal(c.Stats)
	case CheckPermission, RequestPermission, OpenPermissionPane, ShowPanel, Quit:
		return nil, nil
	default:
		return json.Marshal(c)
	}
}

// ErrCovers is returned for a malformed cover payload.
var ErrCovers = errors.New("bridge: malformed covers payload")

// EncodeCovers packs a SetCovers command, little-endian:
//
//	u16      display ID length n
//	n bytes  display ID (UTF-8)
//	f64      fade-in seconds
//	f64      fade-out seconds
//	u32      rectangle count k
//	k × 4 f64 x, y, w, h in normalised display coordinates (0 to 1, origin top left)
func EncodeCovers(c SetCovers) ([]byte, error) {
	if len(c.Display) > math.MaxUint16 {
		return nil, fmt.Errorf("%w: display ID too long", ErrCovers)
	}

	b := make([]byte, 0, 2+len(c.Display)+8+8+4+32*len(c.Rects))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(c.Display)))
	b = append(b, c.Display...)
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(c.FadeIn))
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(c.FadeOut))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(c.Rects)))

	for _, r := range c.Rects {
		for _, v := range [4]float64{r.X, r.Y, r.W, r.H} {
			b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
		}
	}

	return b, nil
}

// DecodeCovers unpacks EncodeCovers' layout.
func DecodeCovers(b []byte) (SetCovers, error) {
	var c SetCovers

	if len(b) < 2 {
		return c, ErrCovers
	}

	n := int(binary.LittleEndian.Uint16(b))
	b = b[2:]

	if len(b) < n+20 {
		return c, ErrCovers
	}

	c.Display = string(b[:n])
	b = b[n:]
	c.FadeIn = math.Float64frombits(binary.LittleEndian.Uint64(b))
	c.FadeOut = math.Float64frombits(binary.LittleEndian.Uint64(b[8:]))
	k := int(binary.LittleEndian.Uint32(b[16:]))
	b = b[20:]

	if len(b) != 32*k {
		return SetCovers{}, ErrCovers
	}

	c.Rects = make([]Rect, k)
	for i := range c.Rects {
		f := func(j int) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(b[32*i+8*j:])) }
		c.Rects[i] = Rect{X: f(0), Y: f(1), W: f(2), H: f(3)}
	}

	return c, nil
}
