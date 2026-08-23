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

// Package provider holds the tracing provider selected at startup.
//
// It is deliberately stdlib-only. Leaf packages such as log need to know which
// provider is active in order to emit the right trace-correlation keys, but
// they must not pull in the OpenTelemetry SDK or dd-trace-go; the SDK-bearing
// code lives in the parent internal/telemetry package instead.
package provider

import (
	"strings"
	"sync/atomic"
)

// Provider identifies a tracing backend.
type Provider string

const (
	// Datadog keeps the Orchestrion-woven dd-trace-go tracer running. This is
	// the historical behaviour and the default.
	Datadog Provider = "datadog"
	// OTLP exports spans to an OpenTelemetry collector over gRPC.
	OTLP Provider = "otlp"
	// None disables tracing entirely.
	None Provider = "none"

	// Default is used when no provider is configured, or when an unrecognised
	// value is supplied.
	Default = Datadog
)

var current atomic.Value // Provider

func init() {
	current.Store(Default)
}

// Parse maps a user-supplied value onto a Provider. Parsing is
// case-insensitive and tolerates surrounding whitespace. An empty value selects
// Default.
//
// The second return value is false when the value was not recognised, in which
// case Default is returned; callers are expected to warn rather than silently
// disable tracing.
func Parse(s string) (Provider, bool) {
	switch Provider(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return Default, true
	case Datadog:
		return Datadog, true
	case OTLP:
		return OTLP, true
	case None:
		return None, true
	default:
		return Default, false
	}
}

// Set records the active provider. It is called once during startup.
func Set(p Provider) {
	current.Store(p)
}

// Current reports the active provider. It is safe for concurrent use and
// returns Default until Set has been called.
func Current() Provider {
	if p, ok := current.Load().(Provider); ok {
		return p
	}
	return Default
}
