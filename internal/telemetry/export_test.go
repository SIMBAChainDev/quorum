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
	"testing"

	"github.com/ethereum/go-ethereum/internal/telemetry/provider"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// resetTelemetry returns the package to its pre-Init state so tests do not leak
// pipelines or provider selections into one another.
func resetTelemetry() {
	_ = Shutdown(context.Background())
	provider.Set(provider.Default)
	setExcludedMethods(DefaultExcludedMethods)
}

// isDefaultShutdown reports whether Shutdown is still the declaration-time
// no-op, i.e. whether Init installed a real pipeline.
func isDefaultShutdown() bool {
	return !pipelineInstalled.Load()
}

// installRecorder wires up an in-memory pipeline and selects the OTLP provider,
// so the code under test takes exactly the same paths it would against a real
// collector.
func installRecorder(t *testing.T, ratio float64) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	install(sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(rec),
		sdktrace.WithSampler(sampler(ratio)),
	))
	provider.Set(provider.OTLP)
	setExcludedMethods(DefaultExcludedMethods)
	t.Cleanup(resetTelemetry)
	return rec
}
