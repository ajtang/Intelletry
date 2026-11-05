package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	telemetryv1 "github.com/intelletry/ev-telemetry/gen/telemetry/v1"
)

const maxBodyBytes = 8 << 20

var (
	jsonIn  = protojson.UnmarshalOptions{DiscardUnknown: true}
	jsonOut = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
)

// HTTPHandler serves POST /telemetry. The body is a batch ({"events":[...]}), or a
// single event object as a convenience. Protobuf is accepted via Content-Type.
func HTTPHandler(i *Ingestor, auth *Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		code := serveTelemetry(i, auth, w, r)
		i.metrics.requestDuration.WithLabelValues("http", strconv.Itoa(code)).Observe(time.Since(start).Seconds())
		level := slog.LevelDebug
		if code >= 500 {
			level = slog.LevelWarn
		}
		slog.Log(r.Context(), level, "telemetry request", "status", code, "duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

func fail(w http.ResponseWriter, code int, msg string) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"error":` + strconv.Quote(msg) + `}`))
	return code
}

func serveTelemetry(i *Ingestor, auth *Authenticator, w http.ResponseWriter, r *http.Request) int {
	cred, ok := auth.Authenticate(r.Header.Get("Authorization"))
	if !ok {
		i.metrics.authFailures.Inc()
		w.Header().Set("WWW-Authenticate", "Bearer")
		return fail(w, http.StatusUnauthorized, "invalid or missing API key")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return fail(w, http.StatusRequestEntityTooLarge, "request body too large")
	}
	batch := &telemetryv1.TelemetryBatch{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-protobuf") {
		err = proto.Unmarshal(body, batch)
	} else if err = jsonIn.Unmarshal(body, batch); err != nil || len(batch.Events) == 0 {
		single := &telemetryv1.TelemetryEvent{}
		if singleErr := jsonIn.Unmarshal(body, single); singleErr == nil && single.VehicleId != "" {
			batch, err = &telemetryv1.TelemetryBatch{Events: []*telemetryv1.TelemetryEvent{single}}, nil
		}
	}
	if err != nil {
		return fail(w, http.StatusBadRequest, "malformed body: "+err.Error())
	}
	if len(batch.Events) == 0 {
		return fail(w, http.StatusBadRequest, "no events")
	}
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := i.Ingest(ctx, cred, "http", batch)
	if err != nil {
		switch {
		case errors.Is(err, errInvalid):
			return fail(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrRateLimited):
			w.Header().Set("Retry-After", "1")
			return fail(w, http.StatusTooManyRequests, err.Error())
		default:
			w.Header().Set("Retry-After", "2")
			return fail(w, http.StatusServiceUnavailable, "temporarily unable to store telemetry")
		}
	}
	code := http.StatusAccepted
	if resp.Accepted == 0 {
		code = http.StatusUnprocessableEntity
	}
	out, _ := jsonOut.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(out)
	return code
}

// GRPCServer implements TelemetryService.
type GRPCServer struct {
	telemetryv1.UnimplementedTelemetryServiceServer
	ingestor *Ingestor
}

type credKey struct{}

func toStatus(err error) error {
	switch {
	case errors.Is(err, errInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrRateLimited):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "cancelled")
	default:
		return status.Error(codes.Unavailable, "temporarily unable to store telemetry")
	}
}

// IngestBatch implements the unary RPC.
func (s *GRPCServer) IngestBatch(ctx context.Context, b *telemetryv1.TelemetryBatch) (*telemetryv1.IngestResponse, error) {
	start := time.Now()
	resp, err := s.ingestor.Ingest(ctx, ctx.Value(credKey{}).(string), "grpc", b)
	s.ingestor.metrics.requestDuration.WithLabelValues("grpc", status.Code(toStatusOrNil(err)).String()).Observe(time.Since(start).Seconds())
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}

func toStatusOrNil(err error) error {
	if err == nil {
		return nil
	}
	return toStatus(err)
}

// StreamTelemetry acknowledges each batch on a long-lived connection. Batches are
// handled in sequence, which preserves order for a gateway sending one vehicle's data.
func (s *GRPCServer) StreamTelemetry(st grpc.BidiStreamingServer[telemetryv1.TelemetryBatch, telemetryv1.IngestResponse]) error {
	ctx := st.Context()
	cred := ctx.Value(credKey{}).(string)
	for {
		b, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		resp, err := s.ingestor.Ingest(ctx, cred, "grpc-stream", b)
		if err != nil {
			return toStatus(err)
		}
		if err := st.Send(resp); err != nil {
			return err
		}
	}
}

func authenticateGRPC(ctx context.Context, auth *Authenticator, m *Metrics) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 1 {
		if cred, ok := auth.Authenticate(values[0]); ok {
			return context.WithValue(ctx, credKey{}, cred), nil
		}
	}
	m.authFailures.Inc()
	return nil, status.Error(codes.Unauthenticated, "invalid or missing API key")
}

// UnaryAuth authenticates unary calls, except the standard health service.
func UnaryAuth(auth *Authenticator, m *Metrics) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.") {
			return h(ctx, req)
		}
		ctx, err := authenticateGRPC(ctx, auth, m)
		if err != nil {
			return nil, err
		}
		return h(ctx, req)
	}
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authedStream) Context() context.Context { return s.ctx }

// StreamAuth authenticates streaming calls, except the standard health service.
func StreamAuth(auth *Authenticator, m *Metrics) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.") {
			return h(srv, ss)
		}
		ctx, err := authenticateGRPC(ss.Context(), auth, m)
		if err != nil {
			return err
		}
		return h(srv, &authedStream{ss, ctx})
	}
}
