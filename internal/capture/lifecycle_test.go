package capture

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errBegin = errors.New("device refused to start")

func TestEmitterNeverBlocksAndStopsAtClose(t *testing.T) {
	t.Parallel()
	e := NewEmitter()
	for range EventBuffer + 5 {
		e.Emit(EventOverflow, "dropped") // nobody reads: the extra ones are dropped
	}
	assert.Len(t, e.Events(), EventBuffer)
	ev := <-e.Events()
	assert.Equal(t, EventOverflow, ev.Kind)
	assert.Equal(t, "dropped", ev.Detail)
	assert.False(t, ev.At.IsZero())

	e.Close()
	e.Close() // idempotent
	e.Emit(EventStopped, "after close")
	n := 0
	for range e.Events() {
		n++
	}
	assert.Equal(t, EventBuffer-1, n, "the buffered events remain; nothing after Close")
}

func TestEmitterIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	e := NewEmitter()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				e.Emit(EventOverflow, "")
			}
		})
	}
	wg.Go(e.Close)
	wg.Wait()
	n := 0
	for range e.Events() {
		n++
	}
	assert.LessOrEqual(t, n, EventBuffer, "never more than the buffer holds")
}

func TestLifecycleRunsTheDrainUntilClose(t *testing.T) {
	t.Parallel()
	var l Lifecycle
	started := make(chan struct{})
	var stopped atomic.Bool
	require.NoError(t, l.Start(t.Context(), nil, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		stopped.Store(true)
	}))
	<-started
	require.ErrorIs(t, l.Start(t.Context(), nil, func(context.Context) {}), ErrStarted)

	order := []string{}
	assert.True(t, l.Close(func() { order = append(order, "halt") }))
	assert.True(t, stopped.Load(), "Close returns once the drain has returned")
	assert.Equal(t, []string{"halt"}, order)
	assert.False(t, l.Close(func() { t.Error("halt runs once") }), "a second Close does nothing")
	require.ErrorIs(t, l.Start(t.Context(), nil, func(context.Context) {}), ErrClosed)
}

func TestLifecycleHaltRunsWhileTheDrainStillRuns(t *testing.T) {
	t.Parallel()
	var l Lifecycle
	var running atomic.Bool
	running.Store(true)
	require.NoError(t, l.Start(t.Context(), nil, func(ctx context.Context) {
		<-ctx.Done()
		running.Store(false)
	}))
	l.Close(func() { assert.True(t, running.Load(), "the backend is silenced before its drain ends") })
	assert.False(t, running.Load())
}

func TestLifecycleThatFailsToBeginIsNotStarted(t *testing.T) {
	t.Parallel()
	var l Lifecycle
	require.ErrorIs(t, l.Start(t.Context(), func() error { return errBegin }, func(context.Context) {
		t.Error("no drain without a start")
	}), errBegin)
	ran := make(chan struct{})
	require.NoError(t, l.Start(t.Context(), func() error { return nil }, func(context.Context) { close(ran) }))
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never ran")
	}
	assert.True(t, l.Close(nil))
}

func TestLifecycleClosedBeforeStart(t *testing.T) {
	t.Parallel()
	var l Lifecycle
	assert.True(t, l.Close(nil), "a Source that never started still closes")
	require.ErrorIs(t, l.Start(t.Context(), nil, func(context.Context) {}), ErrClosed)
}

func TestLifecycleDrainStopsWithTheStartContext(t *testing.T) {
	t.Parallel()
	var l Lifecycle
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	require.NoError(t, l.Start(ctx, nil, func(dctx context.Context) {
		<-dctx.Done()
		close(done)
	}))
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain outlived its context")
	}
	assert.True(t, l.Close(nil))
}
