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

	"github.com/ethereum/go-ethereum/internal/telemetry/provider"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

// WrapHandler instruments an inbound HTTP handler. When the OTLP provider is
// not selected it returns h unchanged, so the Datadog and none paths pay
// nothing.
//
// This produces one span per HTTP request (named "HTTP <verb>", since every
// JSON-RPC request is a POST to the same path and the URL carries no
// information). The exclude-methods deny-list in shouldTrace only suppresses
// the per-method child span created downstream by SpanForRPCMethod — the
// method name isn't known until the body is parsed, so this outer HTTP span
// cannot itself be deny-listed. A geth instance that only ever calls excluded
// methods still emits sampled, method-less HTTP spans; the deny-list reduces
// diagnostic noise from hot polling calls, it does not reduce their trace
// volume to zero.
func WrapHandler(h http.Handler) http.Handler {
	if provider.Current() != provider.OTLP {
		return h
	}
	return otelhttp.NewHandler(h, "",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			// A constant per method verb. Deriving the name from the URL would
			// be useless here and deriving it from the body would be both
			// expensive and unbounded in cardinality.
			return "HTTP " + r.Method
		}),
	)
}

// WrapTransport instruments an outbound HTTP round tripper, propagating the
// current trace context into the request headers. Returns rt unchanged unless
// the OTLP provider is selected.
func WrapTransport(rt http.RoundTripper) http.RoundTripper {
	if provider.Current() != provider.OTLP {
		return rt
	}
	return otelhttp.NewTransport(rt)
}

// WrapGRPCDialOptions appends client-side tracing to a set of gRPC dial
// options. Returns opts unchanged unless the OTLP provider is selected.
func WrapGRPCDialOptions(opts []grpc.DialOption) []grpc.DialOption {
	if provider.Current() != provider.OTLP {
		return opts
	}
	return append(opts, grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
}

// SpanForRPCMethod starts the span covering one JSON-RPC method call. This is
// the seam that also covers IPC and in-process calls, which never touch the
// HTTP stack.
//
// wsTransport must be true when the call arrived over a WebSocket connection.
// A WebSocket upgrade hijacks the connection and ServeHTTP returns as soon as
// the handshake completes, so the inbound HTTP span is already ended by the
// time any RPC call is read off that socket. Parenting to it would produce
// orphaned children of a closed span and would glue every call on the
// connection into one ever-growing trace, so such calls get a fresh root and,
// where a valid span context is present, a link back to it instead.
//
// The returned span is a non-recording no-op when tracing is disabled or the
// method is on the deny-list; ending it is always safe.
func SpanForRPCMethod(ctx context.Context, method string, wsTransport bool) (context.Context, trace.Span) {
	if provider.Current() != provider.OTLP || !shouldTrace(method) {
		return ctx, noopSpan
	}
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindServer)}
	if wsTransport {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
		}
		opts = append(opts, trace.WithNewRoot())
	}
	return currentTracer().Start(ctx, method, opts...)
}

// StartSpan starts a plain internal span. Callers must pass a low-cardinality,
// statically known name: nothing derived from request parameters, which are
// attacker-controlled and unbounded.
func StartSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if provider.Current() != provider.OTLP {
		return ctx, noopSpan
	}
	return currentTracer().Start(ctx, name)
}

// shouldTrace applies the deny-list. It runs before the sampler because the
// excluded methods are the ones polled hard enough to matter even at a 1%
// sampling ratio.
func shouldTrace(method string) bool {
	if set := excludedMethods.Load(); set != nil {
		if _, excluded := (*set)[method]; excluded {
			return false
		}
	}
	return true
}
