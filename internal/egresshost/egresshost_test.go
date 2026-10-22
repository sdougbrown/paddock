package egresshost

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		host    string
		wantErr string
	}{
		{"api.anthropic.com", ""},
		{"API.Anthropic.COM.", ""},
		{"registry", ""},
		{"_dmarc.example.com", ""},
		{"*.docker.com", "wildcards are not supported"},
		{"*", "wildcards are not supported"},
		{"a.*.example.com", "wildcards are not supported"},
		{"api?.example.com", "wildcards are not supported"},
		{"[2001:db8::1]", "not a hostname"},
		{"1.2.3.4", "IP addresses"},
		{"::1", "IP addresses"},
		{"localhost", "reserved name"},
		{"db.localhost", "reserved name"},
		{"paddock-lifeline-denied.invalid", "reserved name"},
		{"", "not a hostname"},
		{"bad..example.com", "not a hostname"},
		{"spa ce.example.com", "not a hostname"},
		{"x/y", "not a hostname"},
		{"x.example.com:443", "not a hostname"},
		{strings.Repeat("a", 64) + ".com", "not a hostname"},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			err := Validate(tt.host)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v; want containing %q", err, tt.wantErr)
			}
		})
	}
}
