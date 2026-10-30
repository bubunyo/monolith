package monolith

import (
	"io"
	"sync"
)

// consoleRingSize is the maximum number of bytes of recent console output
// retained for replay to newly-attached clients. 64 KiB is enough to capture
// the Alpine kernel + init log up to the login prompt.
const consoleRingSize = 64 * 1024

// consoleBus drains a single PTY slave continuously and broadcasts the bytes
// to a single attached subscriber. It also retains a ring buffer of recent
// output so that a newly-attached client can replay the boot log.
//
// The bus is intentionally simple: only one subscriber at a time (matching
// the daemon's single-console-client invariant). New subscribers atomically
// receive a snapshot of the ring buffer plus a channel of subsequent live
// chunks, with no gap between the two.
type consoleBus struct {
	mu     sync.Mutex
	ring   []byte // recent output, capped at consoleRingSize
	sub    chan []byte
	closed bool
}

func newConsoleBus() *consoleBus {
	return &consoleBus{
		ring: make([]byte, 0, consoleRingSize),
	}
}

// drain reads from r forever, appending bytes into the ring buffer and
// fanning them out to the current subscriber (if any). It returns when r
// returns an error (typically EOF after the PTY closes).
func (b *consoleBus) drain(r io.Reader, logger *Logger) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.publish(buf[:n])
		}
		if err != nil {
			b.mu.Lock()
			b.closed = true
			if b.sub != nil {
				close(b.sub)
				b.sub = nil
			}
			b.mu.Unlock()
			if logger != nil {
				logger.Info("console bus drain ended: %v", err)
			}
			return
		}
	}
}

// publish appends chunk to the ring buffer (truncating from the head if it
// would exceed consoleRingSize) and forwards a copy to the current
// subscriber, if any.
func (b *consoleBus) publish(chunk []byte) {
	// Copy chunk because the caller's buffer is reused.
	cp := make([]byte, len(chunk))
	copy(cp, chunk)

	b.mu.Lock()
	defer b.mu.Unlock()

	// Append to ring; trim from head if oversize.
	b.ring = append(b.ring, cp...)
	if len(b.ring) > consoleRingSize {
		excess := len(b.ring) - consoleRingSize
		b.ring = b.ring[excess:]
	}

	// Fan out to subscriber. Non-blocking: if the subscriber is slow, we
	// drop the live chunk for them — they can still recover from the ring
	// buffer on next attach. In practice the buffered channel + a single
	// fast TCP/Unix-socket consumer means we almost never drop.
	if b.sub != nil {
		select {
		case b.sub <- cp:
		default:
			// Subscriber is backed up; skip.
		}
	}
}

// subscribe returns a snapshot of the ring buffer, a channel that will
// receive subsequent live chunks, and a function to cancel the subscription.
// If a subscriber is already registered, the previous one is evicted (its
// channel is closed) — matching the single-console-client invariant.
func (b *consoleBus) subscribe() (replay []byte, live <-chan []byte, cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Snapshot the ring.
	replay = make([]byte, len(b.ring))
	copy(replay, b.ring)

	if b.closed {
		ch := make(chan []byte)
		close(ch)
		return replay, ch, func() {}
	}

	// Evict any prior subscriber.
	if b.sub != nil {
		close(b.sub)
	}

	ch := make(chan []byte, 64)
	b.sub = ch

	cancel = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.sub == ch {
			close(b.sub)
			b.sub = nil
		}
	}

	return replay, ch, cancel
}
