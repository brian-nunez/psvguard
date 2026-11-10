// Package app is psvguard's controller: events in, commands out. It reads the platform's events
// from the bus, runs one monitor per display, decides what to cover and what to show, and posts
// commands back. It depends only on the bus contract, so it runs and is tested on any platform.
package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/brian-nunez/psvguard/internal/bridge"
	"github.com/brian-nunez/psvguard/internal/coverage"
	"github.com/brian-nunez/psvguard/internal/monitor"
	"github.com/brian-nunez/psvguard/internal/settings"
	"github.com/brian-nunez/psvguard/internal/ui"
)

// Cover fades. Covers appear quickly and leave slowly; each is a single change of brightness, far
// from the several a second that flashing takes.
const (
	FadeIn  = 0.15
	FadeOut = 1.0
)

// Saver stores settings.
type Saver interface {
	Save(settings.Settings) error
}

// Options configure a controller.
type Options struct {
	Bus      bridge.Bus
	Settings settings.Settings
	Store    Saver
	// Log receives diagnostics and, every StatsEvery, one line per display. Nil discards them.
	Log     *log.Logger
	Version string
	// StatsEvery is how often stats are sent and logged; zero means one second.
	StatsEvery time.Duration
	// RetryEvery is how often a missing permission is checked again and failed streams are
	// retried; zero means two seconds.
	RetryEvery time.Duration
}

// stream is one display's capture stream and its monitor.
type stream struct {
	id       uint64
	display  bridge.Display
	cfg      monitor.Config
	mon      *monitor.Display
	cancel   context.CancelFunc
	started  bool
	stopping bool
	rects    []coverage.Rect
	// changedPTS is the presentation time of the frame that last changed the covers, and
	// activityPTS of the first transition of the burst that led to it.
	changedPTS  int64
	activityPTS int64
	prev        monitor.Stats
	prevAt      time.Time
}

// Controller is the state machine. All of its state is owned by the Run goroutine.
type Controller struct {
	o   Options
	bus bridge.Bus
	log *log.Logger
	s   settings.Settings

	caps     bridge.Capabilities
	haveCaps bool
	ready    bool
	perm     bridge.Permission
	asked    bool
	sleeping bool
	displays map[string]bridge.Display

	streams    map[uint64]*stream
	current    map[string]uint64 // display ID → its live stream
	failedAt   map[string]time.Time
	nextStream uint64
	reports    chan monitor.Report

	warned  bool
	message string
	msgAt   time.Time
	quit    bool
}

// New returns a controller.
func New(o Options) *Controller {
	if o.Log == nil {
		o.Log = log.New(io.Discard, "", 0)
	}

	if o.StatsEvery <= 0 {
		o.StatsEvery = time.Second
	}

	if o.RetryEvery <= 0 {
		o.RetryEvery = 2 * time.Second
	}

	return &Controller{
		o:        o,
		bus:      o.Bus,
		log:      o.Log,
		s:        o.Settings,
		perm:     bridge.PermissionUnknown,
		displays: map[string]bridge.Display{},
		streams:  map[uint64]*stream{},
		current:  map[string]uint64{},
		failedAt: map[string]time.Time{},
		reports:  make(chan monitor.Report, 64),
	}
}

// Run handles events until a quit action or ctx ends.
func (c *Controller) Run(ctx context.Context) error {
	stats := time.NewTicker(c.o.StatsEvery)
	defer stats.Stop()

	retry := time.NewTicker(c.o.RetryEvery)
	defer retry.Stop()

	events := c.bus.Events()

	for !c.quit {
		select {
		case <-ctx.Done():
			c.shutdown()

			return ctx.Err()
		case e := <-events:
			c.handle(e)
		case r := <-c.reports:
			c.onReport(r)
		case <-stats.C:
			c.postStats()
		case <-retry.C:
			c.retry()
		}
	}

	return nil
}

func (c *Controller) handle(e bridge.Event) {
	switch e := e.(type) {
	case bridge.CapabilitiesEvent:
		c.caps, c.haveCaps = e.Capabilities, true
		c.log.Printf("platform: excludes own windows %v, can overlay %v, per-display capture %v, consent per session %v, formats %v",
			e.ExcludesOwnWindows, e.CanOverlay, e.PerDisplayCapture, e.ConsentPerSession, e.PixelFormats)
		c.reconcile()
	case bridge.ReadyEvent:
		c.ready = true
		c.bus.Post(bridge.SetControls{Controls: ui.Controls(c.s)})
		c.bus.Post(bridge.CheckPermission{})

		if c.s.ShowPanel {
			c.bus.Post(bridge.ShowPanel{})
		}
	case bridge.DisplaysEvent:
		c.displays = map[string]bridge.Display{}
		for _, d := range e.Displays {
			c.displays[d.ID] = d
		}

		c.log.Printf("displays: %s", describe(e.Displays))
		c.reconcile()
	case bridge.PermissionEvent:
		c.onPermission(e.State)
	case bridge.StreamEvent:
		c.onStream(e)
	case bridge.PowerEvent:
		c.sleeping = e.Sleeping
		if e.Sleeping {
			c.log.Printf("system sleeping: stopping capture")
		} else {
			c.log.Printf("system awake: checking permission")
			c.bus.Post(bridge.CheckPermission{})
		}

		c.reconcile()
	case bridge.ActionEvent:
		c.onAction(e.Action)
	case bridge.CoversShown:
		c.onCoversShown(e)
	case bridge.Transition:
		// The animation can look like flashing; covers it started are withdrawn.
		for _, s := range c.streams {
			if !s.stopping {
				s.mon.Transition()
			}
		}
	}

	c.postStatus()
}

func (c *Controller) onPermission(p bridge.Permission) {
	if p != c.perm {
		c.log.Printf("capture permission: %s", p)
	}

	c.perm = p

	// Ask once per session: platforms that need consent every session show their prompt, and
	// first launches get the system's prompt instead of a dead end.
	if p == bridge.PermissionNeedsConsent && !c.asked && c.s.Enabled {
		c.asked = true
		c.bus.Post(bridge.RequestPermission{})
	}

	c.reconcile()
}

func (c *Controller) onStream(e bridge.StreamEvent) {
	s := c.streams[e.Stream]

	switch e.Kind {
	case bridge.EvStreamStarted:
		if s != nil {
			s.started = true
			c.log.Printf("display %s: capture started (stream %d, %dx%d at %d fps)", e.Display, e.Stream, s.cfg.Width, s.cfg.Height, s.cfg.FPS)
		}
	case bridge.EvStreamFailed:
		c.log.Printf("display %s: capture failed (stream %d): %s", e.Display, e.Stream, e.Error)
		c.failedAt[e.Display] = time.Now()
		c.setMessage("Capture failed: " + e.Error)

		if s != nil {
			c.stop(s, true)
		}

		// A revoked permission shows up as a failed stream.
		c.bus.Post(bridge.CheckPermission{})
	case bridge.EvStreamStopped:
		if s != nil {
			s.cancel()
			delete(c.streams, e.Stream)

			if c.current[e.Display] == e.Stream {
				delete(c.current, e.Display)
			}
		}
	}
}

func (c *Controller) onAction(a ui.Action) {
	switch a.Kind {
	case ui.Toggle:
		c.setSetting("enabled", fmt.Sprint(!c.s.Enabled))
	case ui.SetSetting:
		c.setSetting(a.Key, a.Value)
	case ui.RequestPermission:
		c.bus.Post(bridge.RequestPermission{})
	case ui.OpenPermission:
		c.bus.Post(bridge.OpenPermissionPane{})
	case ui.Quit:
		c.shutdown()
	}
}

func (c *Controller) setSetting(key, value string) {
	next := c.s
	if err := next.Set(key, value); err != nil {
		c.setMessage(err.Error())
		c.bus.Post(bridge.SetControls{Controls: ui.Controls(c.s)})

		return
	}

	if next == c.s {
		return
	}

	c.s = next
	c.log.Printf("setting %s = %s", key, value)

	if c.o.Store != nil {
		if err := c.o.Store.Save(c.s); err != nil {
			c.log.Printf("%v", err)
			c.setMessage(err.Error())
		}
	}

	c.bus.Post(bridge.SetControls{Controls: ui.Controls(c.s)})

	if key == "enabled" && c.s.Enabled && c.perm == bridge.PermissionNeedsConsent && !c.asked {
		c.asked = true
		c.bus.Post(bridge.RequestPermission{})
	}

	params := c.s.Params(c.caps.ExcludesOwnWindows)
	for _, s := range c.streams {
		if !s.stopping {
			s.mon.SetParams(params)
		}
	}

	// Restart-only options take effect through reconcile, which replaces streams whose
	// configuration no longer matches.
	c.reconcile()
}

func (c *Controller) canCapture() bool {
	return c.ready && c.haveCaps && c.perm == bridge.PermissionGranted && c.s.Enabled && !c.sleeping
}

// reconcile starts, replaces and stops streams so there is exactly one live stream per connected
// display, with the current configuration, whenever capture is allowed.
func (c *Controller) reconcile() {
	if !c.canCapture() {
		c.stopAll()

		return
	}

	ids := make([]string, 0, len(c.displays))
	for id := range c.displays {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	for _, id := range ids {
		d := c.displays[id]

		cfg, ok := c.configFor(d)
		if !ok {
			continue
		}

		if sid, ok := c.current[id]; ok {
			s := c.streams[sid]
			if s.cfg.Width == cfg.Width && s.cfg.Height == cfg.Height && s.cfg.FPS == cfg.FPS && s.cfg.Thresholds == cfg.Thresholds {
				s.display = d

				continue
			}

			c.log.Printf("display %s: configuration changed, restarting analysis", id)
			c.stop(s, false)
		}

		if t, failed := c.failedAt[id]; failed && time.Since(t) < c.o.RetryEvery {
			continue
		}

		c.start(d, cfg)
	}

	for id, sid := range c.current {
		if _, ok := c.displays[id]; !ok {
			c.log.Printf("display %s: disconnected", id)
			c.stop(c.streams[sid], true)
		}
	}
}

func (c *Controller) configFor(d bridge.Display) (monitor.Config, bool) {
	w, h := monitor.CaptureSize(d.Width, d.Height, c.s.CaptureWidth)
	if w == 0 {
		return monitor.Config{}, false
	}

	return monitor.Config{
		Display:    d.ID,
		Width:      w,
		Height:     h,
		FPS:        c.s.FPS,
		Thresholds: monitor.ScaleThresholds(c.s.Thresholds(), float64(w)/d.Width),
		Params:     c.s.Params(c.caps.ExcludesOwnWindows),
	}, true
}

func (c *Controller) start(d bridge.Display, cfg monitor.Config) {
	format, ok := c.caps.Format()
	if !ok {
		c.setMessage("The platform delivers no supported pixel format.")

		return
	}

	c.nextStream++
	id := c.nextStream

	mon, err := monitor.NewDisplay(cfg, id, c.reports)
	if err != nil {
		c.log.Printf("display %s: %v", d.ID, err)
		c.setMessage(err.Error())
		c.failedAt[d.ID] = time.Now()

		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &stream{id: id, display: d, cfg: cfg, mon: mon, cancel: cancel, prevAt: time.Now()}
	c.streams[id], c.current[d.ID] = s, id
	delete(c.failedAt, d.ID)

	go mon.Run(ctx)

	c.bus.Post(bridge.StartCapture{
		Stream: id, Display: d.ID, Width: cfg.Width, Height: cfg.Height, FPS: cfg.FPS,
		Format: format, Target: mon,
	})
}

// stop stops a stream. clearCovers removes its display's covers unless another stream has replaced it.
func (c *Controller) stop(s *stream, clearCovers bool) {
	if s == nil || s.stopping {
		return
	}

	s.stopping = true
	s.cancel()
	c.bus.Post(bridge.StopCapture{Stream: s.id, Display: s.display.ID})

	if c.current[s.display.ID] == s.id {
		delete(c.current, s.display.ID)
	}

	if clearCovers && len(s.rects) > 0 {
		c.bus.Post(bridge.ClearCovers{Display: s.display.ID})
	}

	s.rects = nil
}

func (c *Controller) stopAll() {
	for _, s := range c.streams {
		c.stop(s, true)
	}

	c.warned = false
}

func (c *Controller) retry() {
	if c.ready && c.perm != bridge.PermissionGranted {
		c.bus.Post(bridge.CheckPermission{})
	}

	c.reconcile()
	c.postStatus()
}

func (c *Controller) onReport(r monitor.Report) {
	s := c.streams[r.Stream]
	if s == nil || s.stopping || c.current[r.Display] != r.Stream {
		return
	}

	if r.Err != nil {
		c.log.Printf("display %s: %v", r.Display, r.Err)
		c.setMessage(r.Err.Error())
		c.failedAt[r.Display] = time.Now()
		c.stop(s, true)
		c.postStatus()

		return
	}

	was := len(s.rects) > 0
	s.rects, s.changedPTS, s.activityPTS = r.Rects, r.PTS, r.ActivityPTS

	switch {
	case !was && len(r.Rects) > 0:
		c.log.Printf("display %s: covering %s (hot area %.1f%%, peak %d, fill %.2f)", r.Display, describeRects(r.Rects, r.Full), r.Decision.HotArea, r.Decision.Peak, r.Decision.Fill)
	case r.Transition:
		c.log.Printf("display %s: covers withdrawn after a display transition (%d left)", r.Display, len(r.Rects))
	case was && len(r.Rects) == 0:
		c.log.Printf("display %s: uncovered", r.Display)
	default:
		c.log.Printf("display %s: covers now %s", r.Display, describeRects(r.Rects, r.Full))
	}

	if c.caps.CanOverlay {
		c.bus.Post(bridge.SetCovers{Display: r.Display, FadeIn: FadeIn, FadeOut: FadeOut, Rects: r.Rects})
	} else if !was && len(r.Rects) > 0 && !c.warned {
		// Without overlays the best psvguard can do is warn.
		c.warned = true
		c.bus.Post(bridge.Notify{Title: "Flashing on screen", Body: "PSV Guard found flashing content. Look away until it stops."})
	}

	if !c.anyCovering() {
		c.warned = false
	}

	c.postStatus()
}

func (c *Controller) onCoversShown(e bridge.CoversShown) {
	sid, ok := c.current[e.Display]
	if !ok {
		return
	}

	if s := c.streams[sid]; s != nil && s.changedPTS > 0 && e.At > 0 {
		c.log.Printf("display %s: %d covers applied %.1f ms after the frame that changed them, %.1f ms after the burst's first transition",
			e.Display, e.Rects, float64(e.At-s.changedPTS)/1e6, float64(e.At-s.activityPTS)/1e6)
	}
}

func (c *Controller) anyCovering() bool {
	for _, sid := range c.current {
		if s := c.streams[sid]; s != nil && len(s.rects) > 0 {
			return true
		}
	}

	return false
}

func (c *Controller) setMessage(m string) { c.message, c.msgAt = m, time.Now() }

func (c *Controller) status() ui.Status {
	st := ui.Status{Enabled: c.s.Enabled, Disclaimer: ui.Disclaimer, Version: c.o.Version}

	switch {
	case !c.ready || !c.haveCaps:
		st.State, st.Title = ui.Starting, "Starting…"
	case c.perm != bridge.PermissionGranted && c.perm != bridge.PermissionUnknown:
		st.State, st.Title, st.PermissionButton = ui.Permission, "Screen capture permission needed", true
		st.Detail = "Allow PSV Guard to capture the screen in your system settings, then quit and reopen PSV Guard."
	case c.perm == bridge.PermissionUnknown:
		st.State, st.Title = ui.Starting, "Checking screen capture permission…"
	case !c.s.Enabled:
		st.State, st.Title, st.Detail = ui.Paused, "Protection is off", "Turn it on to watch the screen for flashing."
	case c.sleeping:
		st.State, st.Title = ui.Paused, "Paused while the system sleeps"
	case c.anyCovering() && !c.caps.CanOverlay:
		st.State, st.Title = ui.Warning, "Flashing on screen"
		st.Detail = "This system cannot draw covers over other apps. Look away until it stops."
	case c.anyCovering():
		st.State, st.Title = ui.Covering, "Covering flashing"
		st.Detail = "Covered areas uncover after a few calm seconds."
	default:
		n := len(c.current)
		st.State = ui.Watching
		st.Title = fmt.Sprintf("Watching %d display%s", n, plural(n))
		st.Detail = "Flashing that builds up is covered before it can fail WCAG."
	}

	if c.message != "" && time.Since(c.msgAt) < 10*time.Second {
		if st.Detail != "" {
			st.Detail += " "
		}

		st.Detail += c.message
	}

	return st
}

func (c *Controller) postStatus() { c.bus.Post(bridge.SetStatus{Status: c.status()}) }

func (c *Controller) postStats() {
	ids := make([]string, 0, len(c.current))
	for id := range c.current {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	var out ui.Stats

	now := time.Now()

	for _, id := range ids {
		s := c.streams[c.current[id]]
		st := s.mon.Stats()
		secs := now.Sub(s.prevAt).Seconds()
		frames := st.Frames - s.prev.Frames
		d := ui.DisplayStats{
			ID: id, Name: s.display.Name, Size: fmt.Sprintf("%dx%d", s.cfg.Width, s.cfg.Height),
			HotArea: st.HotArea, Covers: st.Covers, Dropped: st.Dropped, Repeated: st.Repeats, Misses: st.Misses,
		}

		if secs > 0 {
			d.FPS = float64(frames) / secs
			d.Captured = float64(st.Captured-s.prev.Captured) / secs
		}

		if frames > 0 {
			d.WatchMs = float64(st.WatchTotal-s.prev.WatchTotal) / float64(frames) / 1e6
		}

		c.log.Printf("display %s: %.0f fps analysed, %.0f fps captured, watch %.2f ms (max %.2f), callback %.3f ms (max %.3f), hot %.1f%%, covers %d, dropped %d, repeated %d, decided failures %d, misses %d",
			id, d.FPS, d.Captured, d.WatchMs, float64(st.WatchMax)/1e6, float64(st.CallbackMean)/1e6, float64(st.CallbackMax)/1e6,
			d.HotArea, d.Covers, d.Dropped, d.Repeated, st.DecidedFailures, d.Misses)

		if st.Misses > s.prev.Misses {
			c.log.Printf("display %s: %d decided failures were not covered", id, st.Misses-s.prev.Misses)
		}

		s.prev, s.prevAt = st, now

		out.Displays = append(out.Displays, d)
	}

	c.bus.Post(bridge.SetStats{Stats: out})
}

// shutdown stops everything and asks the platform to quit.
func (c *Controller) shutdown() {
	if c.quit {
		return
	}

	c.quit = true

	for _, s := range c.streams {
		if !s.stopping {
			s.stopping = true
			s.cancel()
			c.bus.Post(bridge.StopCapture{Stream: s.id, Display: s.display.ID})
		}
	}

	c.bus.Post(bridge.ClearCovers{})
	c.bus.Post(bridge.Quit{})
}

func describe(ds []bridge.Display) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = fmt.Sprintf("%s %q %.0fx%.0f (%dx%d px)", d.ID, d.Name, d.Width, d.Height, d.PixelWidth, d.PixelHeight)
	}

	if len(parts) == 0 {
		return "none"
	}

	return strings.Join(parts, ", ")
}

func describeRects(rs []coverage.Rect, full bool) string {
	if full {
		return "the whole display"
	}

	return fmt.Sprintf("%d area%s", len(rs), plural(len(rs)))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}

	return "s"
}
