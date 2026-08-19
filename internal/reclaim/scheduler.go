package reclaim

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Sweeper runs reclamation on a schedule.
//
// Reclaiming after each deployment handles the steady state, but only for the
// project that deployed. A periodic sweep is what collects images belonging to
// projects that were deleted, dangling layers left by failed builds, and the
// build cache — none of which are attributable to any one deployment.
type Sweeper struct {
	Reclaimer *Reclaimer
	Interval  time.Duration
	// Delay staggers the first sweep past startup so a restart does not spend
	// its first seconds walking the image graph.
	Delay  time.Duration
	Logger *slog.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (s *Sweeper) Start(parent context.Context) {
	if s.Reclaimer == nil || s.Interval <= 0 {
		return
	}
	if s.Delay <= 0 {
		s.Delay = 5 * time.Minute
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(s.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.sweep(ctx)
		ticker := time.NewTicker(s.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweep(ctx)
			}
		}
	}()
}

func (s *Sweeper) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *Sweeper) sweep(ctx context.Context) {
	result, err := s.Reclaimer.Run(ctx)
	if err != nil {
		s.Logger.Warn("storage reclamation failed", "error", err)
		return
	}
	if result.FreedBytes == 0 && result.ImagesRemoved == 0 && result.BuildCacheBytes == 0 {
		s.Logger.Debug("storage reclamation found nothing to free")
		return
	}
	s.Logger.Info("storage reclaimed", "summary", result.Summary(), "freed_bytes", result.FreedBytes, "images", result.ImagesRemoved)
}
