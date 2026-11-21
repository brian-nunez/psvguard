//go:build !(darwin || linux) || !cgo

package bridge

// New reports that this platform has no bus: macOS and Linux have one when built with cgo.
func New(Options) (Bus, error) { return nil, ErrUnsupported }
