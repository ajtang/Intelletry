package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/ajtang/Intelletry/internal/stream"
)

// Hub fans one Redis subscription out to all feed connections on this pod. Each replica
// subscribes independently, so a connection sees events no matter which pod produced them.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	gauge   prometheus.Gauge
}

// NewHub returns an empty hub.
func NewHub(g prometheus.Gauge) *Hub { return &Hub{clients: map[chan []byte]struct{}{}, gauge: g} }

// Run consumes the Redis channel and reconnects after failures.
func (h *Hub) Run(ctx context.Context, rdb *redis.Client) {
	for ctx.Err() == nil {
		sub := rdb.Subscribe(ctx, stream.OpsChannel)
		// The channel only closes when the subscription does, so without this the loop
		// would outlive ctx and block process shutdown forever.
		stop := context.AfterFunc(ctx, func() { _ = sub.Close() })
		for msg := range sub.Channel() {
			h.broadcast([]byte(msg.Payload))
		}
		stop()
		_ = sub.Close()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (h *Hub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c <- msg:
		default:
			// A slow browser loses feed entries instead of stalling every other client.
		}
	}
}

func (h *Hub) add() chan []byte {
	c := make(chan []byte, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
	if h.gauge != nil {
		h.gauge.Set(float64(len(h.clients)))
	}
	return c
}

func (h *Hub) remove(c chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	if h.gauge != nil {
		h.gauge.Set(float64(len(h.clients)))
	}
}

func prepareSSE(w http.ResponseWriter) (http.Flusher, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming unsupported")
	}
	// Streams outlive the server's WriteTimeout, which still protects ordinary requests.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("could not clear write deadline", "error", err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // otherwise ingress-nginx buffers the stream
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, nil
}

func (a *API) events(w http.ResponseWriter, r *http.Request) error {
	flusher, err := prepareSSE(w)
	if err != nil {
		return err
	}
	c := a.hub.add()
	defer a.hub.remove(c)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-a.stopStreams:
			return nil
		case msg := <-c:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return nil
			}
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return nil
			}
		}
		flusher.Flush()
	}
}

func (a *API) vehicleStream(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	sub := a.rdb.Subscribe(r.Context(), stream.VehicleChannel(id))
	defer sub.Close()
	flusher, err := prepareSSE(w)
	if err != nil {
		return err
	}
	// Subscribe before reading current state, so no update falls between the two.
	if raw, err := a.rdb.HGet(r.Context(), stream.FleetStateKey, id).Bytes(); err == nil && json.Valid(raw) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		flusher.Flush()
	}
	messages := sub.Channel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-a.stopStreams:
			return nil
		case msg, ok := <-messages:
			if !ok {
				return nil
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg.Payload); err != nil {
				return nil
			}
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return nil
			}
		}
		flusher.Flush()
	}
}
