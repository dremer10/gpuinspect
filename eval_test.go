package main

import (
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestSmiBDF(t *testing.T) {
	tests := []struct{ in, want string }{
		{"00000000:19:00.0", "0000:19:00.0"}, // nvidia-smi 8-digit domain
		{"0000:19:00.0", "0000:19:00.0"},     // already sysfs form
		{"  00000000:DB:00.0 ", "0000:db:00.0"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := smiBDF(tt.in); got != tt.want {
			t.Errorf("smiBDF(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

const evalCSVSample = `0, GPU-aaa, NVIDIA H100 80GB HBM3, 1650000000001, 00000000:19:00.0, P0, 5, 5, 16, 16, 32, 0, 0, 81559, 1, 70.00, 700.00, 345, 2619, Enabled, 0, 0
1, GPU-bbb, NVIDIA H100 80GB HBM3, 1650000000002, 00000000:3B:00.0, P0, 5, 5, 8, 16, 45, 0, 0, 81559, 1, 95.00, 700.00, 1980, 2619, Enabled, 12, 0
garbage line that should be dropped
2, GPU-ccc, NVIDIA H100 80GB HBM3, 1650000000003, 00000000:4C:00.0, P0, 1, 5, 16, 16, 30, 0, 0, 81559, 1, 68.00, 700.00, 210, 2619, Enabled, 0, 0`

func TestParseEvalCSV(t *testing.T) {
	gpus := parseEvalCSV(evalCSVSample)
	if len(gpus) != 3 {
		t.Fatalf("parseEvalCSV: got %d GPUs, want 3", len(gpus))
	}
	want := evalGPU{
		Index: "1", UUID: "GPU-bbb", Name: "NVIDIA H100 80GB HBM3", Serial: "1650000000002",
		BDF: "0000:3b:00.0", PState: "P0", GenCur: "5", GenMax: "5", WidthCur: "8", WidthMax: "16",
		TempC: "45", UtilGPU: "0", UtilMem: "0", MemTotal: "81559", MemUsed: "1",
		PowerW: "95.00", PowerLim: "700.00", SMClock: "1980", MemClock: "2619",
		ECCMode: "Enabled", ECCCorr: "12", ECCUnc: "0",
	}
	if !reflect.DeepEqual(gpus[1], want) {
		t.Errorf("parseEvalCSV row 1\n got: %+v\nwant: %+v", gpus[1], want)
	}
}

func TestEvalWidthMismatches(t *testing.T) {
	gpus := parseEvalCSV(evalCSVSample)
	got := evalWidthMismatches(gpus)
	// Only GPU 1 is x8 < x16; GPU 2's gen 1 < 5 is idle ASPM and must NOT flag.
	want := []string{"GPU 1 (0000:3b:00.0): PCIe width x8 below max x16"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("evalWidthMismatches = %v; want %v", got, want)
	}
}

func TestEvalHotIdle(t *testing.T) {
	gpus := parseEvalCSV(evalCSVSample)
	got := evalHotIdle(gpus)
	// GPU 1: SM 1980 MHz at 0% util. GPU 0 (345) and GPU 2 (210) are under 500.
	if len(got) != 1 || !reflect.DeepEqual(got[0], "GPU 1 (0000:3b:00.0): SM clock 1980 MHz at 0% util (temp 45 C) — hot-idle candidate") {
		t.Errorf("evalHotIdle = %v", got)
	}
}

// runConcurrently must overlap its functions (dmon/pmon wall time = the
// longer window, not the sum) and return only after all have finished.
// Overlap is proven deterministically: fn1 blocks on an unbuffered channel
// that only fn2 fills — any serial execution order deadlocks, caught by the
// timeout guard.
func TestRunConcurrently(t *testing.T) {
	done := make(chan struct{})
	var ran atomic.Int32
	go func() {
		defer close(done)
		ch := make(chan struct{})
		runConcurrently(
			func() { <-ch; ran.Add(1) },
			func() { ch <- struct{}{}; ran.Add(1) },
		)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runConcurrently did not overlap its functions (serial execution would deadlock)")
	}
	if got := ran.Load(); got != 2 {
		t.Errorf("runConcurrently returned before all functions finished: %d of 2 ran", got)
	}
}

func TestDcgmDiagFailed(t *testing.T) {
	pass := `+---------------------------+------------------------------------------------+
| Diagnostic                | Result                                         |
|-----  Deployment  --------+------------------------------------------------|
| Denylist                  | Pass                                           |
| CUDA Main Library         | Pass                                           |
+---------------------------+------------------------------------------------+`
	fail := pass + "\n| PCIe                      | Fail                                           |"
	if dcgmDiagFailed(pass) {
		t.Error("dcgmDiagFailed: all-Pass output flagged as failure")
	}
	if !dcgmDiagFailed(fail) {
		t.Error("dcgmDiagFailed: Fail row not detected")
	}
}
