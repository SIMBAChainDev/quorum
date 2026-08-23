package log

import (
	"context"
	"testing"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/mocktracer"
	ddtracer "github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/ethereum/go-ethereum/internal/telemetry/provider"
	"go.opentelemetry.io/otel/trace"
)

// captureHandler records every Record it's asked to log.
type captureHandler struct {
	records []*Record
}

func (h *captureHandler) Log(r *Record) error {
	h.records = append(h.records, r)
	return nil
}

// TestWriteDoesNotAccumulateTraceContext guards against a regression where
// write() appended dd.trace_id/dd.span_id onto the logger's persistent base
// context (l.ctx) instead of a per-record copy, causing every subsequent log
// line from that logger to carry an ever-growing list of every trace/span
// pair logged over the logger's lifetime.
func TestWriteDoesNotAccumulateTraceContext(t *testing.T) {
	mt := mocktracer.Start()
	defer mt.Stop()

	span := ddtracer.StartSpan("test-op")
	ctx := ddtracer.ContextWithSpan(context.Background(), span)

	capture := &captureHandler{}
	l := &logger{ctx: nil, logContext: ctx, h: new(swapHandler)}
	l.SetHandler(capture)

	const calls = 5
	for i := 0; i < calls; i++ {
		l.Info("test message")
	}

	if len(l.ctx) != 0 {
		t.Fatalf("logger's persistent base context grew: len(l.ctx)=%d, want 0 (write() must not mutate l.ctx)", len(l.ctx))
	}

	if len(capture.records) != calls {
		t.Fatalf("expected %d records, got %d", calls, len(capture.records))
	}

	// Each record's own Ctx should carry exactly the trace_id + span_id
	// key/value pairs (4 slice elements), not an accumulating count across
	// calls.
	for i, r := range capture.records {
		if len(r.Ctx) != 4 {
			t.Fatalf("record %d: len(Ctx)=%d, want 4 (trace_id+span_id pairs per line, not accumulated)", i, len(r.Ctx))
		}
	}
}

// TestWriteDatadogCorrelationKeys pins the Datadog log-correlation keys and
// their encodings: the Datadog log pipeline joins on dd.trace_id / dd.span_id
// verbatim.
func TestWriteDatadogCorrelationKeys(t *testing.T) {
	mt := mocktracer.Start()
	defer mt.Stop()
	provider.Set(provider.Datadog)
	t.Cleanup(func() { provider.Set(provider.Default) })

	span := ddtracer.StartSpan("test-op")
	ctx := ddtracer.ContextWithSpan(context.Background(), span)

	capture := &captureHandler{}
	l := &logger{logContext: ctx, h: new(swapHandler)}
	l.SetHandler(capture)
	l.Info("test message")

	fields := recordFields(t, capture)
	if got, want := fields["dd.trace_id"], span.Context().TraceID(); got != want {
		t.Fatalf("dd.trace_id = %q, want %q", got, want)
	}
	if fields["dd.span_id"] == "" {
		t.Fatal("dd.span_id is missing")
	}
}

// TestWriteOtelCorrelationKeys covers the OTLP branch: W3C hex-encoded
// trace_id/span_id taken from the otel span context.
func TestWriteOtelCorrelationKeys(t *testing.T) {
	provider.Set(provider.OTLP)
	t.Cleanup(func() { provider.Set(provider.Default) })

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := trace.SpanIDFromHex(spanID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))

	capture := &captureHandler{}
	l := &logger{logContext: ctx, h: new(swapHandler)}
	l.SetHandler(capture)
	l.Info("test message")

	fields := recordFields(t, capture)
	if got := fields["trace_id"]; got != traceID {
		t.Fatalf("trace_id = %q, want %q", got, traceID)
	}
	if got := fields["span_id"]; got != spanID {
		t.Fatalf("span_id = %q, want %q", got, spanID)
	}
	if _, ok := fields["dd.trace_id"]; ok {
		t.Fatal("Datadog correlation keys leaked onto the OTLP path")
	}
}

// TestWriteWithoutSpanAddsNoCorrelation checks that a span-less context (which
// is what log.Root() carries today) contributes no extra fields on either path.
func TestWriteWithoutSpanAddsNoCorrelation(t *testing.T) {
	for _, p := range []provider.Provider{provider.Datadog, provider.OTLP, provider.None} {
		t.Run(string(p), func(t *testing.T) {
			provider.Set(p)
			t.Cleanup(func() { provider.Set(provider.Default) })

			capture := &captureHandler{}
			l := &logger{logContext: context.Background(), h: new(swapHandler)}
			l.SetHandler(capture)
			l.Info("test message")

			if n := len(capture.records[0].Ctx); n != 0 {
				t.Fatalf("len(Ctx) = %d, want 0 without a span", n)
			}
		})
	}
}

// recordFields flattens the single captured record's key/value context.
func recordFields(t *testing.T, capture *captureHandler) map[string]string {
	t.Helper()
	if len(capture.records) != 1 {
		t.Fatalf("captured %d records, want 1", len(capture.records))
	}
	ctx := capture.records[0].Ctx
	if len(ctx)%2 != 0 {
		t.Fatalf("record context has an odd length %d", len(ctx))
	}
	fields := make(map[string]string, len(ctx)/2)
	for i := 0; i < len(ctx); i += 2 {
		key, ok := ctx[i].(string)
		if !ok {
			t.Fatalf("record context key %d is a %T, want string", i, ctx[i])
		}
		value, ok := ctx[i+1].(string)
		if !ok {
			t.Fatalf("record context value for %q is a %T, want string", key, ctx[i+1])
		}
		fields[key] = value
	}
	return fields
}
