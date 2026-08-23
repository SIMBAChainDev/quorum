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

	"github.com/ethereum/go-ethereum/internal/telemetry/provider"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// InstallForTesting selects the OTLP provider and installs tp as the
// process-wide pipeline, returning a function that tears both down again.
//
// It exists so that the packages instrumented through this one (rpc, node,
// graphql, ...) can assert on the spans they actually emit, which is not
// possible via Init: that always builds a real OTLP exporter. Production code
// must use Init.
func InstallForTesting(tp *sdktrace.TracerProvider, excludeMethods []string) (restore func()) {
	install(tp)
	setExcludedMethods(excludeMethods)
	provider.Set(provider.OTLP)
	return func() {
		_ = Shutdown(context.Background())
		provider.Set(provider.Default)
		setExcludedMethods(DefaultExcludedMethods)
	}
}
