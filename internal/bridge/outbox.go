package bridge

import (
	"sync"
	"time"

	"github.com/brian-nunez/psvguard/internal/coverage"
)

// Sender delivers one encoded command to the platform layer. It must copy payload and return
// without waiting for the command to be applied.
type Sender func(kind CommandKind, payload []byte)

// Outbox is the one place commands leave Go. A single goroutine (Run) owns every call to the
// Sender, so commands are ordered and come from one place. It coalesces: only the latest pending
// cover command per display, the latest status and the latest stats are sent; covers are sent
// only when they change; status and stats are throttled.
type Outbox struct {
	send  Sender
	trace func(Command)

	// StatusGap is the least time between two status updates, and StatsGap between two stats
	// updates.
	StatusGap, StatsGap time.Duration

	mu         sync.Mutex
	queue      []Command
	covers     map[string]Command
	coverOrder []string
	status     *SetStatus
	stats      *SetStats
	sentCovers map[string][]Rect
	lastStatus time.Time
	lastStats  time.Time
	wake       chan struct{}
}

// NewOutbox returns an outbox that delivers through send. trace, when not nil, sees every command
// sent.
func NewOutbox(send Sender, trace func(Command)) *Outbox {
	return &Outbox{
		send:       send,
		trace:      trace,
		StatusGap:  250 * time.Millisecond,
		StatsGap:   time.Second,
		covers:     map[string]Command{},
		sentCovers: map[string][]Rect{},
		wake:       make(chan struct{}, 1),
	}
}

// Post queues a command and returns at once. It is safe from any goroutine.
func (o *Outbox) Post(c Command) {
	o.mu.Lock()

	switch c := c.(type) {
	case SetCovers:
		o.putCovers(c.Display, c)
	case ClearCovers:
		if c.Display == "" {
			o.covers, o.coverOrder = map[string]Command{}, nil
			o.queue = append(o.queue, c)
		} else {
			o.putCovers(c.Display, c)
		}
	case SetStatus:
		o.status = &c
	case SetStats:
		o.stats = &c
	default:
		o.queue = append(o.queue, c)
	}

	o.mu.Unlock()

	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *Outbox) putCovers(display string, c Command) {
	if _, ok := o.covers[display]; !ok {
		o.coverOrder = append(o.coverOrder, display)
	}

	o.covers[display] = c
}

// Run delivers commands until done is closed, then delivers what is still pending.
func (o *Outbox) Run(done <-chan struct{}) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		select {
		case <-o.wake:
		case <-timer.C:
		case <-done:
			o.Drain(time.Now().Add(time.Hour))

			return
		}

		if next := o.Drain(time.Now()); !next.IsZero() {
			timer.Reset(time.Until(next))
		}
	}
}

// Drain sends everything that is due at now and returns when throttled updates are next due, or
// the zero time.
func (o *Outbox) Drain(now time.Time) time.Time {
	o.mu.Lock()
	queue := o.queue
	o.queue = nil

	covers := make([]Command, 0, len(o.coverOrder))
	for _, d := range o.coverOrder {
		covers = append(covers, o.covers[d])
	}

	o.covers, o.coverOrder = map[string]Command{}, nil

	var (
		status *SetStatus
		stats  *SetStats
		next   time.Time
	)

	if o.status != nil {
		if due := o.lastStatus.Add(o.StatusGap); !now.Before(due) {
			status, o.status, o.lastStatus = o.status, nil, now
		} else {
			next = due
		}
	}

	if o.stats != nil {
		if due := o.lastStats.Add(o.StatsGap); !now.Before(due) {
			stats, o.stats, o.lastStats = o.stats, nil, now
		} else if next.IsZero() || due.Before(next) {
			next = due
		}
	}

	o.mu.Unlock()

	for _, c := range covers {
		o.deliverCovers(c)
	}

	for _, c := range queue {
		if cc, ok := c.(ClearCovers); ok && cc.Display == "" {
			o.sentCovers = map[string][]Rect{}
		}

		o.deliver(c)
	}

	if status != nil {
		o.deliver(*status)
	}

	if stats != nil {
		o.deliver(*stats)
	}

	return next
}

// deliverCovers sends a cover command unless the display already shows exactly that.
func (o *Outbox) deliverCovers(c Command) {
	var (
		display string
		rects   []Rect
	)

	switch c := c.(type) {
	case SetCovers:
		display, rects = c.Display, c.Rects
	case ClearCovers:
		display = c.Display
	}

	if prev, ok := o.sentCovers[display]; ok && coverage.Equal(prev, rects) {
		return
	}

	if len(rects) == 0 {
		if _, shown := o.sentCovers[display]; !shown {
			o.sentCovers[display] = nil

			return
		}

		c = ClearCovers{Display: display}
	}

	o.sentCovers[display] = append([]Rect(nil), rects...)
	o.deliver(c)
}

func (o *Outbox) deliver(c Command) {
	b, err := EncodeCommand(c)
	if err != nil {
		return
	}

	if o.trace != nil {
		o.trace(c)
	}

	o.send(c.CommandKind(), b)
}
