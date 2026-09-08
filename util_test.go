package main

import "testing"

func TestGtNum(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"32.0 GT/s PCIe", 32.0, true},
		{"2.5GT/s", 2.5, true},
		{"  8 GT/s", 8, true},
		{"", 0, false},
		{"unknown", 0, false},
		{"GT/s 16", 0, false},
	}
	for _, tt := range tests {
		got, ok := gtNum(tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("gtNum(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestNumLT(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"8.0 GT/s", "32.0 GT/s", true},
		{"32.0 GT/s", "32.0 GT/s", false},
		{"32.0 GT/s", "8.0 GT/s", false},
		// Unparseable speeds return false here by design: fail-closed handling
		// for unparseable link speeds lives in checkBDF (missing sysfs attrs are
		// flagged as errors before numLT runs), so numLT staying false is intentional.
		{"unknown", "32.0 GT/s", false},
		{"8.0 GT/s", "unknown", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := numLT(tt.a, tt.b); got != tt.want {
			t.Errorf("numLT(%q, %q) = %v; want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0000:5c:00.0", "0000_5c_00_0"},
	}
	for _, tt := range tests {
		if got := sanitize(tt.in); got != tt.want {
			t.Errorf("sanitize(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestDescOnly(t *testing.T) {
	tests := []struct{ in, want string }{
		{"5c:00.0 PCI bridge: Broadcom / LSI PEX890xx ...", "Broadcom / LSI PEX890xx ..."},
		{"no separator here", "no separator here"},
	}
	for _, tt := range tests {
		if got := descOnly(tt.in); got != tt.want {
			t.Errorf("descOnly(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestOrNA(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "<unavailable>"},
		{"x", "x"},
	}
	for _, tt := range tests {
		if got := orNA(tt.in); got != tt.want {
			t.Errorf("orNA(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestB2i(t *testing.T) {
	tests := []struct {
		in   bool
		want int
	}{
		{true, 1},
		{false, 0},
	}
	for _, tt := range tests {
		if got := b2i(tt.in); got != tt.want {
			t.Errorf("b2i(%v) = %d; want %d", tt.in, got, tt.want)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("GNH_TEST_ENVOR", "custom")
	if got := envOr("GNH_TEST_ENVOR", "default"); got != "custom" {
		t.Errorf("envOr(set) = %q; want %q", got, "custom")
	}
	if got := envOr("GNH_TEST_ENVOR_UNSET", "default"); got != "default" {
		t.Errorf("envOr(unset) = %q; want %q", got, "default")
	}
}
