package sniproxy

import (
	"slices"
	"testing"
)

func TestAllowlistAllows(t *testing.T) {
	a, err := NewAllowlist([]string{"api.anthropic.com", "Registry.NPMJS.org."})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		want bool
	}{
		{"api.anthropic.com", true},
		{"API.ANTHROPIC.COM", true},
		{"api.anthropic.com.", true},
		{"registry.npmjs.org", true},
		{"anthropic.com", false},
		{"evil-api.anthropic.com", false},
		{"sub.api.anthropic.com", false},
		{"api.anthropic.com.evil.test", false},
		{"xapi.anthropic.com", false},
		{"", false},
		{"api.anthropic.com\x00.evil", false},
	}
	for _, tt := range tests {
		if got := a.Allows(tt.name); got != tt.want {
			t.Errorf("Allows(%q) = %v; want %v", tt.name, got, tt.want)
		}
	}
	if got, want := a.Hosts(), []string{"api.anthropic.com", "registry.npmjs.org"}; !slices.Equal(got, want) {
		t.Errorf("Hosts() = %v; want %v", got, want)
	}
}

func TestNewAllowlistErrors(t *testing.T) {
	if _, err := NewAllowlist(nil); err == nil {
		t.Error("empty allowlist accepted")
	}
	for _, bad := range []string{"*.docker.com", "1.2.3.4", "bad..example.com"} {
		if _, err := NewAllowlist([]string{"ok.example.com", bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
