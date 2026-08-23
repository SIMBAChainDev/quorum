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

package rpc

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/internal/telemetry"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The WebSocket codec must carry the transport marker runMethod keys off, and
// nothing else may: HTTP, IPC and in-process calls all want normal parenting.
var _ websocketTransport = (*websocketCodec)(nil)

func TestOnlyWebsocketCodecIsMarkedAsWebsocketTransport(t *testing.T) {
	if _, ok := interface{}(&jsonCodec{}).(websocketTransport); ok {
		t.Fatal("jsonCodec claims to be a WebSocket transport; HTTP and IPC calls would lose span parenting")
	}
}

// recordSpans installs an in-memory tracing pipeline for the duration of a test.
func recordSpans(t *testing.T, excludeMethods ...string) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	restore := telemetry.InstallForTesting(sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(rec),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	), excludeMethods)
	t.Cleanup(restore)
	return rec
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

// TestTelemetrySpanPerMethod checks the runMethod seam: one span per call, named
// after the RPC method.
func TestTelemetrySpanPerMethod(t *testing.T) {
	rec := recordSpans(t)

	server := newTestServer()
	defer server.Stop()
	client := DialInProc(server)
	defer client.Close()

	var resp echoResult
	if err := client.Call(&resp, "test_echo", "hello", 10, nil); err != nil {
		t.Fatal(err)
	}

	if n := len(spansNamed(rec, "test_echo")); n != 1 {
		t.Fatalf("recorded %d spans named test_echo, want 1", n)
	}
}

// TestTelemetryExcludedMethodProducesNoSpan checks that the deny-list is applied
// before any span is created, on the seam that all transports share.
func TestTelemetryExcludedMethodProducesNoSpan(t *testing.T) {
	rec := recordSpans(t, "test_echo")

	server := newTestServer()
	defer server.Stop()
	client := DialInProc(server)
	defer client.Close()

	var resp echoResult
	if err := client.Call(&resp, "test_echo", "hello", 10, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&resp, "test_echoWithCtx", "hello", 10, nil); err != nil {
		t.Fatal(err)
	}

	if n := len(spansNamed(rec, "test_echo")); n != 0 {
		t.Fatalf("recorded %d spans for the excluded method, want 0", n)
	}
	if n := len(spansNamed(rec, "test_echoWithCtx")); n != 1 {
		t.Fatalf("recorded %d spans for the traced method, want 1", n)
	}
}

// TestTelemetryWebsocketCallsAreIndependentTraces is the end-to-end version of
// the WebSocket span-parenting rule. A WS upgrade hijacks the connection and
// ServeHTTP returns at the handshake, so the inbound HTTP span is finished long
// before any call is read off the socket. Every call on the connection must
// therefore get its own trace rather than being glued into one.
//
// Server.ServeCodec happens to root the WebSocket handler's context at
// context.Background() today, so this passes for two independent reasons; it is
// here to catch a future change that plumbs the upgrade request's context
// through. The discriminating test for the parenting rule itself lives in
// internal/telemetry.
func TestTelemetryWebsocketCallsAreIndependentTraces(t *testing.T) {
	rec := recordSpans(t)

	server := newTestServer()
	defer server.Stop()
	httpsrv := httptest.NewServer(server.WebsocketHandler([]string{"*"}))
	defer httpsrv.Close()

	client, err := DialWebsocket(context.Background(), "ws:"+strings.TrimPrefix(httpsrv.URL, "http:"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var resp echoResult
	for i := 0; i < 2; i++ {
		if err := client.Call(&resp, "test_echo", "hello", 10, nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	spans := spansNamed(rec, "test_echo")
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans named test_echo, want 2", len(spans))
	}
	first, second := spans[0].SpanContext().TraceID(), spans[1].SpanContext().TraceID()
	if first == second {
		t.Fatalf("both WebSocket calls share trace ID %s; each call must be its own root", first)
	}
	for i, s := range spans {
		if s.Parent().IsValid() {
			t.Fatalf("span %d has parent %s; WebSocket calls must be roots", i, s.Parent().SpanID())
		}
	}
}
