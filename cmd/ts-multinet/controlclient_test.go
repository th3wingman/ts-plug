package main

import "testing"

func TestParseOnOff(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
		fail bool
	}{
		{"", true, false},
		{"on", true, false},
		{"ON", true, false},
		{"off", false, false},
		{"Off", false, false},
		{"nope", false, true},
	} {
		got, err := parseOnOff(tt.in)
		if tt.fail {
			if err == nil {
				t.Errorf("parseOnOff(%q): expected error, got %v", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("parseOnOff(%q) = %v, %v; want %v, nil", tt.in, got, err, tt.want)
		}
	}
}
