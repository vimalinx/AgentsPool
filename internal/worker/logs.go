package worker

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/client"
	"github.com/qiyuehuajin/agentspool/internal/store"
)

type logBatcher struct {
	client     *client.Client
	leaseID    string
	leaseToken string
	maxChunk   int
	logger     *slog.Logger
	ctx        context.Context
	cancel     context.CancelFunc
	channel    chan store.LogEvent
	done       chan struct{}
	sequence   atomic.Int64
	dropped    atomic.Uint64
	closeOnce  sync.Once
}

func newLogBatcher(parent context.Context, apiClient *client.Client, leaseID, leaseToken string, maxChunk int, logger *slog.Logger) *logBatcher {
	if maxChunk <= 0 {
		maxChunk = 64 << 10
	}
	ctx, cancel := context.WithCancel(parent)
	batcher := &logBatcher{
		client: apiClient, leaseID: leaseID, leaseToken: leaseToken, maxChunk: maxChunk,
		logger: logger, ctx: ctx, cancel: cancel, channel: make(chan store.LogEvent, 1024), done: make(chan struct{}),
	}
	go batcher.run()
	return batcher
}

func (b *logBatcher) Emit(stream, data string) {
	if data == "" {
		return
	}
	for len(data) > 0 {
		length := len(data)
		if length > b.maxChunk {
			length = b.maxChunk
		}
		event := store.LogEvent{Sequence: b.sequence.Add(1), Timestamp: time.Now().UTC(), Stream: stream, Data: data[:length]}
		select {
		case b.channel <- event:
		case <-b.ctx.Done():
			return
		default:
			b.dropped.Add(1)
		}
		data = data[length:]
	}
}

func (b *logBatcher) Close() {
	b.closeOnce.Do(func() {
		close(b.channel)
		<-b.done
		b.cancel()
	})
}

func (b *logBatcher) run() {
	defer close(b.done)
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]store.LogEvent, 0, 128)
	bytes := 0
	flush := func() {
		if dropped := b.dropped.Swap(0); dropped > 0 {
			batch = append(batch, store.LogEvent{Sequence: b.sequence.Add(1), Timestamp: time.Now().UTC(), Stream: "system", Data: "AgentsPool worker dropped log chunks because the local buffer was full\n"})
		}
		if len(batch) == 0 {
			return
		}
		request := struct {
			Events []store.LogEvent `json:"events"`
		}{Events: batch}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := b.client.DoJSON(ctx, http.MethodPost, "/api/v1/leases/"+b.leaseID+"/logs", nil, request, nil, map[string]string{"X-AgentsPool-Lease-Token": b.leaseToken})
		cancel()
		if err != nil {
			b.logger.Warn("failed to deliver execution logs", "lease_id", b.leaseID, "events", len(batch), "error", err)
		}
		batch = make([]store.LogEvent, 0, 128)
		bytes = 0
	}
	for {
		select {
		case event, ok := <-b.channel:
			if !ok {
				flush()
				return
			}
			batch = append(batch, event)
			bytes += len(event.Data)
			if len(batch) >= 100 || bytes >= 256<<10 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-b.ctx.Done():
			flush()
			return
		}
	}
}
