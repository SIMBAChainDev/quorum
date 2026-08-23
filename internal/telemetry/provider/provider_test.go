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

package provider

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Provider
		wantO bool
	}{
		{"empty falls back to default", "", Datadog, true},
		{"datadog", "datadog", Datadog, true},
		{"otlp", "otlp", OTLP, true},
		{"none", "none", None, true},
		{"mixed case otlp", "OTLP", OTLP, true},
		{"mixed case datadog", "DataDog", Datadog, true},
		{"padded", "  otlp  ", OTLP, true},
		{"garbage", "jaeger", Datadog, false},
		{"typo", "otel", Datadog, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Parse(tt.input)
			if got != tt.want || ok != tt.wantO {
				t.Fatalf("Parse(%q) = (%q, %v), want (%q, %v)", tt.input, got, ok, tt.want, tt.wantO)
			}
		})
	}
}

func TestCurrentDefaultsToDatadog(t *testing.T) {
	if got := Current(); got != Datadog {
		t.Fatalf("Current() = %q, want %q", got, Datadog)
	}
}

func TestSetCurrentRoundTrip(t *testing.T) {
	t.Cleanup(func() { Set(Default) })
	for _, p := range []Provider{OTLP, None, Datadog} {
		Set(p)
		if got := Current(); got != p {
			t.Fatalf("after Set(%q), Current() = %q", p, got)
		}
	}
}
