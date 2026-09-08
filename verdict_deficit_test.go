package main

import "testing"

// A GPU-inspection verdict must never read HEALTHY while GPUs are missing
// from nvidia-smi (driver/NVML fault) or lspci (off the bus).
func TestGPUDeficitGate(t *testing.T) {
	cases := []struct {
		name string
		gpu  *gpuFindings
		want bool
	}{
		{"driver down: smi 0/8, lspci 8/8", &gpuFindings{Expected: 8, CountSMI: 0, CountLspci: 8}, true},
		{"off the bus: smi 7/8, lspci 7/8", &gpuFindings{Expected: 8, CountSMI: 7, CountLspci: 7}, true},
		{"all present", &gpuFindings{Expected: 8, CountSMI: 8, CountLspci: 8}, false},
		{"unknown expectation", &gpuFindings{Expected: 0, CountSMI: 0, CountLspci: 0}, false},
		{"no GPU node", nil, false},
	}
	for _, c := range cases {
		tr := &triage{gpu: c.gpu}
		if got := tr.gpuDeficit(); got != c.want {
			t.Errorf("%s: gpuDeficit() = %v, want %v", c.name, got, c.want)
		}
	}
}
