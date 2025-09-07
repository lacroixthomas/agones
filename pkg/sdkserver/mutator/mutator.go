package mutator

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	RetryErr = errors.New("errored will retry")
)

type Action[T any] interface {
	Mutate(context.Context, *T) error
	Finalize(context.Context, *T) error
	ErrCh() chan error
}

type FlushFn[T any] func(ctx context.Context, m *Mutator[T], actions []Action[T])

type Mutator[T any] struct {
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	pending  []Action[T]
	quiet    time.Duration
	maxWait  time.Duration
	flushFn  FlushFn[T]
	flushReq chan struct{}
}

func NewMutator[T any](quiet, maxWait time.Duration, flushFn FlushFn[T]) *Mutator[T] {
	m := &Mutator[T]{
		quiet:    quiet,
		maxWait:  maxWait,
		flushFn:  flushFn,
		flushReq: make(chan struct{}, 1),
	}

	return m
}

func (m *Mutator[T]) Push(action ...Action[T]) {
	m.mu.Lock()
	m.pending = append(m.pending, action...)
	m.mu.Unlock()

	select {
	case m.flushReq <- struct{}{}:
	default:
	}
}

func (m *Mutator[T]) Stop() {
	m.cancel()
}

func (m *Mutator[T]) Run(parentCtx context.Context) {
	ctx, cancel := context.WithCancel(parentCtx)
	m.ctx = ctx
	m.cancel = cancel

	var quietTimer *time.Timer
	var maxWaitTimer *time.Timer
	timersStarted := false

	stopAndNilTimers := func() {
		if quietTimer != nil {
			quietTimer.Stop()
			quietTimer = nil
		}
		if maxWaitTimer != nil {
			maxWaitTimer.Stop()
			maxWaitTimer = nil
		}
		timersStarted = false
	}

	resetTimers := func() {
		if quietTimer != nil {
			if !quietTimer.Stop() {
				select {
				case <-quietTimer.C:
				default:
				}
			}
			quietTimer.Reset(m.quiet)
		}
		if maxWaitTimer != nil {
			if !maxWaitTimer.Stop() {
				select {
				case <-maxWaitTimer.C:
				default:
				}
			}
			maxWaitTimer.Reset(m.maxWait)
		}
	}

	for {
		select {
		case <-m.flushReq:
			m.mu.Lock()
			hasPending := len(m.pending) > 0
			m.mu.Unlock()
			if !hasPending {
				stopAndNilTimers()
				continue
			}
			// Only start or reset timers if there are pending actions
			if !timersStarted {
				quietTimer = time.NewTimer(m.quiet)
				maxWaitTimer = time.NewTimer(m.maxWait)
				timersStarted = true
			} else {
				resetTimers()
			}
		case <-func() <-chan time.Time {
			if quietTimer != nil {
				return quietTimer.C
			}
			return make(chan time.Time)
		}():
			m.flushBatch()
			m.mu.Lock()
			hasPending := len(m.pending) > 0
			m.mu.Unlock()
			if hasPending {
				quietTimer.Reset(m.quiet)
			} else {
				stopAndNilTimers()
			}
		case <-func() <-chan time.Time {
			if maxWaitTimer != nil {
				return maxWaitTimer.C
			}
			return make(chan time.Time)
		}():
			m.flushBatch()
			m.mu.Lock()
			hasPending := len(m.pending) > 0
			m.mu.Unlock()
			if hasPending {
				maxWaitTimer.Reset(m.maxWait)
			} else {
				stopAndNilTimers()
			}
		case <-m.ctx.Done():
			m.flushBatch()
			stopAndNilTimers()
			return
		}
	}
}

func (m *Mutator[T]) flushBatch() {
	m.mu.Lock()
	actions := m.pending
	m.pending = nil
	m.mu.Unlock()

	if len(actions) > 0 {
		m.flushFn(m.ctx, m, actions)
	}
}
