package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/jack-work/figaro/api/rpc"
)

// Turn activity, for the Telegram typing indicator.
//
// There is no "turn started" push on the wire: the only three are
// figaro.aria, turn.done and form.delta. But the daemon emits a figaro.aria
// frame carrying the inquiry back before any provider call, and a watcher
// sees it almost at once. Measured on a real turn:
//
//	+0.1ms     submit returns
//	+34.5ms    figaro.aria, turn opens
//	+2263.6ms  figaro.aria, first content
//	+2324.9ms  turn.done
//
// So the running state is available 2.2 seconds before anything is printed,
// and the 35ms is a durable write plus tool-call cleanup, not a model call.
// Typing on at the first frame, off at turn.done.
//
// (The CLI's own spinner is late for an unrelated reason: armThinking returns
// early while the pager is up, so in transcript mode it waits for first
// output. A rendering choice, not a protocol limit.)

// activity tracks whether an aria is mid-turn.
type activity struct {
	mu      sync.Mutex
	running bool
	changed chan struct{}
}

func newActivity() *activity {
	return &activity{changed: make(chan struct{}, 1)}
}

func (a *activity) set(running bool) {
	a.mu.Lock()
	was := a.running
	a.running = running
	a.mu.Unlock()
	if was != running {
		select {
		case a.changed <- struct{}{}:
		default:
		}
	}
}

func (a *activity) get() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// notify translates daemon pushes into activity transitions.
func (a *activity) notify(method string, params json.RawMessage) {
	switch method {
	case rpc.MethodAriaFrame:
		// The inquiry echo arrives before the provider is called, so this is
		// the earliest point at which "it is working" is true.
		a.set(true)
	case rpc.MethodTurnDone:
		a.set(false)
	}
}

// TypingSignal is what the bridge needs from the transport to keep a Telegram
// typing indicator alive: Telegram's own lapses after a few seconds, so it has
// to be refreshed while the turn runs.
const TypingRefresh = 4 * time.Second

// holdTyping keeps the indicator lit for as long as the aria is working.
//
// send is called to refresh it; Telegram clears the indicator by itself once
// refreshes stop, so there is nothing to clear on the way out.
func holdTyping(ctx context.Context, a *activity, send func(context.Context) error) {
	ticker := time.NewTicker(TypingRefresh)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.changed:
		case <-ticker.C:
		}
		if !a.get() {
			continue
		}
		if err := send(ctx); err != nil {
			log.Printf("typing indicator: %v", err)
		}
	}
}
