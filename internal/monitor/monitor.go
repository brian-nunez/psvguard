// Package monitor runs one display's analysis: frames from the capture fast path are resampled
// to a fixed rate, watched by a psv Analyzer, and turned into cover decisions.
package monitor

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brian-nunez/psv"

	"github.com/brian-nunez/psvguard/internal/coverage"
	"github.com/brian-nunez/psvguard/internal/frame"
	"github.com/brian-nunez/psvguard/internal/resample"
)

// Config sets up one display's analysis.
type Config struct {
	Display string
	// Width and Height are the size of the analysed frames.
	Width, Height int
	// FPS is the fixed analysis rate.
	FPS        int
	Thresholds psv.Thresholds
	Params     coverage.Params
}

// ScaleThresholds converts the region size of th, given in reference pixels (WCAG's CSS pixels,
// points on macOS), to analysed pixels, scale being analysed pixels per reference pixel. Area
// thresholds are percentages and need no change.
func ScaleThresholds(th psv.Thresholds, scale float64) psv.Thresholds {
	if scale <= 0 {
		return th
	}

	scaleDim := func(v int) int {
		if v == 0 {
			return 0
		}

		return max(1, int(math.Round(float64(v)*scale)))
	}

	th.RegionWidth, th.RegionHeight = scaleDim(th.RegionWidth), scaleDim(th.RegionHeight)

	return th
}

// CaptureSize returns the analysed frame size for a display of width×height logical units at the
// requested analysis width: the display's aspect ratio is kept and both sides are even.
func CaptureSize(width, height float64, analysisWidth int) (int, int) {
	if width <= 0 || height <= 0 || analysisWidth < 2 {
		return 0, 0
	}

	w := analysisWidth &^ 1
	h := int(math.Round(float64(w)*height/width/2)) * 2

	return w, max(h, 2)
}

// Result is the outcome of one tick.
type Result struct {
	// Frame is the analyser's frame number.
	Frame int
	// PTS is the presentation time of the frame watched.
	PTS int64
	// Repeat reports that the previous frame was watched again.
	Repeat   bool
	Decision coverage.Decision
	// Changed reports that the covers differ from the previous tick's.
	Changed bool
	// DecidedFailures counts failing frames psv decided on this tick, and Misses those of them
	// that were not covered when they were watched.
	DecidedFailures, Misses int
	// FirstMissFrame is the first missed frame decided on this tick, or zero.
	FirstMissFrame int
	// ActivityPTS is the presentation time of the first frame with any transition after at least
	// a second without one: when the current burst of activity began on screen.
	ActivityPTS int64
	Watch       time.Duration
}

// Counters are a pipeline's running totals.
type Counters struct {
	Frames          uint64
	Fresh           uint64
	Repeats         uint64
	Superseded      uint64
	DecidedFailures uint64
	Misses          uint64
	WatchTotal      time.Duration
	WatchMax        time.Duration
}

// Pipeline is one display's analysis, driven synchronously: Offer frames, then Step on a clock.
// It is not safe for concurrent use.
type Pipeline struct {
	cfg      Config
	analyzer *psv.Analyzer
	res      *resample.Resampler
	tracker  *coverage.Tracker

	rects   []coverage.Rect
	covered []bool // covering state by frame number, a ring of a few seconds
	counts  Counters
	last    coverage.Decision

	// activityPTS is when the current burst of transitions began; lastActive is the clock time of
	// the latest frame with a transition.
	activityPTS int64
	lastActive  time.Duration
	active      bool
}

// NewPipeline returns a pipeline whose resampler returns released frames with release.
func NewPipeline(cfg Config, release func(*frame.Slot)) (*Pipeline, error) {
	a, err := psv.NewAnalyzer(float64(cfg.FPS), cfg.Width, cfg.Height, psv.Config{Thresholds: cfg.Thresholds})
	if err != nil {
		return nil, fmt.Errorf("start analyser for display %s: %w", cfg.Display, err)
	}

	return &Pipeline{
		cfg:      cfg,
		analyzer: a,
		res:      resample.New(cfg.FPS, release),
		tracker:  coverage.NewTracker(cfg.Params),
		covered:  make([]bool, 3*cfg.FPS+1),
	}, nil
}

// Offer hands over a received frame.
func (p *Pipeline) Offer(s *frame.Slot) { p.res.Offer(s) }

// SetParams changes the covering policy.
func (p *Pipeline) SetParams(params coverage.Params) {
	p.cfg.Params = params
	p.tracker.SetParams(params)
}

// Step watches one frame if a tick is due at now and returns its result. ok is false when no
// tick was due or no frame has arrived yet.
func (p *Pipeline) Step(now time.Duration) (r Result, ok bool, err error) {
	if !p.res.Due(now) {
		return Result{}, false, nil
	}

	s, repeat := p.res.Tick()
	if s == nil {
		return Result{}, false, nil
	}

	start := time.Now()

	u, err := p.analyzer.Watch(&s.Frame)
	if err != nil {
		return Result{}, false, fmt.Errorf("watch display %s: %w", p.cfg.Display, err)
	}

	r = Result{Frame: u.Frame, PTS: s.PTS, Repeat: repeat, Watch: time.Since(start)}

	if u.Building.Luminance.Peak > 0 || u.Building.Red.Peak > 0 {
		if !p.active || now-p.lastActive > time.Second {
			p.activityPTS = s.PTS
		}

		p.active, p.lastActive = true, now
	}

	r.ActivityPTS = p.activityPTS

	// Decided failures are the backstop: one that was not covered when its frame was watched is
	// a miss, and its flashing area is covered now.
	for i := range u.Decided {
		res := &u.Decided[i]
		if !res.Failed() {
			continue
		}

		f := u.DecidedFrom + i
		r.DecidedFailures++

		if p.covered[f%len(p.covered)] {
			continue
		}

		r.Misses++
		if r.FirstMissFrame == 0 {
			r.FirstMissFrame = f
		}

		p.tracker.Force(now, res.Luminance.Mask)
		p.tracker.Force(now, res.Red.Mask)
	}

	r.Decision = p.tracker.Update(now, u.Building)
	p.last = r.Decision
	p.covered[u.Frame%len(p.covered)] = r.Decision.Covering()

	if !coverage.Equal(p.rects, r.Decision.Rects) {
		r.Changed = true
		p.rects = append(p.rects[:0], r.Decision.Rects...)
	}

	p.counts.Frames++
	p.counts.DecidedFailures += uint64(r.DecidedFailures)
	p.counts.Misses += uint64(r.Misses)
	p.counts.WatchTotal += r.Watch
	p.counts.WatchMax = max(p.counts.WatchMax, r.Watch)

	return r, true, nil
}

// Transition withdraws covers begun in the last second, unless a decided failure forced them,
// and holds off new covers briefly: a system animation moved the whole display. It returns the
// decision afterwards and whether the covers changed.
func (p *Pipeline) Transition(now time.Duration) (coverage.Decision, bool) {
	p.tracker.Transition(now, time.Second, 600*time.Millisecond)
	d := p.tracker.Recompute(now)
	p.last = d

	changed := !coverage.Equal(p.rects, d.Rects)
	if changed {
		p.rects = append(p.rects[:0], d.Rects...)
	}

	return d, changed
}

// Counters returns the running totals.
func (p *Pipeline) Counters() Counters {
	c := p.counts
	st := p.res.Stats()
	c.Fresh, c.Repeats, c.Superseded = st.Fresh, st.Repeats, st.Superseded+st.Stale

	return c
}

// Last returns the latest decision.
func (p *Pipeline) Last() coverage.Decision { return p.last }

// Close ends the analysis and releases the frames held.
func (p *Pipeline) Close() {
	_, _ = p.analyzer.Stop()
	p.res.Reset()
}

// Report tells the controller that a display's covers changed.
type Report struct {
	Display string
	Stream  uint64
	Rects   []coverage.Rect
	Full    bool
	// PTS is the presentation time of the frame that changed the covers, and ActivityPTS of the
	// frame that began the burst of transitions leading to it.
	PTS         int64
	ActivityPTS int64
	Decision    coverage.Decision
	// Transition reports that the change follows a system animation rather than a frame.
	Transition bool
	// Err ends the stream's analysis.
	Err error
}

// Stats is a snapshot of one display's analysis.
type Stats struct {
	Counters
	Captured     uint64
	Dropped      uint64
	CallbackMean time.Duration
	CallbackMax  time.Duration
	HotArea      float64
	Covers       int
	Covering     bool
}

// poolSize is the number of frames per display: one being converted by the capture thread, one in
// the hand-off channel, and the resampler's pending and current frames, plus one spare.
const poolSize = 5

// Display is one display's analysis goroutine and the frame fast path that feeds it.
type Display struct {
	cfg    Config
	stream uint64
	pool   *frame.Pool
	in     chan *frame.Slot
	params chan coverage.Params
	trans  chan struct{}
	out    chan<- Report
	pipe   *Pipeline

	captured, dropped, cbCount atomic.Uint64
	cbTotal, cbMax             atomic.Int64

	mu    sync.Mutex
	stats Stats
}

// NewDisplay returns a display analysis that sends cover changes to out.
func NewDisplay(cfg Config, stream uint64, out chan<- Report) (*Display, error) {
	pool := frame.NewPool(poolSize, cfg.Width, cfg.Height)

	pipe, err := NewPipeline(cfg, pool.Put)
	if err != nil {
		return nil, err
	}

	return &Display{
		cfg:    cfg,
		stream: stream,
		pool:   pool,
		in:     make(chan *frame.Slot, 1),
		params: make(chan coverage.Params, 1),
		trans:  make(chan struct{}, 1),
		out:    out,
		pipe:   pipe,
	}, nil
}

// Config returns the display's configuration.
func (d *Display) Config() Config { return d.cfg }

// Stream returns the capture stream the display belongs to.
func (d *Display) Stream() uint64 { return d.stream }

// CaptureFrame converts one captured buffer straight into a pooled frame and hands it to the
// analysis goroutine. It runs on the capture thread: it never blocks, keeps no reference to src
// and allocates nothing. If analysis is behind, the older waiting frame is dropped; if no pooled
// frame is free, this one is.
func (d *Display) CaptureFrame(format frame.Format, src []byte, width, height, stride int, pts int64) {
	start := time.Now()
	defer d.timeCallback(start)

	d.captured.Add(1)

	s := d.pool.Get()
	if s == nil {
		d.dropped.Add(1)

		return
	}

	if err := frame.Convert(&s.Frame, format, src, width, height, stride); err != nil {
		d.pool.Put(s)
		d.dropped.Add(1)

		return
	}

	s.PTS = pts

	select {
	case d.in <- s:
		return
	default:
	}

	select {
	case old := <-d.in:
		d.pool.Put(old)
		d.dropped.Add(1)
	default:
	}

	select {
	case d.in <- s:
	default:
		d.pool.Put(s)
		d.dropped.Add(1)
	}
}

func (d *Display) timeCallback(start time.Time) {
	ns := int64(time.Since(start))
	d.cbTotal.Add(ns)
	d.cbCount.Add(1)

	for {
		m := d.cbMax.Load()
		if ns <= m || d.cbMax.CompareAndSwap(m, ns) {
			return
		}
	}
}

// SetParams changes the covering policy; the analysis goroutine applies it.
func (d *Display) SetParams(p coverage.Params) {
	select {
	case <-d.params:
	default:
	}

	d.params <- p
}

// Transition tells the analysis goroutine that a system animation moved the whole display.
func (d *Display) Transition() {
	select {
	case d.trans <- struct{}{}:
	default:
	}
}

// Run analyses frames at the configured rate until ctx ends.
func (d *Display) Run(ctx context.Context) {
	defer d.pipe.Close()

	interval := time.Second / time.Duration(d.cfg.FPS)
	ticker := time.NewTicker(interval)

	defer ticker.Stop()

	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case p := <-d.params:
			d.pipe.SetParams(p)
		case <-d.trans:
			if dec, changed := d.pipe.Transition(time.Since(start)); changed {
				d.send(ctx, Report{Display: d.cfg.Display, Stream: d.stream, Rects: dec.Rects, Full: dec.Full, Decision: dec, Transition: true})
			}
		case s := <-d.in:
			d.pipe.Offer(s)
		case <-ticker.C:
			r, ok, err := d.pipe.Step(time.Since(start))
			if err != nil {
				d.send(ctx, Report{Display: d.cfg.Display, Stream: d.stream, Err: err})

				return
			}

			if !ok {
				continue
			}

			d.update(r)

			if r.Changed {
				d.send(ctx, Report{
					Display: d.cfg.Display, Stream: d.stream,
					Rects: r.Decision.Rects, Full: r.Decision.Full,
					PTS: r.PTS, ActivityPTS: r.ActivityPTS, Decision: r.Decision,
				})
			}
		}
	}
}

func (d *Display) send(ctx context.Context, r Report) {
	select {
	case d.out <- r:
	case <-ctx.Done():
	}
}

func (d *Display) update(r Result) {
	d.mu.Lock()
	d.stats.Counters = d.pipe.Counters()
	d.stats.HotArea = r.Decision.HotArea
	d.stats.Covers = len(r.Decision.Rects)
	d.stats.Covering = r.Decision.Covering()
	d.mu.Unlock()
}

// Stats returns a snapshot of the display's analysis.
func (d *Display) Stats() Stats {
	d.mu.Lock()
	s := d.stats
	d.mu.Unlock()

	s.Captured, s.Dropped = d.captured.Load(), d.dropped.Load()
	if n := d.cbCount.Load(); n > 0 {
		s.CallbackMean = time.Duration(d.cbTotal.Load() / int64(n))
	}

	s.CallbackMax = time.Duration(d.cbMax.Load())

	return s
}
