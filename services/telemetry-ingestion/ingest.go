package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	telemetryv1 "github.com/intelletry/ev-telemetry/gen/telemetry/v1"
	"github.com/intelletry/ev-telemetry/internal/telemetry"
)

// ErrRateLimited means the credential is over its allowance.
var ErrRateLimited = errors.New("rate limit exceeded")

// MaxBatchEvents bounds one request, independent of the rate limit.
const MaxBatchEvents = 5000

// Metrics are the ingestion service's Prometheus series.
type Metrics struct {
	accepted        *prometheus.CounterVec
	rejected        prometheus.Counter
	rateLimited     prometheus.Counter
	authFailures    prometheus.Counter
	requestDuration *prometheus.HistogramVec
	batchSize       prometheus.Histogram
	queueDepth      prometheus.Gauge
	publishRetries  prometheus.Counter
	publishFailures prometheus.Counter
}

// NewMetrics registers the series.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		accepted:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ingest_events_accepted_total", Help: "Events durably written to Redis."}, []string{"transport"}),
		rejected:     prometheus.NewCounter(prometheus.CounterOpts{Name: "ingest_events_rejected_total", Help: "Events failing validation."}),
		rateLimited:  prometheus.NewCounter(prometheus.CounterOpts{Name: "ingest_rate_limited_total", Help: "Batches rejected by the rate limiter."}),
		authFailures: prometheus.NewCounter(prometheus.CounterOpts{Name: "ingest_auth_failures_total", Help: "Rejected credentials."}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ingest_request_duration_seconds", Help: "Request latency including durable write.",
			Buckets: []float64{.002, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"transport", "code"}),
		batchSize:       prometheus.NewHistogram(prometheus.HistogramOpts{Name: "ingest_flush_events", Help: "Events per Redis flush.", Buckets: prometheus.ExponentialBuckets(1, 4, 8)}),
		queueDepth:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "ingest_queue_depth", Help: "Jobs waiting in the in-process queue."}),
		publishRetries:  prometheus.NewCounter(prometheus.CounterOpts{Name: "ingest_publish_retries_total", Help: "Failed Redis publish attempts."}),
		publishFailures: prometheus.NewCounter(prometheus.CounterOpts{Name: "ingest_publish_failures_total", Help: "Flushes that failed after all retries."}),
	}
	reg.MustRegister(m.accepted, m.rejected, m.rateLimited, m.authFailures, m.requestDuration, m.batchSize, m.queueDepth, m.publishRetries, m.publishFailures)
	return m
}

// Authenticator validates bearer API keys against stored digests.
type Authenticator struct {
	keys []credential
}

type credential struct {
	id     string
	digest [32]byte
}

// NewAuthenticator parses "id:secret,id:secret".
func NewAuthenticator(spec string) (*Authenticator, error) {
	a := &Authenticator{}
	for _, item := range strings.Split(spec, ",") {
		id, secret, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok || id == "" || len(secret) < 16 {
			return nil, fmt.Errorf("API key %q must be id:secret with a secret of at least 16 characters", id)
		}
		a.keys = append(a.keys, credential{id, sha256.Sum256([]byte(secret))})
	}
	return a, nil
}

// Authenticate returns the credential ID for an "Authorization" value. It checks every
// key without an early exit, so timing does not reveal whether a key prefix matched.
func (a *Authenticator) Authenticate(header string) (string, bool) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", false
	}
	digest := sha256.Sum256([]byte(token))
	found := ""
	for _, k := range a.keys {
		if subtle.ConstantTimeCompare(digest[:], k.digest[:]) == 1 {
			found = k.id
		}
	}
	return found, found != ""
}

// Ingestor is the transport-independent core, shared by REST and gRPC so that the two
// cannot drift apart in behaviour.
type Ingestor struct {
	limiter *Limiter
	batcher *Batcher
	metrics *Metrics
	now     func() time.Time
}

// Ingest validates, rate limits and durably stores a batch. Invalid events are
// rejected individually so that one bad sensor reading cannot sink a whole batch.
func (i *Ingestor) Ingest(ctx context.Context, cred, transport string, batch *telemetryv1.TelemetryBatch) (*telemetryv1.IngestResponse, error) {
	ctx, span := otel.Tracer("telemetry-ingestion").Start(ctx, "ingest.batch")
	defer span.End()
	span.SetAttributes(attribute.Int("batch.events", len(batch.GetEvents())), attribute.String("transport", transport))

	if len(batch.GetEvents()) > MaxBatchEvents {
		return nil, fmt.Errorf("%w: batch of %d exceeds %d events", errInvalid, len(batch.GetEvents()), MaxBatchEvents)
	}
	if !i.limiter.Allow(ctx, cred, len(batch.GetEvents())) {
		i.metrics.rateLimited.Inc()
		return nil, ErrRateLimited
	}
	resp := &telemetryv1.IngestResponse{}
	now := i.now()
	valid := make([]*telemetryv1.TelemetryEvent, 0, len(batch.GetEvents()))
	for idx, ev := range batch.GetEvents() {
		if err := telemetry.Validate(ev, now); err != nil {
			resp.Rejected = append(resp.Rejected, &telemetryv1.Rejection{Index: uint32(idx), Reason: err.Error()})
			continue
		}
		valid = append(valid, ev)
	}
	i.metrics.rejected.Add(float64(len(resp.Rejected)))
	if len(valid) == 0 {
		return resp, nil
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if err := i.batcher.Submit(ctx, valid, carrier.Get("traceparent")); err != nil {
		return nil, err
	}
	resp.Accepted = uint32(len(valid))
	i.metrics.accepted.WithLabelValues(transport).Add(float64(len(valid)))
	return resp, nil
}

var errInvalid = errors.New("invalid request")
