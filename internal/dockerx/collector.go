package dockerx

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rousoftware/asgard/internal/store"
)

type Collector struct {
	Engine   *Engine
	Store    *store.Store
	Interval time.Duration
	logger   *slog.Logger
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func (c *Collector) Start(parent context.Context) {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.collect(ctx)
		ticker := time.NewTicker(c.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.collect(ctx)
			}
		}
	}()
}
func (c *Collector) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
}

func (c *Collector) collect(ctx context.Context) {
	items, err := c.Engine.Containers(ctx, true)
	if err != nil {
		c.logger.Debug("metrics inventory unavailable", "error", err)
		return
	}
	for _, item := range items {
		if !item.Managed || item.ServiceID == "" {
			continue
		}
		_ = c.Store.UpdateRuntimeState(ctx, item.ID, item.State)
		if item.State != "running" {
			continue
		}
		stats, err := c.Engine.Stats(ctx, item.ID)
		if err != nil {
			c.logger.Debug("container metrics unavailable", "container", item.Name, "error", err)
			continue
		}
		stats.ThrottledPercent = c.intervalThrottling(ctx, item.ID, stats)
		_, err = c.Store.DB.ExecContext(ctx, `INSERT INTO metrics(service_id,container_id,cpu_percent,memory_bytes,memory_limit,network_rx,network_tx,block_read,block_write,pids,cpu_periods,cpu_throttled_periods,cpu_throttled_nanos,cpu_throttled_percent,collected_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.ServiceID, item.ID, stats.CPUPercent, stats.MemoryBytes, stats.MemoryLimit, stats.NetworkRX, stats.NetworkTX, stats.BlockRead, stats.BlockWrite, stats.PIDs, stats.CPUPeriods, stats.CPUThrottledPeriods, stats.CPUThrottledNanos, stats.ThrottledPercent, stats.CollectedAt.Format(time.RFC3339Nano))
		if err != nil {
			c.logger.Warn("store metrics", "error", err)
		}
	}
	_, _ = c.Store.DB.ExecContext(ctx, `DELETE FROM metrics WHERE collected_at < ?`, time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339Nano))
}

// intervalThrottling converts the container's cumulative throttling counters
// into the share of scheduling periods throttled since the previous sample.
//
// The cumulative ratio is the wrong number to show: a container that spent its
// first hour saturated and has been idle since still reports that hour forever,
// and one that only started struggling ten minutes ago has it diluted by every
// quiet period before. Differencing against the last stored sample gives the
// share over one collection interval, which is what an operator looking at a
// slow service is actually asking about.
//
// A restart resets the kernel's counters, so a negative delta means the
// container is new and the counters it carries are its whole life so far.
func (c *Collector) intervalThrottling(ctx context.Context, containerID string, stats Stats) float64 {
	var periods, throttled int64
	err := c.Store.DB.QueryRowContext(ctx, `SELECT cpu_periods,cpu_throttled_periods FROM metrics WHERE container_id=? ORDER BY collected_at DESC LIMIT 1`, containerID).Scan(&periods, &throttled)
	if err != nil {
		return stats.ThrottledPercent
	}
	deltaPeriods, deltaThrottled := stats.CPUPeriods-periods, stats.CPUThrottledPeriods-throttled
	if deltaPeriods < 0 || deltaThrottled < 0 {
		return stats.ThrottledPercent
	}
	if deltaPeriods == 0 {
		// No scheduling periods elapsed between samples: the container did no
		// work at all, which is genuinely zero throttling rather than unknown.
		return 0
	}
	return ThrottledPercent(deltaPeriods, deltaThrottled)
}
