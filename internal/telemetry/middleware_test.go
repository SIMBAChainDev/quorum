// Copyright 2025 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/internal/telemetry/provider"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
)

type stubHandler struct{}

func (*stubHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

type stubTransport struct{}

func (*stubTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

// TestWrappersPassThrough asserts the Datadog and none paths pay nothing: the
// wrappers must hand back the very same handler/transport/options they were
// given.
func TestWrappersPassThrough(t *testing.T) {
	for _, p := range []provider.Provider{provider.Datadog, provider.None} {
		t.Run(string(p), func(t *testing.T) {
			provider.Set(p)
			t.Cleanup(resetTelemetry)

			handler := &stubHandler{}
			if got := WrapHandler(handler); got != http.Handler(handler) {
				t.Errorf("WrapHandler returned %T, want the original handler", got)
			}
			transport := &stubTransport{}
			if got := WrapTransport(transport); got != http.RoundTripper(transport) {
				t.Errorf("WrapTransport returned %T, want the original transport", got)
			}
			opts := []grpc.DialOption{}
			if got := WrapGRPCDialOptions(opts); len(got) != 0 {
				t.Errorf("WrapGRPCDialOptions added %d options, want 0", len(got))
			}
			if got := WrapGRPCDialOptions(nil); got != nil {
				t.Errorf("WrapGRPCDialOptions(nil) = %v, want nil", got)
			}
		})
	}
}

func TestWrappersInstrumentOnOTLP(t *testing.T) {
	installRecorder(t, 1)

	handler := &stubHandler{}
	if got := WrapHandler(handler); got == http.Handler(handler) {
		t.Error("WrapHandler did not wrap the handler on the OTLP path")
	}
	transport := &stubTransport{}
	if got := WrapTransport(transport); got == http.RoundTripper(transport) {
		t.Error("WrapTransport did not wrap the transport on the OTLP path")
	}
	if got := WrapGRPCDialOptions(nil); len(got) != 1 {
		t.Errorf("WrapGRPCDialOptions(nil) returned %d options, want 1", len(got))
	}
}

func TestSpanForRPCMethodDenyList(t *testing.T) {
	tests := []struct {
		method    string
		wantSpans int
	}{
		{"eth_getBalance", 1},
		{"eth_sendRawTransaction", 1},
		{"eth_blockNumber", 0},
		{"eth_syncing", 0},
		{"net_version", 0},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			rec := installRecorder(t, 1)

			_, span := SpanForRPCMethod(context.Background(), tt.method, false)
			span.End()

			got := spansNamed(rec, tt.method)
			if len(got) != tt.wantSpans {
				t.Fatalf("recorded %d spans named %q, want %d", len(got), tt.method, tt.wantSpans)
			}
		})
	}
}

func TestSpanForRPCMethodNameIsTheMethod(t *testing.T) {
	rec := installRecorder(t, 1)

	_, span := SpanForRPCMethod(context.Background(), "eth_getBalance", false)
	span.End()

	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if got := ended[0].Name(); got != "eth_getBalance" {
		t.Fatalf("span name = %q, want %q", got, "eth_getBalance")
	}
}

func TestSpanForRPCMethodDisabledWithoutOTLP(t *testing.T) {
	rec := installRecorder(t, 1)
	provider.Set(provider.Datadog)

	ctx, span := SpanForRPCMethod(context.Background(), "eth_getBalance", false)
	span.End()

	if span.SpanContext().IsValid() {
		t.Error("got a recording span while the Datadog provider is selected")
	}
	if ctx != context.Background() {
		t.Error("context was modified while tracing is disabled")
	}
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("recorded %d spans, want 0", n)
	}
}

// TestSpanForRPCMethodHTTPParenting is the contrast case for the WebSocket rule
// below: a call arriving on a live HTTP request should be a child of that
// request's span.
func TestSpanForRPCMethodHTTPParenting(t *testing.T) {
	rec := installRecorder(t, 1)

	parentCtx, parent := currentTracer().Start(context.Background(), "HTTP POST")
	_, child := SpanForRPCMethod(parentCtx, "eth_getBalance", false)
	child.End()
	parent.End()

	if got, want := child.SpanContext().TraceID(), parent.SpanContext().TraceID(); got != want {
		t.Fatalf("child trace ID = %s, want the parent's %s", got, want)
	}
	if n := len(spansNamed(rec, "eth_getBalance")); n != 1 {
		t.Fatalf("recorded %d method spans, want 1", n)
	}
}

// TestWebsocketCallsGetIndependentTraces covers the WebSocket span-parenting
// rule. The upgrade request's span ends at the handshake, long before any RPC
// call is read off the socket, so calls must not be parented to it: each gets
// its own trace, with a link back rather than a parent edge.
func TestWebsocketCallsGetIndependentTraces(t *testing.T) {
	rec := installRecorder(t, 1)

	// The span otelhttp created for the upgrade request, already finished.
	connCtx, upgrade := currentTracer().Start(context.Background(), "HTTP GET")
	upgrade.End()

	_, first := SpanForRPCMethod(connCtx, "eth_getBalance", true)
	first.End()
	_, second := SpanForRPCMethod(connCtx, "eth_getBalance", true)
	second.End()

	upgradeTrace := upgrade.SpanContext().TraceID()
	firstTrace := first.SpanContext().TraceID()
	secondTrace := second.SpanContext().TraceID()

	if firstTrace == secondTrace {
		t.Fatalf("both WebSocket calls share trace ID %s; each must be its own root", firstTrace)
	}
	if firstTrace == upgradeTrace || secondTrace == upgradeTrace {
		t.Fatalf("a WebSocket call was parented to the finished upgrade span (trace %s)", upgradeTrace)
	}

	// The connection is still recoverable through a link.
	for _, span := range spansNamed(rec, "eth_getBalance") {
		links := span.Links()
		if len(links) != 1 {
			t.Fatalf("span has %d links, want 1 back to the connection span", len(links))
		}
		if got := links[0].SpanContext.SpanID(); got != upgrade.SpanContext().SpanID() {
			t.Fatalf("link points at span %s, want the upgrade span %s", got, upgrade.SpanContext().SpanID())
		}
	}
}

// TestInboundTraceparentIsHonoured drives a full inbound request through
// WrapHandler to prove the ParentBased sampler adopts a caller's trace instead
// of starting a new one. The sampling ratio is 0, so the only way a span can be
// recorded at all is by inheriting the caller's sampled decision.
func TestInboundTraceparentIsHonoured(t *testing.T) {
	rec := installRecorder(t, 0)

	handler := WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, span := SpanForRPCMethod(r.Context(), "eth_getBalance", false)
		span.End()
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	method := spansNamed(rec, "eth_getBalance")
	if len(method) != 1 {
		t.Fatalf("recorded %d method spans, want 1 (the inbound sampled decision must be honoured)", len(method))
	}
	if got := method[0].SpanContext().TraceID().String(); got != traceID {
		t.Fatalf("method span trace ID = %s, want the inbound %s", got, traceID)
	}
}

// TestUnsampledTraceparentIsHonoured is the mirror image: a caller that says
// "do not sample" must not produce spans either.
func TestUnsampledTraceparentIsHonoured(t *testing.T) {
	rec := installRecorder(t, 1)

	handler := WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, span := SpanForRPCMethod(r.Context(), "eth_getBalance", false)
		span.End()
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("recorded %d spans, want 0 for an explicitly unsampled caller", n)
	}
}

// TestWrapHandlerSpanNameIsConstant guards the cardinality rule for the
// transport span: every JSON-RPC request is a POST to the same path, and the
// meaningful name belongs on the per-method span instead.
func TestWrapHandlerSpanNameIsConstant(t *testing.T) {
	rec := installRecorder(t, 1)

	srv := httptest.NewServer(WrapHandler(&stubHandler{}))
	defer srv.Close()

	for _, path := range []string{"/", "/graphql", "/some/other/path"} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	ended := rec.Ended()
	if len(ended) != 3 {
		t.Fatalf("recorded %d transport spans, want 3", len(ended))
	}
	for _, span := range ended {
		if got := span.Name(); got != "HTTP POST" {
			t.Fatalf("transport span name = %q, want %q", got, "HTTP POST")
		}
	}
}

func TestStartSpanPassThrough(t *testing.T) {
	rec := installRecorder(t, 1)
	provider.Set(provider.None)

	ctx, span := StartSpan(context.Background(), "graphql.request")
	span.End()
	if ctx != context.Background() {
		t.Error("context was modified while tracing is disabled")
	}
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("recorded %d spans, want 0", n)
	}

	provider.Set(provider.OTLP)
	_, span = StartSpan(context.Background(), "graphql.request")
	span.End()
	if n := len(spansNamed(rec, "graphql.request")); n != 1 {
		t.Fatalf("recorded %d graphql spans, want 1", n)
	}
}

func spansNamed(rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}
