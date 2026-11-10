package frame

import (
	"errors"
	"testing"

	"github.com/brian-nunez/psv"
)

func TestConvertHonoursRowPadding(t *testing.T) {
	const w, h, stride = 3, 2, 16 // 12 bytes of pixels and 4 of padding per row

	src := make([]byte, stride*h)
	for i := range src {
		src[i] = 0xEE // padding must never reach the frame
	}

	for y := range h {
		for x := range w {
			p := src[y*stride+x*4:]
			p[0], p[1], p[2], p[3] = byte(10*y+x), byte(100+10*y+x), byte(200+10*y+x), 255 // B, G, R, A
		}
	}

	dst := &psv.Frame{Width: w, Height: h, Pix: make([]byte, w*h*3)}
	if err := Convert(dst, BGRA, src, w, h, stride); err != nil {
		t.Fatal(err)
	}

	for y := range h {
		for x := range w {
			got := dst.Pix[(y*w+x)*3:][:3]
			want := []byte{byte(200 + 10*y + x), byte(100 + 10*y + x), byte(10*y + x)}

			if string(got) != string(want) {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
}

func TestConvertRGBOrder(t *testing.T) {
	src := []byte{1, 2, 3, 4}
	dst := &psv.Frame{Width: 1, Height: 1, Pix: make([]byte, 3)}

	for _, tc := range []struct {
		f    Format
		want []byte
	}{
		{RGBA, []byte{1, 2, 3}},
		{RGBX, []byte{1, 2, 3}},
		{BGRA, []byte{3, 2, 1}},
		{BGRX, []byte{3, 2, 1}},
	} {
		if err := Convert(dst, tc.f, src, 1, 1, 4); err != nil {
			t.Fatal(err)
		}

		if string(dst.Pix) != string(tc.want) {
			t.Errorf("%v: got %v, want %v", tc.f, dst.Pix, tc.want)
		}
	}
}

func TestConvertRejectsMismatches(t *testing.T) {
	dst := &psv.Frame{Width: 2, Height: 2, Pix: make([]byte, 12)}

	if err := Convert(dst, BGRA, make([]byte, 15), 2, 2, 8); !errors.Is(err, ErrSize) {
		t.Errorf("short buffer: got %v, want ErrSize", err)
	}

	if err := Convert(dst, BGRA, make([]byte, 64), 4, 4, 16); !errors.Is(err, ErrSize) {
		t.Errorf("wrong size: got %v, want ErrSize", err)
	}

	if err := Convert(dst, Format(99), make([]byte, 16), 2, 2, 8); err == nil {
		t.Error("unknown format: want an error")
	}
}

func TestConvertAllocatesNothing(t *testing.T) {
	const w, h = 640, 360

	src := make([]byte, w*4*h)
	dst := &psv.Frame{Width: w, Height: h, Pix: make([]byte, w*h*3)}

	allocs := testing.AllocsPerRun(10, func() {
		_ = Convert(dst, BGRA, src, w, h, w*4)
	})
	if allocs != 0 {
		t.Errorf("Convert allocated %.0f times per run", allocs)
	}
}

func TestPool(t *testing.T) {
	p := NewPool(2, 4, 3)

	a, b := p.Get(), p.Get()
	if a == nil || b == nil {
		t.Fatal("pool of 2 returned nil")
	}

	if p.Get() != nil {
		t.Fatal("empty pool returned a slot")
	}

	if len(a.Frame.Pix) != 4*3*3 {
		t.Fatalf("slot buffer is %d bytes", len(a.Frame.Pix))
	}

	p.Put(a)
	p.Put(&Slot{Frame: psv.Frame{Width: 9, Height: 9}}) // foreign size: ignored

	if p.Get() != a {
		t.Fatal("Put slot was not returned")
	}

	if p.Get() != nil {
		t.Fatal("foreign slot entered the pool")
	}
}

func TestParseFormat(t *testing.T) {
	for _, f := range []Format{BGRA, RGBA, BGRX, RGBX} {
		got, err := ParseFormat(f.String())
		if err != nil || got != f {
			t.Errorf("ParseFormat(%q) = %v, %v", f.String(), got, err)
		}
	}

	if _, err := ParseFormat("YUV"); err == nil {
		t.Error("ParseFormat(YUV): want an error")
	}
}

func BenchmarkConvert640(b *testing.B) {
	const w, h = 640, 416

	src := make([]byte, (w*4+64)*h)
	dst := &psv.Frame{Width: w, Height: h, Pix: make([]byte, w*h*3)}

	for b.Loop() {
		_ = Convert(dst, BGRA, src, w, h, w*4+64)
	}
}
