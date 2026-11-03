package cli

import (
	"os"
	"sync"
)

// watchSignals calls onSignal, on a goroutine of its own, for each SIGINT
// or SIGTERM until the returned stop func is called (which is safe to call
// more than once). While a session sets up, two watchers run at once: the
// setup guard and the session's own handler. Every signal reaches both, so
// none is lost in the handover between them.
func (a *app) watchSignals(onSignal func(os.Signal)) (stop func()) {
	sigs, stopSigs := a.deps.services.signals()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case sig := <-sigs:
				onSignal(sig)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			stopSigs()
		})
	}
}
