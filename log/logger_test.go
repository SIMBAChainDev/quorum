package log

import (
	"context"
	"testing"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/mocktracer"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
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

	span := tracer.StartSpan("test-op")
	ctx := tracer.ContextWithSpan(context.Background(), span)

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
