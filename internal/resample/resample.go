// Package resample turns frames that arrive only when the screen changes into a fixed-rate
// stream, as psv's analyser expects: on every tick it hands out the newest frame received, or the
// previous one again when nothing new arrived.
package resample

import (
	"time"

	"github.com/brian-nunez/psvguard/internal/frame"
)

// Stats counts what the resampler did.
type Stats struct {
	// Ticks is the number of frames handed out.
	Ticks uint64
	// Fresh is the number of ticks that handed out a newly received frame.
	Fresh uint64
	// Repeats is the number of ticks that repeated the previous frame.
	Repeats uint64
	// Superseded is the number of received frames replaced by a newer one before a tick used
	// them.
	Superseded uint64
	// Stale is the number of received frames dropped because their presentation time was not
	// after the frame already held.
	Stale uint64
}

// Resampler holds the frame being watched and the newest frame received. It is not safe for
// concurrent use; one display's analysis goroutine owns it.
type Resampler struct {
	interval time.Duration
	release  func(*frame.Slot)

	// current is the frame handed out by the last tick. It stays out of the pool until a newer
	// frame replaces it, because the next tick may repeat it.
	current *frame.Slot
	// pending is the newest frame received since the last tick.
	pending *frame.Slot

	next  time.Duration
	stats Stats
}

// New returns a resampler at fps frames per second. release returns frames it no longer needs to
// their pool.
func New(fps int, release func(*frame.Slot)) *Resampler {
	if fps < 1 {
		fps = 1
	}

	return &Resampler{interval: time.Second / time.Duration(fps), release: release}
}

// Interval returns the time between ticks.
func (r *Resampler) Interval() time.Duration { return r.interval }

// Offer takes a newly received frame. A frame older than the one held is dropped; a frame that
// arrives before the previous one was used replaces it.
func (r *Resampler) Offer(s *frame.Slot) {
	if s == nil {
		return
	}

	newest := r.pending
	if newest == nil {
		newest = r.current
	}

	if newest != nil && s.PTS <= newest.PTS {
		r.stats.Stale++
		r.release(s)

		return
	}

	if r.pending != nil {
		r.stats.Superseded++
		r.release(r.pending)
	}

	r.pending = s
}

// Due reports whether a tick is due at now, the time on the caller's clock, and schedules the next
// one. A tick up to a quarter interval early counts, so a timer firing at the frame rate never
// skips one; ticks that fell further behind than one interval are skipped rather than replayed,
// so a stalled goroutine does not burst frames.
func (r *Resampler) Due(now time.Duration) bool {
	if now+r.interval/4 < r.next {
		return false
	}

	r.next += r.interval
	if r.next <= now {
		r.next = now + r.interval
	}

	return true
}

// Tick returns the frame to watch now and whether it repeats the previous tick's frame. It returns
// nil until the first frame arrives.
func (r *Resampler) Tick() (s *frame.Slot, repeat bool) {
	if r.pending != nil {
		if r.current != nil {
			r.release(r.current)
		}

		r.current, r.pending = r.pending, nil
		r.stats.Ticks++
		r.stats.Fresh++

		return r.current, false
	}

	if r.current == nil {
		return nil, false
	}

	r.stats.Ticks++
	r.stats.Repeats++

	return r.current, true
}

// Stats returns the counters so far.
func (r *Resampler) Stats() Stats { return r.stats }

// Reset releases the frames held, for example when the stream stops or changes size.
func (r *Resampler) Reset() {
	if r.current != nil {
		r.release(r.current)
	}

	if r.pending != nil {
		r.release(r.pending)
	}

	r.current, r.pending = nil, nil
}
