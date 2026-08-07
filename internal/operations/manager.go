package operations

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/rousoftware/asgard/internal/store"
)

type Handler func(context.Context, store.Operation) error

type Manager struct {
	store    *store.Store
	workers  int
	queue    chan string
	mu       sync.Mutex
	handlers map[string]Handler
	cancels  map[string]context.CancelFunc
	wg       sync.WaitGroup
}

func New(store *store.Store, workers int) *Manager {
	return &Manager{store: store, workers: workers, queue: make(chan string, 256), handlers: map[string]Handler{}, cancels: map[string]context.CancelFunc{}}
}
func (m *Manager) Register(kind string, handler Handler) { m.handlers[kind] = handler }

func (m *Manager) Start(ctx context.Context) error {
	for i := 0; i < m.workers; i++ {
		m.wg.Add(1)
		go m.worker(ctx)
	}
	ids, err := m.store.QueuedOperations(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		m.Enqueue(id)
	}
	return nil
}
func (m *Manager) Wait() { m.wg.Wait() }
func (m *Manager) Enqueue(id string) {
	select {
	case m.queue <- id:
	default:
		go func() { m.queue <- id }()
	}
}

func (m *Manager) worker(root context.Context) {
	defer m.wg.Done()
	for {
		select {
		case <-root.Done():
			return
		case id := <-m.queue:
			m.run(root, id)
		}
	}
}

func (m *Manager) run(root context.Context, id string) {
	op, err := m.store.GetOperation(root, id)
	if err != nil || op.Status != "queued" {
		return
	}
	handler, ok := m.handlers[op.Kind]
	if !ok {
		_ = m.store.CompleteOperation(root, id, "failed", "no handler registered for "+op.Kind)
		return
	}
	ctx, cancel := context.WithCancel(root)
	m.mu.Lock()
	m.cancels[id] = cancel
	m.mu.Unlock()
	defer func() { cancel(); m.mu.Lock(); delete(m.cancels, id); m.mu.Unlock() }()
	if err := m.store.StartOperation(ctx, id); err != nil {
		return
	}
	_ = m.store.LogOperation(ctx, id, "info", "Operation started")
	err = handler(ctx, op)
	if err == nil {
		_ = m.store.LogOperation(ctx, id, "info", "Operation completed")
		_ = m.store.CompleteOperation(ctx, id, "succeeded", "")
		return
	}
	status := "failed"
	if errors.Is(err, context.Canceled) {
		status = "canceled"
	}
	_ = m.store.LogOperation(context.WithoutCancel(root), id, "error", err.Error())
	_ = m.store.CompleteOperation(context.WithoutCancel(root), id, status, err.Error())
}

func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	cancel := m.cancels[id]
	m.mu.Unlock()
	if cancel == nil {
		op, err := m.store.GetOperation(context.Background(), id)
		if err != nil {
			return err
		}
		if op.Status == "queued" {
			return m.store.CompleteOperation(context.Background(), id, "canceled", "canceled before start")
		}
		return fmt.Errorf("operation is %s", op.Status)
	}
	cancel()
	return nil
}
