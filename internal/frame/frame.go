// Package frame converts captured pixel buffers into psv frames and pools the frames, so the
// capture path converts each buffer in one pass and allocates nothing per frame.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/brian-nunez/psv"
)

// Format is the layout of a captured pixel buffer: four bytes per pixel, in the order named. The
// values are part of the bridge contract; every platform layer uses the same numbers.
type Format int

// Pixel formats a bus can deliver.
const (
	BGRA Format = 1 // macOS ScreenCaptureKit (kCVPixelFormatType_32BGRA)
	RGBA Format = 2
	BGRX Format = 3 // X11 and PipeWire's common BGRx layout; the fourth byte is ignored
	RGBX Format = 4
)

// String names the format.
func (f Format) String() string {
	switch f {
	case BGRA:
		return "BGRA"
	case RGBA:
		return "RGBA"
	case BGRX:
		return "BGRx"
	case RGBX:
		return "RGBx"
	default:
		return fmt.Sprintf("Format(%d)", int(f))
	}
}

// ParseFormat returns the format named by s, as String writes it.
func ParseFormat(s string) (Format, error) {
	for _, f := range []Format{BGRA, RGBA, BGRX, RGBX} {
		if f.String() == s {
			return f, nil
		}
	}

	return 0, fmt.Errorf("unknown pixel format %q", s)
}

// redFirst reports whether the format stores red in the first byte of a pixel.
func (f Format) redFirst() (bool, error) {
	switch f {
	case BGRA, BGRX:
		return false, nil
	case RGBA, RGBX:
		return true, nil
	default:
		return false, fmt.Errorf("unsupported pixel format %d", int(f))
	}
}

// ErrSize is returned when a source buffer does not match the destination frame.
var ErrSize = errors.New("frame: size mismatch")

// Convert writes the width×height image in src, whose rows are stride bytes apart, into dst as
// packed 8-bit RGB. dst must already have that size and a Pix of at least width*height*3 bytes.
// It reads src once and allocates nothing.
func Convert(dst *psv.Frame, format Format, src []byte, width, height, stride int) error {
	redFirst, err := format.redFirst()
	if err != nil {
		return err
	}

	if dst.Width != width || dst.Height != height || len(dst.Pix) < width*height*3 {
		return fmt.Errorf("%w: frame %dx%d, buffer %dx%d", ErrSize, dst.Width, dst.Height, width, height)
	}

	if width < 1 || height < 1 || stride < width*4 || len(src) < (height-1)*stride+width*4 {
		return fmt.Errorf("%w: %d bytes for %dx%d with stride %d", ErrSize, len(src), width, height, stride)
	}

	out := dst.Pix
	for y := range height {
		convertRow(out[y*width*3:(y+1)*width*3], src[y*stride:y*stride+width*4], redFirst)
	}

	return nil
}

// convertRow converts one row, four pixels at a time: each 32-bit pixel is read as a word and the
// four RGB triplets are written with one 64-bit and one 32-bit store.
func convertRow(o, row []byte, redFirst bool) {
	n := min(len(row)/16, len(o)/12)

	if redFirst {
		for i := range n {
			r, w := row[i*16:i*16+16], o[i*12:i*12+12]
			p0 := uint64(binary.LittleEndian.Uint32(r[0:]) & 0xffffff)
			p1 := uint64(binary.LittleEndian.Uint32(r[4:]) & 0xffffff)
			p2 := uint64(binary.LittleEndian.Uint32(r[8:]) & 0xffffff)
			p3 := uint64(binary.LittleEndian.Uint32(r[12:]) & 0xffffff)
			binary.LittleEndian.PutUint64(w[0:], p0|p1<<24|p2<<48)
			binary.LittleEndian.PutUint32(w[8:], uint32(p2>>16|p3<<8))
		}
	} else {
		for i := range n {
			r, w := row[i*16:i*16+16], o[i*12:i*12+12]
			p0 := swap(binary.LittleEndian.Uint32(r[0:]))
			p1 := swap(binary.LittleEndian.Uint32(r[4:]))
			p2 := swap(binary.LittleEndian.Uint32(r[8:]))
			p3 := swap(binary.LittleEndian.Uint32(r[12:]))
			binary.LittleEndian.PutUint64(w[0:], p0|p1<<24|p2<<48)
			binary.LittleEndian.PutUint32(w[8:], uint32(p2>>16|p3<<8))
		}
	}

	for x, j := n*16, n*12; x+4 <= len(row) && j+3 <= len(o); x, j = x+4, j+3 {
		if redFirst {
			o[j], o[j+1], o[j+2] = row[x], row[x+1], row[x+2]
		} else {
			o[j], o[j+1], o[j+2] = row[x+2], row[x+1], row[x]
		}
	}
}

// swap turns a little-endian BGRA word into packed RGB in the low 24 bits.
func swap(p uint32) uint64 { return uint64(p>>16&0xff | p&0xff00 | p&0xff<<16) }

// Slot is a pooled frame with the presentation time of the capture it holds.
type Slot struct {
	Frame psv.Frame
	// PTS is the capture's presentation time in nanoseconds, in the bus's clock.
	PTS int64
}

// Pool is a fixed set of frames of one size. Get and Put never allocate and never block, so the
// capture callback can use them.
type Pool struct {
	width, height int
	free          chan *Slot
}

// NewPool returns a pool of n frames of width×height.
func NewPool(n, width, height int) *Pool {
	p := &Pool{width: width, height: height, free: make(chan *Slot, n)}

	for range n {
		p.free <- &Slot{Frame: psv.Frame{Width: width, Height: height, Pix: make([]byte, width*height*3)}}
	}

	return p
}

// Size returns the frame size of the pool.
func (p *Pool) Size() (width, height int) { return p.width, p.height }

// Get returns a free slot, or nil when every slot is in use.
func (p *Pool) Get() *Slot {
	select {
	case s := <-p.free:
		return s
	default:
		return nil
	}
}

// Put returns a slot to the pool. A nil slot, or one of another size, is ignored.
func (p *Pool) Put(s *Slot) {
	if s == nil || s.Frame.Width != p.width || s.Frame.Height != p.height {
		return
	}

	select {
	case p.free <- s:
	default:
	}
}
