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
	"time"

	"github.com/ethereum/go-ethereum/internal/telemetry/provider"
)

// TestShutdownBeforeInit covers the clef path: cmd/clef calls debug.Exit() on a
// repeated SIGINT but never calls debug.Setup(), so Shutdown is reachable
// without Init ever having run. A nil shutdown func there would turn clef's
// diagnostic dump into a nil-pointer panic.
func TestShutdownBeforeInit(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Shutdown panicked before Init: %v", r)
		}
	}()
	if err := Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Init returned %v, want nil", err)
	}
	// Second call must be just as safe.
	if err := Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown returned %v, want nil", err)
	}
}

func TestInitProviderSelection(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  provider.Provider
	}{
		{"empty defaults to datadog", "", provider.Datadog},
		{"datadog", "datadog", provider.Datadog},
		{"none", "none", provider.None},
		{"mixed case", "NoNe", provider.None},
		{"garbage falls back to datadog", "not-a-provider", provider.Datadog},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(resetTelemetry)
			cfg := DefaultConfig()
			cfg.Provider = tt.input
			Init(cfg)
			if got := provider.Current(); got != tt.want {
				t.Fatalf("after Init(%q), provider.Current() = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestInitUnreachableEndpoint asserts that a dead or bogus collector cannot stop
// a node from starting: otlptracegrpc dials lazily, so Init must return
// promptly, leave a usable Shutdown behind, and not panic.
func TestInitUnreachableEndpoint(t *testing.T) {
	t.Cleanup(resetTelemetry)

	cfg := DefaultConfig()
	cfg.Provider = string(provider.OTLP)
	// Port 1 on the loopback interface: nothing is listening, and nothing will.
	cfg.Endpoint = "127.0.0.1:1"

	done := make(chan struct{})
	go func() {
		defer close(done)
		Init(cfg)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Init blocked on an unreachable collector")
	}

	if got := provider.Current(); got != provider.OTLP {
		t.Fatalf("provider.Current() = %q, want %q", got, provider.OTLP)
	}
	// The pipeline is installed, so Shutdown must now be doing real work rather
	// than remaining the declaration-time no-op.
	if isDefaultShutdown() {
		t.Fatal("Init left the default no-op shutdown in place")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Flushing to a dead collector may error; it must not panic or hang.
	_ = Shutdown(ctx)
}

// TestInitInvalidEndpointDegrades checks the "warn and carry on" contract: an
// endpoint the exporter cannot even parse must leave tracing disabled rather
// than aborting startup.
func TestInitInvalidEndpointDegrades(t *testing.T) {
	t.Cleanup(resetTelemetry)

	cfg := DefaultConfig()
	cfg.Provider = string(provider.OTLP)
	cfg.Endpoint = "://not a url at all"

	Init(cfg)

	if err := Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after a failed Init returned %v, want nil", err)
	}
}

func TestShouldTraceDenyList(t *testing.T) {
	t.Cleanup(resetTelemetry)
	setExcludedMethods(DefaultExcludedMethods)

	tests := []struct {
		method string
		want   bool
	}{
		{"eth_getBalance", true},
		{"eth_sendTransaction", true},
		{"eth_blockNumber", false},
		{"eth_syncing", false},
		{"eth_chainId", false},
		{"net_version", false},
		{"eth_gasPrice", false},
		{"web3_clientVersion", false},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			if got := shouldTrace(tt.method); got != tt.want {
				t.Fatalf("shouldTrace(%q) = %v, want %v", tt.method, got, tt.want)
			}
		})
	}
}

func TestSetExcludedMethodsTrimsAndDropsEmpties(t *testing.T) {
	t.Cleanup(resetTelemetry)
	setExcludedMethods([]string{" eth_syncing ", "", "  "})

	if shouldTrace("eth_syncing") {
		t.Fatal("shouldTrace(eth_syncing) = true, want false after a padded deny-list entry")
	}
	if !shouldTrace("") {
		t.Fatal(`shouldTrace("") = false; empty deny-list entries must be dropped`)
	}
}

func TestClampRatio(t *testing.T) {
	tests := []struct {
		in, want float64
	}{
		{-1, 0}, {0, 0}, {0.01, 0.01}, {1, 1}, {2, 1},
	}
	for _, tt := range tests {
		if got := clampRatio(tt.in); got != tt.want {
			t.Fatalf("clampRatio(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestExporterOptions(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantLen  int
	}{
		{"empty leaves it to the environment", "", 0},
		{"bare host:port is plaintext", "localhost:4317", 2},
		{"url keeps its scheme", "https://collector:4317", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(exporterOptions(tt.endpoint)); got != tt.wantLen {
				t.Fatalf("len(exporterOptions(%q)) = %d, want %d", tt.endpoint, got, tt.wantLen)
			}
		})
	}
}
