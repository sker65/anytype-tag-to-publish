package watcher

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sker65/anypub/internal/app"
)

// Handler processes one task. Errors are retried with backoff.
type Handler func(ctx context.Context, t app.Task) error

// Queue serializes work per object id, runs different objects in parallel,
// debounces bursts and coalesces events: the latest desired state wins.
type Queue struct {
	handler    Handler
	log        *slog.Logger
	maxRetries int
	backoff    func(attempt int) time.Duration

	mu    sync.Mutex
	cond  *sync.Cond
	keys  map[string]*keyState
	ready []string
	done  bool
	wg    sync.WaitGroup
}

type keyState struct {
	pending *app.Task
	timer   *time.Timer
	queued  bool // in ready list
	running bool
	attempt int
	due     bool // timer fired while running
}

func NewQueue(h Handler, maxRetries int, log *slog.Logger) *Queue {
	q := &Queue{
		handler:    h,
		log:        log,
		maxRetries: maxRetries,
		keys:       map[string]*keyState{},
		backoff: func(attempt int) time.Duration {
			d := time.Second << attempt
			if d > time.Minute {
				d = time.Minute
			}
			return d
		},
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Start launches n workers; they stop when ctx ends.
func (q *Queue) Start(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		q.wg.Add(1)
		go q.worker(ctx)
	}
	go func() {
		<-ctx.Done()
		q.mu.Lock()
		q.done = true
		for _, ks := range q.keys {
			if ks.timer != nil {
				ks.timer.Stop()
			}
		}
		q.cond.Broadcast()
		q.mu.Unlock()
	}()
}

func (q *Queue) Wait() { q.wg.Wait() }

// Submit merges t into the pending task of its object and schedules it after delay.
func (q *Queue) Submit(t app.Task, delay time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.done {
		return
	}
	ks := q.keys[t.ObjectID]
	if ks == nil {
		ks = &keyState{}
		q.keys[t.ObjectID] = ks
	}
	if p := ks.pending; p != nil {
		if !p.Remove && !t.Remove {
			t.OnlySelf = p.OnlySelf && t.OnlySelf
		}
		if !p.Received.IsZero() && (t.Received.IsZero() || p.Received.Before(t.Received)) {
			t.Received = p.Received
		}
	}
	ks.pending = &t
	ks.attempt = 0
	q.schedule(t.ObjectID, ks, delay)
}

func (q *Queue) schedule(key string, ks *keyState, delay time.Duration) {
	if ks.timer != nil {
		ks.timer.Stop()
		ks.timer = nil
	}
	if delay <= 0 {
		q.markDue(key, ks)
		return
	}
	ks.timer = time.AfterFunc(delay, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		ks.timer = nil
		q.markDue(key, ks)
	})
}

// markDue must be called with mu held.
func (q *Queue) markDue(key string, ks *keyState) {
	if ks.running {
		ks.due = true
		return
	}
	if !ks.queued && ks.pending != nil {
		ks.queued = true
		q.ready = append(q.ready, key)
		q.cond.Signal()
	}
}

func (q *Queue) worker(ctx context.Context) {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		for len(q.ready) == 0 && !q.done {
			q.cond.Wait()
		}
		if q.done {
			q.mu.Unlock()
			return
		}
		key := q.ready[0]
		q.ready = q.ready[1:]
		ks := q.keys[key]
		ks.queued = false
		task := ks.pending
		ks.pending = nil
		ks.running = true
		attempt := ks.attempt
		q.mu.Unlock()

		if task == nil {
			q.finish(key, ks)
			continue
		}
		task.Final = attempt >= q.maxRetries-1
		err := q.handler(ctx, *task)

		q.mu.Lock()
		ks.running = false
		if err != nil && ctx.Err() == nil {
			if ks.pending == nil && attempt+1 < q.maxRetries {
				// retry unless a newer event superseded the task
				ks.pending = task
				ks.attempt = attempt + 1
				d := q.backoff(attempt)
				q.log.Warn("task failed, retrying", "object_id", key, "attempt", attempt+1, "in", d, "err", err)
				q.schedule(key, ks, d)
			} else if ks.pending == nil {
				q.log.Error("task failed, giving up until the next event", "object_id", key, "err", err)
			}
		}
		if ks.due {
			ks.due = false
			q.markDue(key, ks)
		}
		if ks.pending == nil && ks.timer == nil && !ks.queued {
			delete(q.keys, key)
		}
		q.mu.Unlock()
	}
}

func (q *Queue) finish(key string, ks *keyState) {
	q.mu.Lock()
	defer q.mu.Unlock()
	ks.running = false
	if ks.pending == nil && ks.timer == nil && !ks.queued {
		delete(q.keys, key)
	}
}
