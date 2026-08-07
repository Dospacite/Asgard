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
		_, err = c.Store.DB.ExecContext(ctx, `INSERT INTO metrics(service_id,container_id,cpu_percent,memory_bytes,memory_limit,network_rx,network_tx,block_read,block_write,pids,collected_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, item.ServiceID, item.ID, stats.CPUPercent, stats.MemoryBytes, stats.MemoryLimit, stats.NetworkRX, stats.NetworkTX, stats.BlockRead, stats.BlockWrite, stats.PIDs, stats.CollectedAt.Format(time.RFC3339Nano))
		if err != nil {
			c.logger.Warn("store metrics", "error", err)
		}
	}
	_, _ = c.Store.DB.ExecContext(ctx, `DELETE FROM metrics WHERE collected_at < ?`, time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339Nano))
}
