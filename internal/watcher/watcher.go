// Package watcher keeps one SSE search stream per configured space open and
// turns its events into tasks. The opening snapshot of every (re)connect is
// the reconcile: objects in it are ensured, published objects missing from
// it are unpublished.
package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sker65/anypub/internal/anyapi"
	"github.com/sker65/anypub/internal/app"
	"github.com/sker65/anypub/internal/obs"
)

type Streamer interface {
	Stream(ctx context.Context, spaceID string, sr anyapi.SearchRequest, heartbeat time.Duration, fn func(anyapi.StreamEvent) error) error
	OptionExists(ctx context.Context, spaceID, key, name string) (bool, error)
}

// Space is a configured space with everything resolved at startup.
type Space struct {
	ID       string
	Filter   string
	Fields   []string
	TagKey   string // property key of the tag relation
	TagName  string // the option the filter names
	Bot      anyapi.Member
	ModByKey string // property key of last_modified_by
}

type Config struct {
	Heartbeat       time.Duration
	Debounce        time.Duration
	SafetyReconcile time.Duration
	MinBackoff      time.Duration
	MaxBackoff      time.Duration
	// OptionPoll is how often a space whose tag option does not exist yet is
	// checked again.
	OptionPoll time.Duration
}

type Watcher struct {
	api     Streamer
	queue   *Queue
	proc    *app.Processor
	cfg     Config
	metrics *obs.Metrics
	log     *slog.Logger

	mu    sync.Mutex
	ready map[string]bool
	kicks map[string]context.CancelFunc
}

func New(api Streamer, q *Queue, proc *app.Processor, cfg Config, m *obs.Metrics, log *slog.Logger) *Watcher {
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.OptionPoll <= 0 {
		cfg.OptionPoll = 30 * time.Second
	}
	return &Watcher{api: api, queue: q, proc: proc, cfg: cfg, metrics: m, log: log,
		ready: map[string]bool{}, kicks: map[string]context.CancelFunc{}}
}

// TagFilter builds the stream filter for a tag name.
func TagFilter(tagProperty, tag string) string {
	return fmt.Sprintf(`%s IN ("%s")`, tagProperty, strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(tag))
}

// Ready reports whether every space has a live stream past its snapshot.
func (w *Watcher) Ready(spaces []Space) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range spaces {
		if !w.ready[s.ID] {
			return false
		}
	}
	return len(spaces) > 0
}

// Run blocks until ctx ends.
func (w *Watcher) Run(ctx context.Context, spaces []Space) {
	var wg sync.WaitGroup
	for _, s := range spaces {
		wg.Add(1)
		go func(s Space) {
			defer wg.Done()
			w.runSpace(ctx, s)
		}(s)
	}
	if w.cfg.SafetyReconcile > 0 {
		go w.safetyLoop(ctx)
	}
	wg.Wait()
}

// safetyLoop periodically forces a reconnect: the fresh snapshot is a full
// reconcile that catches anything a lost event might have missed.
func (w *Watcher) safetyLoop(ctx context.Context) {
	t := time.NewTicker(w.cfg.SafetyReconcile)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.mu.Lock()
			for id, cancel := range w.kicks {
				w.log.Info("safety reconcile", "space", id)
				cancel()
			}
			w.mu.Unlock()
		}
	}
}

func (w *Watcher) setReady(space string, v bool) {
	w.mu.Lock()
	w.ready[space] = v
	w.mu.Unlock()
}

func (w *Watcher) runSpace(ctx context.Context, s Space) {
	backoff := w.cfg.MinBackoff
	waiting := false // the tag option does not exist yet
	for ctx.Err() == nil {
		sctx, cancel := context.WithCancel(ctx)
		w.mu.Lock()
		w.kicks[s.ID] = cancel
		w.mu.Unlock()

		gotSnapshot := false
		err := w.stream(sctx, s, &gotSnapshot)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if !gotSnapshot && anyapi.IsValidation(err) && s.TagKey != "" {
			if exists, oerr := w.api.OptionExists(ctx, s.ID, s.TagKey, s.TagName); oerr == nil && !exists {
				if !waiting {
					// nothing can carry an option that does not exist
					if rerr := w.reconcile(ctx, s, nil, time.Now()); rerr != nil {
						w.log.Warn("reconcile failed", "space", s.ID, "err", rerr)
					} else {
						waiting = true
						w.setReady(s.ID, true)
						w.log.Info("tag option does not exist yet, nothing to publish; checking again periodically",
							"space", s.ID, "property", s.TagKey, "option", s.TagName, "every", w.cfg.OptionPoll.String())
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(w.cfg.OptionPoll):
				}
				continue
			}
		}
		waiting = false
		w.setReady(s.ID, false)
		kicked := sctx.Err() != nil
		if gotSnapshot {
			backoff = w.cfg.MinBackoff
		}
		if kicked {
			continue
		}
		w.metrics.StreamReconnects.Inc()
		w.log.Warn("stream ended, reconnecting", "space", s.ID, "in", backoff, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > w.cfg.MaxBackoff {
			backoff = w.cfg.MaxBackoff
		}
	}
}

func (w *Watcher) stream(ctx context.Context, s Space, gotSnapshot *bool) error {
	snapshot := map[string]anyapi.Row{}
	inSnapshot := true
	req := anyapi.SearchRequest{Filter: s.Filter, Fields: s.Fields}
	return w.api.Stream(ctx, s.ID, req, w.cfg.Heartbeat, func(ev anyapi.StreamEvent) error {
		w.metrics.Events.WithLabelValues(ev.Type).Inc()
		now := time.Now()
		switch ev.Type {
		case anyapi.EventObjectAdded, anyapi.EventObjectUpdated:
			if ev.Object == nil || ev.Object.ID == "" {
				return nil
			}
			row := *ev.Object
			if inSnapshot {
				snapshot[row.ID] = row
				return nil
			}
			delay := time.Duration(0)
			if ev.Type == anyapi.EventObjectUpdated {
				delay = w.cfg.Debounce
			}
			w.queue.Submit(app.Task{
				SpaceID:  s.ID,
				ObjectID: row.ID,
				Row:      row,
				OnlySelf: ev.Type == anyapi.EventObjectUpdated && s.Bot.Is(row.Prop(s.ModByKey)),
				Received: now,
			}, delay)
		case anyapi.EventObjectRemoved:
			if inSnapshot {
				delete(snapshot, ev.ObjectID)
				return nil
			}
			w.queue.Submit(app.Task{SpaceID: s.ID, ObjectID: ev.ObjectID, Remove: true, Received: now}, 0)
		case anyapi.EventSnapshotComplete:
			inSnapshot = false
			*gotSnapshot = true
			for _, warn := range ev.Warnings {
				w.log.Warn("search warning", "space", s.ID, "warning", string(warn))
			}
			if err := w.reconcile(ctx, s, snapshot, now); err != nil {
				return err
			}
			w.setReady(s.ID, true)
			w.log.Info("stream live", "space", s.ID, "tagged", len(snapshot))
		}
		return nil
	})
}

func (w *Watcher) reconcile(ctx context.Context, s Space, snapshot map[string]anyapi.Row, now time.Time) error {
	published, err := w.proc.ActiveIDs(ctx, s.ID)
	if err != nil {
		return err
	}
	for id := range published {
		if _, ok := snapshot[id]; !ok {
			w.queue.Submit(app.Task{SpaceID: s.ID, ObjectID: id, Remove: true, Received: now}, 0)
		}
	}
	for id, row := range snapshot {
		w.queue.Submit(app.Task{SpaceID: s.ID, ObjectID: id, Row: row, Received: now}, 0)
	}
	return nil
}
