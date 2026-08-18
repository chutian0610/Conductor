package runmgr

import (
	"context"
	"encoding/json"
	"sync"

	"conductor/server/internal/agentregistry"
)

// runState owns the per-run state: a tracked event list kept in
// memory for fast SSE tail, a list of subscribers with buffered
// channels, and a `done` chan signalled when the run finishes.
//
// runState is goroutine-safe. We use a single sync.Mutex because
// the per-operation work is tiny (push an event, register a
// subscriber) and a Mutex is plenty fast.
type runState struct {
	runID int64

	mu          sync.Mutex
	events      []*agentregistry.Event // copied (not aliasing registry) for fast SSE tail.
	subscribers []chan []byte          // SSE wire bytes: each chan carries formatted events.
	finished    bool
	done        chan struct{} // closed on finish.
}

func newRunState(runID int64) *runState {
	return &runState{runID: runID, done: make(chan struct{})}
}

// appendEvent records one event into the in-memory list. Only the
// run goroutine calls this — no contention against readers in
// steady state. We hold mu briefly so Subscribe() can scan the
// list safely when the run is fast.
func (rs *runState) appendEvent(ctx context.Context, store *agentregistry.Store, ev *agentregistry.Event) error {
	if err := store.AppendEvent(ctx, rs.runID, ev.Kind, ev.Payload); err != nil {
		return err
	}
	rs.mu.Lock()
	rs.events = append(rs.events, ev)
	wire := formatSSEEvent(ev)
	for _, ch := range rs.subscribers {
		select {
		case ch <- wire:
		default:
			// drop slow consumers — buffer full. ADR §5 mandates
			// "no buffering"; we pick the live path over back-pressure.
		}
	}
	rs.mu.Unlock()
	return nil
}

// markFinished closes all subscriber channels after delivering the
// terminal event (already in rs.events). Idempotent.
func (rs *runState) markFinished() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.finished {
		return
	}
	rs.finished = true
	for _, ch := range rs.subscribers {
		close(ch)
	}
	rs.subscribers = nil
	close(rs.done)
}

// registerSubscriber adds a new SSE subscriber with the given
// buffer size. Returns the channel, the count of events already
// stored at subscription time (so the caller can replay them
// explicitly), and an unregister func.
//
// IMPORTANT: registerSubscriber itself does NOT replay history;
// the caller is responsible for streaming any required events to
// the channel BEFORE calling publish on live events. This split
// lets the caller honour `Last-Event-Id` headers cleanly.
func (rs *runState) registerSubscriber(bufSize int) (chan []byte, int) {
	ch := make(chan []byte, bufSize)
	rs.mu.Lock()
	defer rs.mu.Unlock()
	count := len(rs.events)
	if rs.finished {
		close(ch)
		return ch, count
	}
	rs.subscribers = append(rs.subscribers, ch)
	return ch, count
}

// Done returns a chan closed when the run finishes. Polling
// clients use this for graceful shutdown.
func (rs *runState) Done() <-chan struct{} { return rs.done }

// Finished reports whether the run has finished. Captured under
// the lock so a Subscribe call right at the finish boundary does
// not race.
func (rs *runState) Finished() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.finished
}

// EventCount returns how many events are currently buffered in
// memory (mirrors the registry; resynced on Subscribe).
func (rs *runState) EventCount() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.events)
}

// formatSSEEvent renders one event as SSE wire bytes:
//
//	id: <seq>

//	event: <kind>

//	data: <json payload>

//

// Lines are SSE-spec compliant; CRLF is deliberately not used
// (Go's net/http fprintln already uses LF; SSE-over-HTTP proxies
// handle either on a properly-merged flush).
func formatSSEEvent(ev *agentregistry.Event) []byte {
	return []byte("id: " + uintToString(uint64(ev.Seq)) + "\nevent: " + ev.Kind + "\ndata: " + string(ev.Payload) + "\n\n")
}

// uintToString avoids pulling strconv into the hot path for
// monotonic seq numbers; small seqs are the common case.
func uintToString(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// keep json imported even if future formats skip it
var _ = json.RawMessage{}
