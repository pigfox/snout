package snout

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// writer buffers events and writes them in batches: when a batch fills, when
// the flush interval elapses, and once more on close. enqueue never blocks — a
// full buffer drops the event and counts it.
type writer struct {
	db           DB
	ch           chan Event
	batch        int
	interval     time.Duration
	flushTimeout time.Duration
	onError      func(error)

	dropped     *atomic.Uint64
	written     *atomic.Uint64
	flushErrors *atomic.Uint64

	done      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
}

func (w *writer) enqueue(e Event) {
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

func (w *writer) run() {
	defer close(w.stopped)
	buf := make([]Event, 0, w.batch)
	tick := time.NewTicker(w.interval)
	defer tick.Stop()

	for {
		select {
		case e := <-w.ch:
			buf = append(buf, e)
			if len(buf) >= w.batch {
				buf = w.flush(buf)
			}
		case <-tick.C:
			buf = w.flush(buf)
		case <-w.done:
			for {
				select {
				case e := <-w.ch:
					buf = append(buf, e)
					if len(buf) >= w.batch {
						buf = w.flush(buf)
					}
				default:
					w.flush(buf)
					return
				}
			}
		}
	}
}

// flush writes buf and returns it emptied. A failed batch is dropped and
// counted rather than retried: retrying would let a database outage grow the
// buffer without bound.
func (w *writer) flush(buf []Event) []Event {
	if len(buf) == 0 {
		return buf
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.flushTimeout)
	defer cancel()
	if err := insertEvents(ctx, w.db, buf); err != nil {
		w.flushErrors.Add(1)
		w.dropped.Add(uint64(len(buf)))
		w.onError(err)
	} else {
		w.written.Add(uint64(len(buf)))
	}
	return buf[:0]
}

// close stops the loop after writing everything still buffered, or returns
// ctx's error if that takes longer than ctx allows.
func (w *writer) close(ctx context.Context) error {
	w.closeOnce.Do(func() { close(w.done) })
	select {
	case <-w.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
