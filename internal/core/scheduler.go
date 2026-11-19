package core

import (
	"context"
	"time"
)

type Scheduler struct {
	Svc           *Service
	Tick          time.Duration // how often due posts are checked
	CollectEvery  time.Duration // how often metrics/comments are refreshed (0 = never)
	CollectWindow time.Duration // how far back posts are tracked
}

// Run blocks until ctx is cancelled.
func (sc *Scheduler) Run(ctx context.Context) {
	if sc.Tick <= 0 {
		sc.Tick = 30 * time.Second
	}
	if sc.CollectWindow <= 0 {
		sc.CollectWindow = 30 * 24 * time.Hour
	}
	// Posts stuck in "publishing" after a crash go back to the queue.
	if n, err := sc.Svc.Store.RecoverStuck(ctx, sc.Svc.now().Add(-10*time.Minute)); err == nil && n > 0 {
		sc.Svc.Log.Warn("recovered posts stuck in publishing", "count", n)
	}
	tick := time.NewTicker(sc.Tick)
	defer tick.Stop()
	var collect <-chan time.Time
	if sc.CollectEvery > 0 {
		ct := time.NewTicker(sc.CollectEvery)
		defer ct.Stop()
		collect = ct.C
	}
	sc.RunDue(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			sc.RunDue(ctx)
		case <-collect:
			sc.Svc.Collect(ctx, sc.CollectWindow)
		}
	}
}

// RunDue publishes every post whose time has come.
func (sc *Scheduler) RunDue(ctx context.Context) {
	posts, err := sc.Svc.Store.ClaimDue(ctx, sc.Svc.now(), 20)
	if err != nil {
		sc.Svc.Log.Error("claim due posts", "err", err)
		return
	}
	for _, p := range posts {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		sc.Svc.Publish(pctx, p)
		cancel()
	}
}
