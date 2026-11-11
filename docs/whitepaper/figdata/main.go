//go:build whitepaper

// Command figdata measures the clips used in the white paper and writes the data and images its
// figures are drawn from. It streams each clip through psv's Analyzer.Watch, frame by frame at the
// clip's own rate, and through PSV Guard's monitor pipeline at 60 fps, the way captured frames
// arrive.
//
//	go run -tags whitepaper ./docs/whitepaper/figdata
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brian-nunez/psv"

	"github.com/brian-nunez/psvguard/internal/coverage"
	"github.com/brian-nunez/psvguard/internal/frame"
	"github.com/brian-nunez/psvguard/internal/monitor"
)

const (
	out       = "docs/whitepaper"
	thumbW    = 1280
	maxLength = 8 * time.Second
)

type clip struct {
	name  string
	video string
	// probe is the pixel whose luminance is traced, as a fraction of the frame.
	px, py float64
	// images writes frames and masks; only for clips made here, never third-party video.
	images bool
}

type summary struct {
	Name          string  `json:"name"`
	FPS           float64 `json:"fps"`
	Source        string  `json:"source"`
	Analysed      string  `json:"analysed"`
	FirstFail     float64 `json:"firstFail"`
	FirstFlashing float64 `json:"firstFlashing"`
	FirstCover    float64 `json:"firstCover"`
	FirstActivity float64 `json:"firstActivity"` // first frame in which at least 1% of the frame transitions
	DecidedFails  int     `json:"decidedFails"`
	Misses        uint64  `json:"misses"`
	CoverSeconds  float64 `json:"coverSeconds"`
	Seconds       float64 `json:"seconds"`
}

func main() {
	log.SetFlags(0)

	examples, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/brian-nunez/psv").Output()
	if err != nil {
		log.Fatal(err)
	}

	ex := filepath.Join(strings.TrimSpace(string(examples)), "testdata", "examples")

	tmp, err := os.MkdirTemp("", "psvguard-figdata")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	for _, d := range []string{"data", "figures/generated"} {
		if err := os.MkdirAll(filepath.Join(out, d), 0o755); err != nil {
			log.Fatal(err)
		}
	}

	flash := filepath.Join(tmp, "flash.mp4")
	synth(flash, "color=c=gray:s=1280x720:r=60:d=4",
		"format=yuv420p,geq=lum='if(lt(T,1),128,if(lt(mod(T-1,0.2),0.1),235,16))':cb=128:cr=128")

	scroll := filepath.Join(tmp, "scroll.mp4")
	writeText(filepath.Join(tmp, "text.txt"))
	synth(scroll, "color=c=white:s=1280x720:r=60:d=4",
		"drawtext=fontfile="+monospaceFont()+":textfile="+filepath.Join(tmp, "text.txt")+":fontsize=22:fontcolor=black:x=40:y=40-t*1500")

	local := filepath.Join(tmp, "local.mp4")
	synth(local, "color=c=0x22262d:s=1280x720:r=60:d=4",
		"drawtext=fontfile="+monospaceFont()+":textfile="+filepath.Join(tmp, "text.txt")+":fontsize=18:fontcolor=0xc8ccd2:x=40:y=40,"+
			"drawbox=x=720:y=170:w=460:h=340:color=white:t=fill:enable='gt(t,1)*lt(mod(t-1,0.2),0.1)'")

	clips := []clip{
		{"local", local, 0.88, 0.5, true},
		{"colors", filepath.Join(ex, "colors-epilepsy-warning", "video.mp4"), 0.5, 0.5, false},
		{"flash", flash, 0.5, 0.5, true},
		{"scroll", scroll, 0.3, 0.3, true},
		{"ad", filepath.Join(ex, "video-ad-sample-2", "video.mp4"), 0.5, 0.5, false},
	}

	var all []summary

	for _, c := range clips {
		s := summary{Name: c.name}
		watchClip(c, &s)
		pipelineClip(c, &s)
		all = append(all, s)
		log.Printf("%s: %+v", c.name, s)
	}

	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "data", "summary.json"), b, 0o644); err != nil {
		log.Fatal(err)
	}

	writeMacros(all)
}

func synth(path, src, vf string) {
	cmd := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", src, "-vf", vf,
		"-c:v", "libx264", "-pix_fmt", "yuv420p", path)
	if b, err := cmd.CombinedOutput(); err != nil {
		log.Fatalf("ffmpeg: %v: %s", err, b)
	}
}

func writeText(path string) {
	words := strings.Fields("the quick brown fox jumps over lazy dog lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor")

	var text strings.Builder

	for line := range 400 {
		for w := range 14 {
			text.WriteString(words[(line*7+w*3)%len(words)] + " ")
		}

		text.WriteString("\n")
	}

	if err := os.WriteFile(path, []byte(text.String()), 0o644); err != nil {
		log.Fatal(err)
	}
}

func monospaceFont() string {
	for _, f := range []string{"/System/Library/Fonts/Menlo.ttc", "/System/Library/Fonts/Monaco.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"} {
		if _, err := os.Stat(f); err == nil {
			return f
		}
	}

	log.Fatal("no monospace font")

	return ""
}

// decoder streams a video as BGRA frames at the analysis size.
type decoder struct {
	cmd  *exec.Cmd
	in   *bufio.Reader
	buf  []byte
	w, h int
	info psv.VideoInfo
}

func open(video string) *decoder {
	info, err := psv.Probe(context.Background(), video, psv.Tools{})
	if err != nil {
		log.Fatal(err)
	}

	w, h := monitor.CaptureSize(float64(info.Width), float64(info.Height), 640)
	cmd := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", video, "-map", "0:v:0", "-an", "-t", fmt.Sprint(maxLength.Seconds()),
		"-vf", fmt.Sprintf("scale=%d:%d:in_color_matrix=auto:in_range=auto:out_range=pc:flags=area+accurate_rnd+full_chroma_int,format=bgra", w, h),
		"-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", "bgra", "pipe:1")

	o, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	return &decoder{cmd: cmd, in: bufio.NewReaderSize(o, 1<<20), buf: make([]byte, w*h*4), w: w, h: h, info: info}
}

func (d *decoder) next() bool {
	if _, err := io.ReadFull(d.in, d.buf); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			log.Fatal(err)
		}

		_ = d.cmd.Wait()

		return false
	}

	return true
}

func thresholds(d *decoder) psv.Thresholds {
	return monitor.ScaleThresholds(psv.WCAG22(), float64(d.w)/float64(d.info.Width))
}

// relLum is the relative luminance of an sRGB pixel, as psv computes it.
func relLum(r, g, b byte) float64 {
	lin := func(c byte) float64 {
		v := float64(c) / 255
		if v <= 0.04045 {
			return v / 12.92
		}

		return math.Pow((v+0.055)/1.055, 2.4)
	}

	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// pixel replays psv's per-pixel transition rules for one pixel.
type pixel struct {
	prev             float64
	stepDir, lastDir int
	hist             []int
}

func (p *pixel) step(t int, lum float64, rate int) (dir int, counted bool, recent int) {
	if t > 0 && math.Abs(lum-p.prev) >= 0.1 && min(lum, p.prev) < 0.8 {
		dir = 1
		if lum < p.prev {
			dir = -1
		}
	}

	p.prev = lum
	starts := dir != 0 && dir != p.stepDir
	p.stepDir = dir

	if starts && dir != p.lastDir {
		p.lastDir = dir
		counted = true
		p.hist = append(p.hist, t)
	}

	for _, at := range p.hist {
		if at > t-rate {
			recent++
		}
	}

	return dir, counted, recent
}

type watched struct {
	b           psv.Building
	img         *image.RGBA
	fail        bool
	mask        []byte
	transitions int
	regionArea  float64
}

func watchClip(c clip, s *summary) {
	d := open(c.video)
	fps := d.info.FrameRate.Float64()
	rate := int(math.Round(fps))
	s.FPS, s.Source, s.Analysed = fps, fmt.Sprintf("%dx%d", d.info.Width, d.info.Height), fmt.Sprintf("%dx%d", d.w, d.h)

	a, err := psv.NewAnalyzer(fps, d.w, d.h, psv.Config{Thresholds: thresholds(d)})
	if err != nil {
		log.Fatal(err)
	}

	f, err := os.Create(filepath.Join(out, "data", c.name+"-frames.csv"))
	if err != nil {
		log.Fatal(err)
	}

	defer f.Close()

	w := bufio.NewWriter(f)
	defer w.Flush()

	fmt.Fprintln(w, "frame,t,lum,dir,counted,recent,peak,area,flashing,redpeak,redarea,failed,transitions,regionarea")

	var (
		frames []watched
		pf     psv.Frame
		probe  pixel
	)

	pf = psv.Frame{Width: d.w, Height: d.h, Pix: make([]byte, d.w*d.h*3)}
	px, py := int(c.px*float64(d.w)), int(c.py*float64(d.h))
	s.FirstFail, s.FirstFlashing, s.FirstActivity = -1, -1, -1

	for i := 0; d.next(); i++ {
		if err := frame.Convert(&pf, frame.BGRA, d.buf, d.w, d.h, d.w*4); err != nil {
			log.Fatal(err)
		}

		u, err := a.Watch(&pf)
		if err != nil {
			log.Fatal(err)
		}

		o := (py*d.w + px) * 3
		lum := relLum(pf.Pix[o], pf.Pix[o+1], pf.Pix[o+2])
		dir, counted, recent := probe.step(i, lum, rate)

		frames = append(frames, watched{b: u.Building, img: thumb(&pf)})

		if s.FirstActivity < 0 && u.Building.Luminance.Area+u.Building.Red.Area >= 1 && i > 0 {
			s.FirstActivity = float64(i) / fps
		}

		if s.FirstFlashing < 0 && (u.Building.Luminance.Flashing > 0 || u.Building.Red.Flashing > 0) {
			s.FirstFlashing = float64(i) / fps
		}

		for k, r := range u.Decided {
			decide(frames, u.DecidedFrom+k-1, r, s, fps)
		}

		probeRows = append(probeRows, probeRow{i, float64(i) / fps, lum, dir, counted, recent})
	}

	u, err := a.Stop()
	if err != nil {
		log.Fatal(err)
	}

	for k, r := range u.Decided {
		decide(frames, u.DecidedFrom+k-1, r, s, fps)
	}

	for _, p := range probeRows {
		fr := frames[p.i]
		b := fr.b
		fmt.Fprintf(w, "%d,%.4f,%.4f,%d,%d,%d,%d,%.3f,%.3f,%d,%.3f,%d,%d,%.2f\n",
			p.i, p.t, p.lum, p.dir, b2i(p.counted), p.recent,
			b.Luminance.Peak, b.Luminance.Area, b.Luminance.Flashing, b.Red.Peak, b.Red.Area,
			b2i(fr.fail), fr.transitions, fr.regionArea)
	}

	probeRows = probeRows[:0]

	snapshots(c, frames, s, fps, px, py, d)
}

type probeRow struct {
	i       int
	t, lum  float64
	dir     int
	counted bool
	recent  int
}

var probeRows []probeRow

// snapAt records the time of each Building snapshot, for the captions.
var snapAt = map[string]float64{}

func b2i(b bool) int {
	if b {
		return 1
	}

	return 0
}

func decide(frames []watched, i int, r psv.FrameResult, s *summary, fps float64) {
	if i < 0 || i >= len(frames) {
		return
	}

	fl := r.Luminance
	if r.Red.Transitions > fl.Transitions {
		fl = r.Red
	}

	frames[i].transitions = fl.Transitions
	frames[i].regionArea = fl.Area

	if !r.Failed() {
		return
	}

	frames[i].fail = true
	frames[i].mask = fl.Mask
	s.DecidedFails++

	if s.FirstFail < 0 {
		s.FirstFail = float64(i) / fps
	}
}

// thumb scales an analysed frame to the figure width.
func thumb(f *psv.Frame) *image.RGBA {
	h := thumbW * f.Height / f.Width
	img := image.NewRGBA(image.Rect(0, 0, thumbW, h))

	for y := range h {
		sy := y * f.Height / h
		for x := range thumbW {
			sx := x * f.Width / thumbW
			o := (sy*f.Width + sx) * 3
			img.SetRGBA(x, y, color.RGBA{f.Pix[o], f.Pix[o+1], f.Pix[o+2], 255})
		}
	}

	return img
}

// snapshots writes the frames and masks the figures show.
func snapshots(c clip, frames []watched, s *summary, fps float64, px, py int, d *decoder) {
	if len(frames) == 0 || !c.images {
		return
	}

	at := func(sec float64) int {
		return min(max(int(math.Round(sec*fps)), 0), len(frames)-1)
	}

	save(filepath.Join(out, "figures", "generated", c.name+"-frame.png"), marker(frames[at(max(s.FirstActivity, 0)+0.04)].img, px, py, d))

	// The first frames in which a pixel that starts a transition reaches 2, 4 (hot) and 7
	// (flashing) transitions within the last second.
	for _, l := range []struct {
		name string
		peak int
	}{{"a", 2}, {"b", 4}, {"c", 7}} {
		for i, f := range frames {
			b := f.b
			if max(b.Luminance.Peak, b.Red.Peak) >= l.peak && b.Luminance.Area+b.Red.Area >= 1 {
				save(filepath.Join(out, "figures", "generated", fmt.Sprintf("%s-building-%s.png", c.name, l.name)),
					heat(f.img, b.Luminance.Mask, b.Red.Mask))
				snapAt[c.name+"-"+l.name] = float64(i) / fps

				break
			}
		}
	}

	if s.FirstFail >= 0 {
		i := at(s.FirstFail)
		save(filepath.Join(out, "figures", "generated", c.name+"-decided.png"), share(frames[i].img, frames[i].mask))
	}
}

func save(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}

	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}

	_ = f.Close()
}

// marker draws a ring around the traced pixel.
func marker(src *image.RGBA, px, py int, d *decoder) *image.RGBA {
	img := image.NewRGBA(src.Bounds())
	copy(img.Pix, src.Pix)

	cx, cy := (px*2+1)*img.Bounds().Dx()/(2*d.w), (py*2+1)*img.Bounds().Dy()/(2*d.h)

	for a := 0.0; a < 2*math.Pi; a += 0.002 {
		for r := 22.0; r < 28; r += 0.5 {
			img.SetRGBA(cx+int(r*math.Cos(a)), cy+int(r*math.Sin(a)), color.RGBA{255, 255, 255, 255})
		}

		for r := 28.0; r < 31; r += 0.5 {
			img.SetRGBA(cx+int(r*math.Cos(a)), cy+int(r*math.Sin(a)), color.RGBA{0, 0, 0, 255})
		}
	}

	return img
}

// peakColour maps a cell's transition count to the figure's scale: below hot, hot, flashing.
func peakColour(n int) (color.RGBA, bool) {
	switch {
	case n <= 0:
		return color.RGBA{}, false
	case n < 4:
		return color.RGBA{56, 132, 255, 255}, true
	case n <= 6:
		return color.RGBA{255, 176, 32, 255}, true
	default:
		return color.RGBA{230, 40, 60, 255}, true
	}
}

// heat darkens the frame and paints each mask cell by the higher of its luminance and red peak.
func heat(src *image.RGBA, luma, red []byte) *image.RGBA {
	img := dim(src)
	b := img.Bounds()

	for cy := range psv.MaskRows {
		for cx := range psv.MaskColumns {
			i := cy*psv.MaskColumns + cx
			n := 0

			if luma != nil {
				n = int(luma[i])
			}

			if red != nil {
				n = max(n, int(red[i]))
			}

			c, ok := peakColour(n)
			if !ok {
				continue
			}

			fill(img, cell(b, cx, cy), c, 0.78)
		}
	}

	grid(img)

	return img
}

// share paints each cell by the share of its pixels that flash in a failing frame.
func share(src *image.RGBA, mask []byte) *image.RGBA {
	img := dim(src)
	b := img.Bounds()

	for cy := range psv.MaskRows {
		for cx := range psv.MaskColumns {
			v := mask[cy*psv.MaskColumns+cx]
			if v == 0 {
				continue
			}

			fill(img, cell(b, cx, cy), color.RGBA{230, 40, 60, 255}, 0.25+0.65*float64(v)/100)
		}
	}

	grid(img)

	return img
}

func cell(b image.Rectangle, cx, cy int) image.Rectangle {
	return image.Rect(cx*b.Dx()/psv.MaskColumns, cy*b.Dy()/psv.MaskRows, (cx+1)*b.Dx()/psv.MaskColumns, (cy+1)*b.Dy()/psv.MaskRows)
}

func dim(src *image.RGBA) *image.RGBA {
	img := image.NewRGBA(src.Bounds())
	for i := 0; i < len(src.Pix); i += 4 {
		img.Pix[i] = uint8(float64(src.Pix[i]) * 0.35)
		img.Pix[i+1] = uint8(float64(src.Pix[i+1]) * 0.35)
		img.Pix[i+2] = uint8(float64(src.Pix[i+2]) * 0.35)
		img.Pix[i+3] = 255
	}

	return img
}

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA, alpha float64) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			o := img.PixOffset(x, y)
			img.Pix[o] = uint8(float64(img.Pix[o])*(1-alpha) + float64(c.R)*alpha)
			img.Pix[o+1] = uint8(float64(img.Pix[o+1])*(1-alpha) + float64(c.G)*alpha)
			img.Pix[o+2] = uint8(float64(img.Pix[o+2])*(1-alpha) + float64(c.B)*alpha)
		}
	}
}

func grid(img *image.RGBA) {
	b := img.Bounds()
	line := color.RGBA{255, 255, 255, 255}

	for cx := 1; cx < psv.MaskColumns; cx++ {
		x := cx * b.Dx() / psv.MaskColumns
		for y := b.Min.Y; y < b.Max.Y; y++ {
			blend(img, x, y, line, 0.12)
		}
	}

	for cy := 1; cy < psv.MaskRows; cy++ {
		y := cy * b.Dy() / psv.MaskRows
		for x := b.Min.X; x < b.Max.X; x++ {
			blend(img, x, y, line, 0.12)
		}
	}
}

func blend(img *image.RGBA, x, y int, c color.RGBA, alpha float64) {
	o := img.PixOffset(x, y)
	img.Pix[o] = uint8(float64(img.Pix[o])*(1-alpha) + float64(c.R)*alpha)
	img.Pix[o+1] = uint8(float64(img.Pix[o+1])*(1-alpha) + float64(c.G)*alpha)
	img.Pix[o+2] = uint8(float64(img.Pix[o+2])*(1-alpha) + float64(c.B)*alpha)
}

// pipelineClip streams the clip through PSV Guard's pipeline at 60 fps and records every tick.
func pipelineClip(c clip, s *summary) {
	d := open(c.video)
	cfg := monitor.Config{
		Display: c.name, Width: d.w, Height: d.h, FPS: 60,
		Thresholds: thresholds(d),
		Params:     coverage.DefaultParams(),
	}

	pool := frame.NewPool(5, d.w, d.h)

	p, err := monitor.NewPipeline(cfg, pool.Put)
	if err != nil {
		log.Fatal(err)
	}

	f, err := os.Create(filepath.Join(out, "data", c.name+"-ticks.csv"))
	if err != nil {
		log.Fatal(err)
	}

	defer f.Close()

	w := bufio.NewWriter(f)
	defer w.Flush()

	fmt.Fprintln(w, "t,hot,fill,peak,covered,covering,full,decided,misses")

	frameDur := time.Duration(float64(time.Second) / d.info.FrameRate.Float64())
	tick := time.Second / 60
	s.FirstCover = -1
	pf := psv.Frame{Width: d.w, Height: d.h, Pix: make([]byte, d.w*d.h*3)}

	var (
		nextFrame time.Duration
		ended     bool
		now       time.Duration
		lastImg   *image.RGBA
		saved     bool
	)

	for ; !ended || now < nextFrame+2*time.Second; now += tick {
		for !ended && nextFrame <= now {
			if !d.next() {
				ended = true

				break
			}

			sl := pool.Get()
			if err := frame.Convert(&sl.Frame, frame.BGRA, d.buf, d.w, d.h, d.w*4); err != nil {
				log.Fatal(err)
			}

			if !saved {
				_ = frame.Convert(&pf, frame.BGRA, d.buf, d.w, d.h, d.w*4)
				lastImg = thumb(&pf)
			}

			sl.PTS = int64(nextFrame)
			p.Offer(sl)
			nextFrame += frameDur
		}

		r, ok, err := p.Step(now)
		if err != nil {
			log.Fatal(err)
		}

		if !ok {
			continue
		}

		dec := r.Decision
		fmt.Fprintf(w, "%.4f,%.3f,%.4f,%d,%.3f,%d,%d,%d,%d\n", now.Seconds(), dec.HotArea, dec.Fill, dec.Peak,
			dec.CoveredArea, b2i(dec.Covering()), b2i(dec.Full), r.DecidedFailures, r.Misses)

		if dec.Covering() {
			s.CoverSeconds += tick.Seconds()

			if s.FirstCover < 0 {
				s.FirstCover = now.Seconds()
			}

			if !saved && lastImg != nil && c.images {
				saved = true
				save(filepath.Join(out, "figures", "generated", c.name+"-cover.png"), covers(lastImg, dec.Rects))
			}
		}
	}

	s.Misses = p.Counters().Misses
	s.Seconds = nextFrame.Seconds()
	p.Close()
}

// covers draws the cover rectangles over a frame the way PSV Guard dims them.
func covers(src *image.RGBA, rects []coverage.Rect) *image.RGBA {
	img := image.NewRGBA(src.Bounds())
	copy(img.Pix, src.Pix)

	b := img.Bounds()
	for _, r := range rects {
		rr := image.Rect(int(r.X*float64(b.Dx())), int(r.Y*float64(b.Dy())),
			int((r.X+r.W)*float64(b.Dx())), int((r.Y+r.H)*float64(b.Dy())))
		fill(img, rr, color.RGBA{24, 24, 30, 255}, 0.97)

		for t := range 4 {
			for x := rr.Min.X; x < rr.Max.X; x++ {
				blend(img, x, min(rr.Min.Y+t, b.Max.Y-1), color.RGBA{255, 255, 255, 255}, 0.8)
				blend(img, x, max(rr.Max.Y-1-t, 0), color.RGBA{255, 255, 255, 255}, 0.8)
			}

			for y := rr.Min.Y; y < rr.Max.Y; y++ {
				blend(img, min(rr.Min.X+t, b.Max.X-1), y, color.RGBA{255, 255, 255, 255}, 0.8)
				blend(img, max(rr.Max.X-1-t, 0), y, color.RGBA{255, 255, 255, 255}, 0.8)
			}
		}
	}

	return img
}

// writeMacros writes the measured numbers as LaTeX macros so the text quotes them exactly.
func writeMacros(all []summary) {
	var b strings.Builder

	b.WriteString("% Generated by docs/whitepaper/figdata. Do not edit.\n")

	for _, s := range all {
		n := strings.ToUpper(s.Name[:1]) + s.Name[1:]
		fmt.Fprintf(&b, "\\newcommand{\\%sFPS}{%.2f}\n", n, s.FPS)
		fmt.Fprintf(&b, "\\newcommand{\\%sSource}{%s}\n", n, strings.ReplaceAll(s.Source, "x", "$\\times$"))
		fmt.Fprintf(&b, "\\newcommand{\\%sAnalysed}{%s}\n", n, strings.ReplaceAll(s.Analysed, "x", "$\\times$"))
		fmt.Fprintf(&b, "\\newcommand{\\%sFirstActivity}{%.2f}\n", n, s.FirstActivity)
		fmt.Fprintf(&b, "\\newcommand{\\%sFirstFlashing}{%.2f}\n", n, s.FirstFlashing)
		fmt.Fprintf(&b, "\\newcommand{\\%sFirstFail}{%.2f}\n", n, s.FirstFail)
		fmt.Fprintf(&b, "\\newcommand{\\%sFirstCover}{%.2f}\n", n, s.FirstCover)
		fmt.Fprintf(&b, "\\newcommand{\\%sDecidedFails}{%d}\n", n, s.DecidedFails)
		fmt.Fprintf(&b, "\\newcommand{\\%sMisses}{%d}\n", n, s.Misses)
		fmt.Fprintf(&b, "\\newcommand{\\%sCoverSeconds}{%.2f}\n", n, s.CoverSeconds)
		fmt.Fprintf(&b, "\\newcommand{\\%sSeconds}{%.2f}\n", n, s.Seconds)
	}

	keys := make([]string, 0, len(snapAt))
	for k := range snapAt {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		v := snapAt[k]
		n := strings.ReplaceAll(k, "-", "")
		fmt.Fprintf(&b, "\\newcommand{\\Snap%s}{%.3f}\n", strings.ToUpper(n[:1])+n[1:], v)
	}

	if err := os.WriteFile(filepath.Join(out, "data", "measured.tex"), []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
}
