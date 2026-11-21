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
// packed 8-bit RGB. dst must have a Pix of at least dst.Width*dst.Height*3 bytes and be no larger
// than the image in either direction. When it is smaller, each frame pixel is the mean of the
// image pixels it covers (an area-average downscale), for platforms whose capture delivers the
// display at full resolution. It reads src once and allocates nothing.
func Convert(dst *psv.Frame, format Format, src []byte, width, height, stride int) error {
	redFirst, err := format.redFirst()
	if err != nil {
		return err
	}

	if dst.Width < 1 || dst.Height < 1 || dst.Width > width || dst.Height > height || len(dst.Pix) < dst.Width*dst.Height*3 {
		return fmt.Errorf("%w: frame %dx%d, buffer %dx%d", ErrSize, dst.Width, dst.Height, width, height)
	}

	if stride < width*4 || len(src) < (height-1)*stride+width*4 {
		return fmt.Errorf("%w: %d bytes for %dx%d with stride %d", ErrSize, len(src), width, height, stride)
	}

	if dst.Width != width || dst.Height != height {
		downscale(dst, src, width, height, stride, redFirst)

		return nil
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

// downscale area-averages the width×height image in src into dst, which is no larger in either
// direction. Frame pixel (dx, dy) covers image columns [dx*width/dst.Width, (dx+1)*width/dst.Width)
// and the matching rows, so every image pixel counts towards exactly one frame pixel.
//
// It runs once per captured frame on the capture thread. It reads the image strictly in order, a
// row at a time, adding each row into per-column sums on the stack; block edges advance by a
// quotient and a carried remainder, and means multiply by a reciprocal recomputed only when the
// block size changes, so there is no division per pixel.
func downscale(dst *psv.Frame, src []byte, width, height, stride int, redFirst bool) {
	dw, dh := dst.Width, dst.Height

	// Two pixels are read per 64-bit word. Their first and third bytes are summed in four 16-bit
	// lanes, their second bytes in two 32-bit lanes; a lane overflows past 256 pixels of 255, so
	// packed sums are used only for blocks of up to 512 pixels and frames up to maxPackedWidth.
	if dw > maxPackedWidth || (width/dw+1)*(height/dh+1) > 512 {
		downscaleSlow(dst, src, width, height, stride, redFirst)

		return
	}

	var ac, g [maxPackedWidth]uint64

	var lastN, recip uint64

	rows := newEdges(height, dh)
	for dy := range dh {
		y0, y1 := rows.next()

		clear(ac[:dw])
		clear(g[:dw])

		for y := y0; y < y1; y++ {
			row := src[y*stride : y*stride+width*4]

			cols := newEdges(width, dw)
			for dx := range dw {
				x0, x1 := cols.next()
				block := row[x0*4 : x1*4]

				var a, b uint64

				i := 0
				for ; i+8 <= len(block); i += 8 {
					p := binary.LittleEndian.Uint64(block[i : i+8])
					a += p & 0x00ff00ff00ff00ff
					b += p >> 8 & 0x000000ff000000ff
				}

				if i+4 <= len(block) {
					p := uint64(binary.LittleEndian.Uint32(block[i : i+4]))
					a += p & 0x00ff00ff
					b += p >> 8 & 0xff
				}

				ac[dx] += a
				g[dx] += b
			}
		}

		out := dst.Pix[dy*dw*3 : (dy+1)*dw*3]

		cols := newEdges(width, dw)
		for dx := range dw {
			x0, x1 := cols.next()
			if n := uint64((x1 - x0) * (y1 - y0)); n != lastN {
				lastN, recip = n, (1<<recipShift+n-1)/n
			}

			half := lastN / 2
			s, t := ac[dx], g[dx]
			first := byte((s&0xffff + s>>32&0xffff + half) * recip >> recipShift)
			second := byte((t&0xffffffff + t>>32 + half) * recip >> recipShift)
			third := byte((s>>16&0xffff + s>>48 + half) * recip >> recipShift)

			o := out[dx*3 : dx*3+3]
			if redFirst {
				o[0], o[1], o[2] = first, second, third
			} else {
				o[0], o[1], o[2] = third, second, first
			}
		}
	}
}

// edges splits n source pixels into parts blocks: block i spans [i*n/parts, (i+1)*n/parts). It
// steps by a quotient and a carried remainder instead of dividing for every block.
type edges struct {
	q, r, parts, end, carry int
}

func newEdges(n, parts int) edges { return edges{q: n / parts, r: n % parts, parts: parts} }

// next returns the next block's start and end.
func (e *edges) next() (start, end int) {
	start, e.end = e.end, e.end+e.q

	e.carry += e.r
	if e.carry >= e.parts {
		e.carry -= e.parts
		e.end++
	}

	return start, e.end
}

// maxPackedWidth is the widest frame downscale sums on the stack: twice the widest capture width
// the settings allow.
const maxPackedWidth = 2560

// recipShift is the fixed-point precision of the reciprocal in downscale. For a rounded block sum
// m ≤ 256·n, m·ceil(2^k/n)/2^k exceeds m/n by less than m/2^k, which stays below 1/n, so the
// result is exact for blocks of up to 512 pixels; m·recip stays below 2^49.
const recipShift = 40

// downscaleSlow is downscale for frames or blocks too large for packed sums. Block edges are
// computed by division, which is fine at the rates such sizes occur.
func downscaleSlow(dst *psv.Frame, src []byte, width, height, stride int, redFirst bool) {
	dw, dh := dst.Width, dst.Height

	for dy := range dh {
		y0, y1 := dy*height/dh, (dy+1)*height/dh

		for dx := range dw {
			x0, x1 := dx*width/dw, (dx+1)*width/dw

			var first, second, third uint64

			for y := y0; y < y1; y++ {
				row := src[y*stride+x0*4 : y*stride+x1*4]
				for i := 0; i+4 <= len(row); i += 4 {
					first += uint64(row[i])
					second += uint64(row[i+1])
					third += uint64(row[i+2])
				}
			}

			n := uint64((x1 - x0) * (y1 - y0))
			a, g, c := byte((first+n/2)/n), byte((second+n/2)/n), byte((third+n/2)/n)

			o := dst.Pix[(dy*dw+dx)*3 : (dy*dw+dx)*3+3]
			if redFirst {
				o[0], o[1], o[2] = a, g, c
			} else {
				o[0], o[1], o[2] = c, g, a
			}
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
