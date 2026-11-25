package pool

import (
	"context"
	"errors"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/nodecap"
)

type startRetry struct {
	reason error
	at     time.Time
}

func (r *startRetry) Error() string { return r.reason.Error() }
func (r *startRetry) Unwrap() error { return r.reason }

type startFlight struct {
	done   chan struct{}
	sample nodecap.PacedStartSample
	err    error
}
type startProbe struct {
	flight       *startFlight
	retry        *startRetry
	readFailures int
}

// One cold flight per node, shared by all cold workers/scale sets. Warm
// requests have their own coalesced flight so one slow cold read cannot
// queue warm I/O behind it. Neither lane owns the POST lock during I/O.
func (b *WindowsBootBudget) sampleStart(ctx context.Context, node string, warm bool, read func(context.Context, string) (nodecap.PacedStartSample, error)) (nodecap.PacedStartSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n := b.node(node)
	n.probeMu.Lock()
	p := &n.coldProbe
	if warm {
		p = &n.warmProbe
	}
	if p.retry != nil && time.Now().Before(p.retry.at) {
		err := p.retry
		n.probeMu.Unlock()
		return nil, err
	}
	flight := p.flight
	if flight == nil {
		priorRetry := p.retry
		flight = &startFlight{done: make(chan struct{})}
		p.flight = flight
		// A worker's cancellation must not abort a shared read or drop peers
		// out of their cold queue. The read itself is bounded independently.
		go func() {
			readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			sample, err := read(readCtx, node)
			n.probeMu.Lock()
			if err != nil {
				p.readFailures = min(p.readFailures+1, 4)
				delay := min(time.Second*time.Duration(1<<(p.readFailures-1)), 5*time.Second)
				p.retry = &startRetry{reason: errors.Join(nodecap.ErrPacedStartRead, err), at: time.Now().Add(delay)}
				err = p.retry
			} else {
				p.readFailures = 0
				// Do not erase a newer capacity/slot cooldown installed while
				// this read was in flight by another prepared cohort.
				if p.retry == priorRetry {
					p.retry = nil
				}
			}
			flight.sample, flight.err = sample, err
			p.flight = nil
			close(flight.done)
			n.probeMu.Unlock()
		}()
	}
	n.probeMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		return flight.sample, flight.err
	}
}

func (b *WindowsBootBudget) deferStart(node string, warm bool, reason error) error {
	if reason == nil {
		reason = nodecap.ErrPacedStartCapacity
	}
	n := b.node(node)
	n.probeMu.Lock()
	defer n.probeMu.Unlock()
	p := &n.coldProbe
	if warm {
		p = &n.warmProbe
	}
	retry := &startRetry{reason: reason, at: time.Now().Add(time.Second)}
	p.retry = retry
	// A saturated start ledger blocks both lanes, unlike a cold shape's
	// memory refusal. Permit another direct settlement probe after one
	// second; exact cleanup/observation can still release the ledger normally.
	if errors.Is(reason, nodecap.ErrPacedStartSlots) {
		n.coldProbe.retry = retry
		n.warmProbe.retry = retry
	}
	return retry
}
