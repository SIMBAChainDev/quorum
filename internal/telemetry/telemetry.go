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

// Package telemetry selects and drives the process-wide tracing backend.
//
// Two backends are supported. The Datadog one needs no runtime code at all:
// Orchestrion weaves dd-trace-go into the binary at build time and injects
// tracer.Start() into package main's init(), so it is already running before
// any flag has been parsed. The OTLP one is a real OpenTelemetry SDK pipeline
// built here, and requires the injected Datadog tracer to be stopped first.
//
// Everything in this package degrades to a no-op rather than failing: a
// collector outage must never stop a node from serving.
package telemetry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ddtracer "github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/ethereum/go-ethereum/internal/telemetry/provider"
	"github.com/ethereum/go-ethereum/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	// DefaultServiceName is reported as service.name when neither the config
	// nor the standard OTEL_* environment variables supply one.
	DefaultServiceName = "quorum"

	// DefaultSampleRatio is deliberately low: geth's JSON-RPC surface is polled
	// hard, and an unsampled pipeline floods a collector.
	DefaultSampleRatio = 0.01

	// tracerName identifies this instrumentation scope.
	tracerName = "github.com/ethereum/go-ethereum"

	// reinitFlushTimeout bounds the flush performed when Init replaces an
	// already-running pipeline.
	reinitFlushTimeout = 5 * time.Second
)

// DefaultExcludedMethods lists JSON-RPC methods whose per-method span is
// never created (see middleware.shouldTrace). These are the cheap,
// high-frequency polling calls that carry no diagnostic value but dominate
// request counts. This only suppresses the method-level span — see the
// WrapHandler doc comment for why the outer per-HTTP-request span cannot
// itself be deny-listed.
var DefaultExcludedMethods = []string{
	"eth_blockNumber",
	"eth_syncing",
	"eth_chainId",
	"net_version",
	"eth_gasPrice",
	"web3_clientVersion",
}

// Config is the tracing configuration. It is exposed as a TOML section by
// cmd/geth, so every field must stay encodable.
type Config struct {
	// Provider selects the backend: "datadog" (default), "otlp" or "none".
	Provider string `toml:",omitempty"`
	// Endpoint is the OTLP collector address, e.g. "localhost:4317". When
	// empty the standard OTEL_EXPORTER_OTLP_ENDPOINT environment variable is
	// used instead.
	Endpoint string `toml:",omitempty"`
	// SampleRatio is the head-sampling ratio applied to traces without an
	// inbound sampling decision. Not omitempty: an explicit 0 (trace nothing)
	// must round-trip through TOML rather than reverting to the default.
	SampleRatio float64
	// ExcludeMethods lists JSON-RPC methods that are never traced. Not
	// omitempty: an explicit empty list (trace everything) must round-trip
	// through TOML rather than being indistinguishable from "unset" and
	// reverting to DefaultExcludedMethods.
	ExcludeMethods []string
	// ServiceName is reported as the OpenTelemetry service.name resource
	// attribute.
	ServiceName string `toml:",omitempty"`
}

// DefaultConfig returns the configuration used when nothing is specified.
func DefaultConfig() Config {
	return Config{
		Provider:       string(provider.Default),
		SampleRatio:    DefaultSampleRatio,
		ExcludeMethods: append([]string(nil), DefaultExcludedMethods...),
		ServiceName:    DefaultServiceName,
	}
}

// tracerHolder lets a trace.Tracer live in an atomic.Value even as the concrete
// type behind the interface changes.
type tracerHolder struct{ t trace.Tracer }

var (
	// noopSpan is the span handed back when tracing is off or the method is
	// excluded. Ending it and querying it are both safe.
	noopSpan = trace.SpanFromContext(context.Background())

	// activeTracer holds the tracer belonging to the installed pipeline. It is
	// resolved once per install rather than looked up per span: otel's global
	// delegating tracer only ever binds to the first provider registered, and
	// the SDK provider takes a lock on every Tracer() call.
	activeTracer atomic.Value // tracerHolder

	// pipelineInstalled reports whether a real (non no-op) pipeline is live.
	pipelineInstalled atomic.Bool

	// excludedMethods is the deny-list, consulted before the sampler.
	excludedMethods atomic.Pointer[map[string]struct{}]

	shutdownMu sync.Mutex
	// shutdown defaults to a safe no-op and is only replaced by Init. Shutdown
	// is reachable without Init ever having run: cmd/clef calls debug.Exit() on
	// repeated SIGINT but never calls debug.Setup(), and a nil func here would
	// turn that diagnostic path into a nil-pointer panic.
	shutdown = func(context.Context) error { return nil }
)

// Init resolves the configured provider and, for OTLP, builds the tracing
// pipeline. It never fails: an unusable configuration is logged and tracing is
// left disabled.
//
// Init must run before any traced work starts. Calling it again replaces the
// previous pipeline, flushing it first.
func Init(cfg Config) {
	if pipelineInstalled.Load() {
		// Replacing a live pipeline: flush it first so its spans are not
		// dropped and its exporter goroutines do not leak.
		ctx, cancel := context.WithTimeout(context.Background(), reinitFlushTimeout)
		if err := Shutdown(ctx); err != nil {
			log.Warn("Failed to flush the previous tracing pipeline", "err", err)
		}
		cancel()
	}

	p, ok := provider.Parse(cfg.Provider)
	if !ok {
		log.Warn("Unknown tracing provider, falling back to default",
			"provider", cfg.Provider, "fallback", string(provider.Default))
	}
	provider.Set(p)
	setExcludedMethods(cfg.ExcludeMethods)

	if p != provider.Datadog {
		// Orchestrion injected tracer.Start() into package main's init(), so
		// the Datadog tracer is already live. Stopping it swaps the global
		// tracer for a no-op, which makes every injected call site inert.
		ddtracer.Stop()
	}
	if p != provider.OTLP {
		return
	}
	if err := initOTLP(cfg); err != nil {
		log.Warn("Failed to initialise OTLP tracing, continuing without traces", "err", err)
	}
}

// Shutdown flushes any buffered spans. It is safe to call without a preceding
// Init, and safe to call more than once; only the first call does work.
func Shutdown(ctx context.Context) error {
	shutdownMu.Lock()
	fn := shutdown
	shutdown = func(context.Context) error { return nil }
	shutdownMu.Unlock()
	pipelineInstalled.Store(false)
	activeTracer.Store(tracerHolder{t: noop.Tracer{}})
	return fn(ctx)
}

func initOTLP(cfg Config) error {
	ctx := context.Background()

	exporter, err := otlptracegrpc.New(ctx, exporterOptions(cfg.Endpoint)...)
	if err != nil {
		return fmt.Errorf("creating OTLP trace exporter: %w", err)
	}

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		// A partial resource is still usable; only a hard failure aborts.
		if res == nil {
			return fmt.Errorf("building telemetry resource: %w", err)
		}
		log.Warn("Partial OpenTelemetry resource detected", "err", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler(cfg.SampleRatio)),
	)
	install(tp)
	log.Info("OTLP tracing enabled", "endpoint", endpointForLog(cfg.Endpoint),
		"service", serviceName, "sampleratio", clampRatio(cfg.SampleRatio))
	return nil
}

// exporterOptions turns a bare host:port into gRPC exporter options. An empty
// endpoint leaves the SDK to read OTEL_EXPORTER_OTLP_ENDPOINT. A value carrying
// a scheme picks TLS or plaintext from that scheme; a bare host:port is treated
// as plaintext, which is what an in-cluster collector sidecar wants.
func exporterOptions(endpoint string) []otlptracegrpc.Option {
	switch {
	case endpoint == "":
		return nil
	case strings.Contains(endpoint, "://"):
		return []otlptracegrpc.Option{otlptracegrpc.WithEndpointURL(endpoint)}
	default:
		return []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(),
		}
	}
}

func endpointForLog(endpoint string) string {
	if endpoint == "" {
		return "$OTEL_EXPORTER_OTLP_ENDPOINT"
	}
	return endpoint
}

// sampler honours an inbound sampling decision (so a caller's traceparent wins)
// and otherwise applies head sampling at the configured ratio.
func sampler(ratio float64) sdktrace.Sampler {
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(clampRatio(ratio)))
}

func clampRatio(ratio float64) float64 {
	switch {
	case ratio < 0:
		return 0
	case ratio > 1:
		return 1
	default:
		return ratio
	}
}

// install publishes a tracer provider as the process-wide one and records how
// to flush it.
func install(tp *sdktrace.TracerProvider) {
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	activeTracer.Store(tracerHolder{t: tp.Tracer(tracerName)})

	shutdownMu.Lock()
	shutdown = tp.Shutdown
	shutdownMu.Unlock()
	pipelineInstalled.Store(true)
}

func setExcludedMethods(methods []string) {
	set := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		if m = strings.TrimSpace(m); m != "" {
			set[m] = struct{}{}
		}
	}
	excludedMethods.Store(&set)
}

// currentTracer returns the installed tracer, or a no-op one before Init.
func currentTracer() trace.Tracer {
	if h, ok := activeTracer.Load().(tracerHolder); ok && h.t != nil {
		return h.t
	}
	return noop.Tracer{}
}
