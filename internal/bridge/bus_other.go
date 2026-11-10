//go:build !darwin || !cgo

package bridge

// New reports that this platform has no bus yet. A Linux bus (X11 or Wayland) implements the same
// contract in bus_linux.go; see docs/adr/0004-keep-the-core-platform-neutral-and-add-linux-as-a-second-bus.md.
func New(Options) (Bus, error) { return nil, ErrUnsupported }
