package capture

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Errors every backend reports for misuse.
var (
	// ErrStarted reports a second Start on the same Source.
	ErrStarted = errors.New("capture already started")
	// ErrClosed reports use of a closed Source.
	ErrClosed = errors.New("capture source closed")
)

// Timing shared by the backends.
const (
	// DrainInterval is how often a backend's drain goroutine empties the
	// ring its real-time callback fills.
	DrainInterval = 10 * time.Millisecond
	// EventBuffer is the capacity of a Source's Events channel.
	EventBuffer = 16
)

// Emitter delivers a Source's events. Emit never blocks the backend: an
// event nobody has room for is dropped. Close closes the channel; events
// emitted after it are dropped too. It is safe for concurrent use.
type Emitter struct {
	mu     sync.Mutex
	ch     chan Event
	closed bool
}

// NewEmitter returns an Emitter with room for EventBuffer events.
func NewEmitter() *Emitter { return &Emitter{ch: make(chan Event, EventBuffer)} }

// Events is the channel Source.Events returns.
func (e *Emitter) Events() <-chan Event { return e.ch }

// Emit sends an event stamped now.
func (e *Emitter) Emit(kind EventKind, detail string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	select {
	case e.ch <- Event{Kind: kind, Detail: detail, At: time.Now()}:
	default:
	}
}

// Close closes the events channel. It is idempotent.
func (e *Emitter) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.closed = true
		close(e.ch)
	}
}

// Lifecycle runs a Source's drain goroutine: Start starts it once, Close
// stops it and waits for it. The zero value is ready to use.
type Lifecycle struct {
	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// Start runs begin (nil to skip), then drain on a new goroutine with a
// context that Close cancels. A Source that is closed, or started already,
// fails with ErrClosed or ErrStarted; one whose begin fails is not started.
func (l *Lifecycle) Start(ctx context.Context, begin func() error, drain func(context.Context)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.closed:
		return ErrClosed
	case l.started:
		return ErrStarted
	}
	if begin != nil {
		if err := begin(); err != nil {
			return err
		}
	}
	l.started = true
	dctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	l.cancel, l.done = cancel, done
	go func() {
		defer close(done)
		drain(dctx)
	}()
	return nil
}

// Close marks the Source closed and stops its drain: it runs halt (nil to
// skip) to silence the backend while the drain still runs, then cancels
// the drain and waits for it to return. It reports whether this call
// closed the Source; later calls do nothing.
func (l *Lifecycle) Close(halt func()) bool {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return false
	}
	l.closed = true
	cancel, done := l.cancel, l.done
	l.mu.Unlock()
	if halt != nil {
		halt()
	}
	if cancel != nil {
		cancel()
		<-done
	}
	return true
}
