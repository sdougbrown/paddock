package envname

import "testing"

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"A": true, "_x1": true, "FOO_BAR": true,
		"": false, "1A": false, "A-B": false, "A B": false, "A=": false, "é": false,
	} {
		if got := Valid(s); got != want {
			t.Errorf("Valid(%q) = %v, want %v", s, got, want)
		}
	}
}
