package bridge

import "sync"

// EventQueue hands events from the platform's threads to the controller. Push never blocks, so a
// callback on the main thread or the capture thread returns immediately; one goroutine forwards
// the events, in order, to the channel the controller reads.
type EventQueue struct {
	mu      sync.Mutex
	pending []Event
	wake    chan struct{}
	out     chan Event
}

// NewEventQueue returns a queue and starts its forwarding goroutine.
func NewEventQueue() *EventQueue {
	q := &EventQueue{wake: make(chan struct{}, 1), out: make(chan Event)}
	go q.forward()

	return q
}

// Push queues an event.
func (q *EventQueue) Push(e Event) {
	q.mu.Lock()
	q.pending = append(q.pending, e)
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Events is the channel the controller reads.
func (q *EventQueue) Events() <-chan Event { return q.out }

func (q *EventQueue) forward() {
	for range q.wake {
		q.mu.Lock()
		batch := q.pending
		q.pending = nil
		q.mu.Unlock()

		for _, e := range batch {
			q.out <- e
		}
	}
}
