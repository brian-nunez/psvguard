package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brian-nunez/psv"

	"github.com/brian-nunez/psvguard/internal/coverage"
	"github.com/brian-nunez/psvguard/internal/frame"
)

// examplesDir finds psv's example videos in the module cache.
func examplesDir(t *testing.T) string {
	t.Helper()

	if dir := os.Getenv("PSV_EXAMPLES"); dir != "" {
		return dir
	}

	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/brian-nunez/psv").Output()
	if err != nil {
		t.Skipf("psv module not found: %v", err)
	}

	dir := filepath.Join(strings.TrimSpace(string(out)), "testdata", "examples")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("psv examples not found: %v", err)
	}

	return dir
}

// clipResult is what streaming one video through the pipeline found.
type clipResult struct {
	native     *psv.Report
	firstFail  time.Duration // first failing frame in psv's native-resolution report, or -1
	covered    time.Duration // first cover, or -1
	decided    time.Duration // first failing frame decided by the pipeline, or -1
	coverTime  time.Duration // total time covered
	maxHot     float64
	misses     uint64
	duration   time.Duration
	analysedAt string
}

// streamClip decodes a video with FFmpeg at the capture width and streams it through a pipeline
// at 60 fps, the way captured frames arrive: each video frame is offered at its presentation time
// and repeated by the resampler until the next one.
func streamClip(t *testing.T, dir string, params coverage.Params) clipResult {
	t.Helper()

	ctx := context.Background()
	video := filepath.Join(dir, "video.mp4")

	info, err := psv.Probe(ctx, video, psv.Tools{})
	if err != nil {
		t.Skipf("ffprobe: %v", err)
	}

	f, err := os.Open(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}

	native, err := psv.DecodeReport(f)
	_ = f.Close()

	if err != nil {
		t.Fatal(err)
	}

	res := clipResult{native: native, firstFail: -1, covered: -1, decided: -1}

	for _, r := range native.Results {
		if r.Failed() {
			res.firstFail = time.Duration(r.Seconds * float64(time.Second))

			break
		}
	}

	w, h := CaptureSize(float64(info.Width), float64(info.Height), 640)
	res.analysedAt = fmt.Sprintf("%dx%d", w, h)

	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", video, "-map", "0:v:0", "-an",
		"-vf", fmt.Sprintf("scale=%d:%d:in_color_matrix=auto:in_range=auto:out_range=pc:flags=area+accurate_rnd+full_chroma_int,format=bgra", w, h),
		"-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", "bgra", "pipe:1")

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Skipf("ffmpeg: %v", err)
	}

	defer func() { _ = cmd.Wait() }()

	cfg := Config{
		Display: "video", Width: w, Height: h, FPS: 60,
		// Treat a video pixel as a reference pixel, like a full-screen video on a display of the
		// video's size.
		Thresholds: ScaleThresholds(psv.WCAG22(), float64(w)/float64(info.Width)),
		Params:     params,
	}

	pool := frame.NewPool(poolSize, w, h)

	p, err := NewPipeline(cfg, pool.Put)
	if err != nil {
		t.Fatal(err)
	}

	in := bufio.NewReaderSize(out, 1<<20)
	buf := make([]byte, w*h*4)
	frameDur := time.Duration(float64(time.Second) / info.FrameRate.Float64())
	tick := time.Second / 60

	var (
		nextFrame time.Duration
		ended     bool
		now       time.Duration
	)

	for ; !ended || now < nextFrame+2*time.Second; now += tick {
		for !ended && nextFrame <= now {
			if _, err := io.ReadFull(in, buf); err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}

				ended = true

				break
			}

			s := pool.Get()
			if s == nil {
				t.Fatal("pool exhausted")
			}

			if err := frame.Convert(&s.Frame, frame.BGRA, buf, w, h, w*4); err != nil {
				t.Fatal(err)
			}

			s.PTS = int64(nextFrame)
			p.Offer(s)

			nextFrame += frameDur
		}

		r, ok, err := p.Step(now)
		if err != nil {
			t.Fatal(err)
		}

		if !ok {
			continue
		}

		res.maxHot = max(res.maxHot, r.Decision.HotArea)

		if r.Decision.Covering() {
			res.coverTime += tick

			if res.covered < 0 {
				res.covered = now
			}
		}

		if res.decided < 0 && r.DecidedFailures > 0 {
			res.decided = time.Duration(r.Frame-p.cfg.FPS+1) * tick
		}
	}

	res.duration = nextFrame
	res.misses = p.Counters().Misses
	p.Close()

	return res
}

func TestExampleVideos(t *testing.T) {
	if testing.Short() {
		t.Skip("decodes several videos")
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	dir := examplesDir(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		t.Run(e.Name(), func(t *testing.T) {
			t.Parallel()

			r := streamClip(t, filepath.Join(dir, e.Name()), coverage.DefaultParams())

			t.Logf("%s at %s: native verdict %s, first failing frame %v; pipeline first decided failure %v; "+
				"first cover %v; covered %v of %v; max hot area %.2f%%; misses %d",
				e.Name(), r.analysedAt, r.native.Verdict, r.firstFail, r.decided,
				r.covered, r.coverTime.Round(time.Millisecond), r.duration.Round(time.Millisecond), r.maxHot, r.misses)

			if r.native.Passed {
				if r.covered >= 0 {
					t.Errorf("passing video covered at %v (max hot area %.2f%%)", r.covered, r.maxHot)
				}

				if r.decided >= 0 {
					t.Errorf("passing video failed at %v at the capture size", r.decided)
				}

				return
			}

			if r.decided < 0 {
				t.Errorf("failing video passed at the capture size")
			}

			if r.covered < 0 {
				t.Fatal("failing video never covered")
			}

			// Decided at 640 px and 60 fps should agree with psv's native report to within a frame
			// of the source.
			if r.decided >= 0 && absDur(r.decided-r.firstFail) > 100*time.Millisecond {
				t.Errorf("first failure at %v at the capture size, %v natively", r.decided, r.firstFail)
			}

			if r.covered >= r.firstFail {
				t.Errorf("covered at %v, not before the first failing frame at %v", r.covered, r.firstFail)
			}

			if r.misses > 0 {
				t.Errorf("%d decided failures were not covered when shown", r.misses)
			}
		})
	}
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}

	return d
}

// TestTuneThresholds sweeps the trigger thresholds over the example videos and logs, for each
// combination, how far ahead of psv's first failing frame each failing video is covered and how
// much of each passing video is covered. Run it with PSVGUARD_TUNE=1 when changing defaults.
func TestTuneThresholds(t *testing.T) {
	if os.Getenv("PSVGUARD_TUNE") == "" {
		t.Skip("set PSVGUARD_TUNE=1 to sweep thresholds")
	}

	dir := examplesDir(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, peak := range []int{4, 5, 6} {
		for _, area := range []float64{2, 5, 10, 20, 30} {
			p := coverage.DefaultParams()
			p.HotPeak, p.MinHotArea = peak, area

			line := fmt.Sprintf("peak %d area %4.1f%%:", peak, area)

			for _, e := range entries {
				if !e.IsDir() {
					continue
				}

				r := streamClip(t, filepath.Join(dir, e.Name()), p)
				if r.native.Passed {
					line += fmt.Sprintf("  %s covered %v/%v", e.Name(), r.coverTime.Round(time.Millisecond), r.duration.Round(time.Millisecond))
				} else {
					line += fmt.Sprintf("  %s lead %v", e.Name(), (r.firstFail - r.covered).Round(time.Millisecond))
				}
			}

			t.Log(line)
		}
	}
}

// clipDir prepares a directory holding path as video.mp4 and psv's native report as report.json,
// the layout streamClip reads.
func clipDir(t *testing.T, path string) (string, *psv.Report) {
	t.Helper()

	dir := t.TempDir()
	if err := os.Symlink(path, filepath.Join(dir, "video.mp4")); err != nil {
		t.Fatal(err)
	}

	a, err := psv.AnalyzeFile(context.Background(), path, psv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	rep := a.Report(true)

	f, err := os.Create(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := json.NewEncoder(f).Encode(rep); err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	return dir, &rep
}

// TestClip streams one video, named by PSVGUARD_CLIP, through the pipeline with the default policy
// and logs when it is covered and when psv decides failures. Use it to check new kinds of content
// for false covers.
func TestClip(t *testing.T) {
	path := os.Getenv("PSVGUARD_CLIP")
	if path == "" {
		t.Skip("set PSVGUARD_CLIP to a video file")
	}

	dir, rep := clipDir(t, path)
	r := streamClip(t, dir, coverage.DefaultParams())
	t.Logf("%s: psv verdict %s (first failing frame %v); first cover %v, covered %v of %v, max hot %.1f%%",
		filepath.Base(path), rep.Verdict, r.firstFail, r.covered, r.coverTime.Round(time.Millisecond), r.duration.Round(time.Millisecond), r.maxHot)
}

// monospaceFont returns a font file for drawing text, or "" when none is found.
func monospaceFont() string {
	for _, f := range []string{
		"/System/Library/Fonts/Menlo.ttc",
		"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
		"/usr/share/fonts/TTF/DejaVuSansMono.ttf",
	} {
		if _, err := os.Stat(f); err == nil {
			return f
		}
	}

	return ""
}

// TestSyntheticClips checks content made to sit on either side of the policy: full-screen
// flashing must be covered before psv's first failing frame, and scrolling or sliding text, which
// makes pixels transition just as often but out of step, must stay uncovered.
func TestSyntheticClips(t *testing.T) {
	if testing.Short() {
		t.Skip("generates and decodes videos")
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	tmp := t.TempDir()
	clips := []struct {
		name, src, vf string
		text          bool
		flashing      bool
	}{
		// Three seconds of grey, then the whole frame flashing five times a second.
		{"flash", "color=c=gray:s=1280x720:r=60:d=6", "format=yuv420p,geq=lum='if(lt(T,3),128,if(lt(mod(T-3,0.2),0.1),235,16))':cb=128:cr=128", false, true},
		{"scroll-fast", "color=c=white:s=1280x720:r=60:d=4", "drawtext=fontfile=FONT:textfile=TEXT:fontsize=22:fontcolor=black:x=40:y=40-t*1500", true, false},
		{"scroll-slow", "color=c=white:s=1280x720:r=60:d=4", "drawtext=fontfile=FONT:textfile=TEXT:fontsize=22:fontcolor=black:x=40:y=40-t*400", true, false},
		{"scroll-dark", "color=c=0x1e1e1e:s=1280x720:r=60:d=4", "drawtext=fontfile=FONT:textfile=TEXT:fontsize=16:fontcolor=0xd4d4d4:x=40:y=40-t*900", true, false},
		{"slide", "color=c=white:s=1280x720:r=60:d=3", "drawtext=fontfile=FONT:textfile=TEXT:fontsize=22:fontcolor=black:x='if(lt(t,1),40,if(lt(t,1.4),40-(t-1)/0.4*1400,-1360))':y=40", true, false},
	}

	font := monospaceFont()

	words := strings.Fields("the quick brown fox jumps over lazy dog lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor")

	var text strings.Builder

	for line := range 400 {
		for w := range 14 {
			text.WriteString(words[(line*7+w*3)%len(words)] + " ")
		}

		text.WriteString("\n")
	}

	if err := os.WriteFile(filepath.Join(tmp, "text.txt"), []byte(text.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range clips {
		t.Run(c.name, func(t *testing.T) {
			if c.text && font == "" {
				t.Skip("no monospace font found for drawtext")
			}

			out := filepath.Join(tmp, c.name+".mp4")
			vf := strings.ReplaceAll(strings.ReplaceAll(c.vf, "FONT", font), "TEXT", "text.txt")

			cmd := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", c.src, "-vf", vf,
				"-c:v", "libx264", "-pix_fmt", "yuv420p", out)
			cmd.Dir = tmp

			if b, err := cmd.CombinedOutput(); err != nil {
				t.Skipf("ffmpeg could not make the clip: %v: %s", err, b)
			}

			dir, rep := clipDir(t, out)
			r := streamClip(t, dir, coverage.DefaultParams())

			t.Logf("%s: psv %s, first failing frame %v; first cover %v, covered %v of %v",
				c.name, rep.Verdict, r.firstFail, r.covered, r.coverTime.Round(time.Millisecond), r.duration.Round(time.Millisecond))

			if !c.flashing {
				if !rep.Passed {
					t.Fatalf("psv fails %s; the clip no longer tests a false cover", c.name)
				}

				if r.covered >= 0 {
					t.Fatalf("%s covered at %v", c.name, r.covered)
				}

				return
			}

			if r.covered < 0 || r.covered >= r.firstFail {
				t.Fatalf("covered at %v; want before the first failing frame at %v", r.covered, r.firstFail)
			}
		})
	}
}
