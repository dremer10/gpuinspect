package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestThermalEvents(t *testing.T) {
	q := `
GPU 00000000:19:00.0
    Clocks Event Reasons
        Idle                              : Active
        SW Thermal Slowdown               : Not Active
        HW Thermal Slowdown               : Not Active
        HW Power Brake Slowdown           : Active
GPU 00000000:C1:00.0
    Clocks Event Reasons
        SW Thermal Slowdown               : Active
        HW Thermal Slowdown               : Not Active
`
	want := []string{"GPU 00000000:C1:00.0: SW Thermal Slowdown Active"}
	if got := thermalEvents(q); !reflect.DeepEqual(got, want) {
		t.Errorf("thermalEvents() = %v; want %v (power brake and Not Active must not match)", got, want)
	}
	if got := thermalEvents(""); got != nil {
		t.Errorf("thermalEvents(empty) = %v; want nil", got)
	}
}

func TestNvlinkAnomalous(t *testing.T) {
	// Real GH200 ss900770x4200951 shape: one GPU, no NVLink fabric at all —
	// must NOT be an anomaly (used to false-flag "NVLink inactive/down links").
	gh200 := `GPU 0: NVIDIA GH200 480GB (UUID: GPU-93364fc3-0daa-b578-a890-499627dac53b)
NVML: Unable to retrieve Nvlink information as all links are inActive`
	if nvlinkAnomalous(gh200) {
		t.Error("single-GPU all-inactive node must not be anomalous")
	}
	// The same NVML message on a multi-GPU node means the fabric is down.
	multi := `GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-aaa)
NVML: Unable to retrieve Nvlink information as all links are inActive
GPU 1: NVIDIA H100 80GB HBM3 (UUID: GPU-bbb)
NVML: Unable to retrieve Nvlink information as all links are inActive`
	if !nvlinkAnomalous(multi) {
		t.Error("multi-GPU all-inactive node must be anomalous")
	}
	// Per-link inactive/down lines are anomalous regardless of GPU count.
	perLink := `GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-aaa)
	 Link 0: 26.562 GB/s
	 Link 1: <inactive>`
	if !nvlinkAnomalous(perLink) {
		t.Error("per-link inactive must be anomalous even on one GPU")
	}
	healthy := `GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-aaa)
	 Link 0: 26.562 GB/s
	 Link 1: 26.562 GB/s`
	if nvlinkAnomalous(healthy) {
		t.Error("healthy links must not be anomalous")
	}
	if nvlinkAnomalous("") {
		t.Error("empty output must not be anomalous")
	}
}

func TestVolatileUncorrNonzero(t *testing.T) {
	// Shape taken from a real H100 nvidia-smi -q: aggregate lifetime counters
	// are nonzero while volatile is clean — must NOT flag.
	aggregateOnly := `
    ECC Mode
        Current                                   : Enabled
    ECC Errors
        Volatile
            SRAM Uncorrectable Parity                  : 0
            SRAM Uncorrectable SEC-DED                 : 0
            DRAM Uncorrectable                         : 0
        Aggregate
            SRAM Uncorrectable Parity                  : 2
            SRAM Uncorrectable SEC-DED                 : 0
            DRAM Uncorrectable                         : 5
        Aggregate Uncorrectable SRAM Sources
            SRAM L2                                    : 0
`
	volatileHit := `
    ECC Errors
        Volatile
            SRAM Uncorrectable Parity                  : 0
            DRAM Uncorrectable                         : 3
        Aggregate
            DRAM Uncorrectable                         : 7
`
	secondGPUVolatile := `
    ECC Errors
        Volatile
            DRAM Uncorrectable                         : 0
        Aggregate
            DRAM Uncorrectable                         : 0
GPU 00000000:BB:00.0
    ECC Errors
        Volatile
            DRAM Uncorrectable                         : 1
`
	tests := []struct {
		name string
		q    string
		want bool
	}{
		{"aggregate-only history", aggregateOnly, false},
		{"volatile nonzero", volatileHit, true},
		{"volatile on second GPU", secondGPUVolatile, true},
		{"empty", "", false},
	}
	for _, tt := range tests {
		if got := volatileUncorrNonzero(tt.q); got != tt.want {
			t.Errorf("%s: volatileUncorrNonzero() = %v; want %v", tt.name, got, tt.want)
		}
	}
}

// The three nvidia-smi -q derived sections must grep the already-captured
// nvidia_smi_q.txt instead of re-invoking `nvidia-smi -q` (~2s each), with
// the displayed command line being the command actually run; and the section
// order / titles (the gpu_full.txt "===== section =====" layout) must stay
// exactly as parseGPUHealth-era tooling knows them.
func TestGPUSuiteSections(t *testing.T) {
	qFile := "/some/bundle/nvidia_smi_q.txt"
	sections := gpuSuiteSections(qFile)

	wantTitles := []string{
		"nvidia-smi", "list-gpus", "q grep state", "ecc counters", "ecc nonzero",
		"gpu inventory (module id)", "gpu inventory", "remapped rows",
		"topo matrix", "p2p", "nvlink status", "nvlink anomalies", "nvlink -R",
	}
	var gotTitles []string
	byTitle := map[string]gpuSuiteSection{}
	for _, s := range sections {
		gotTitles = append(gotTitles, s.title)
		byTitle[s.title] = s
	}
	if !reflect.DeepEqual(gotTitles, wantTitles) {
		t.Errorf("section order changed:\n got %v\nwant %v", gotTitles, wantTitles)
	}

	for _, title := range []string{"q grep state", "ecc counters", "ecc nonzero"} {
		s, ok := byTitle[title]
		if !ok {
			t.Fatalf("section %q missing", title)
		}
		if !strings.Contains(s.script, qFile) {
			t.Errorf("%q does not grep the captured q file: %q", title, s.script)
		}
		if strings.Contains(s.script, "nvidia-smi -q") {
			t.Errorf("%q still re-invokes nvidia-smi -q: %q", title, s.script)
		}
	}
	// ecc nonzero keeps its nonzero filter on top of the ecc grep.
	if s := byTitle["ecc nonzero"]; !strings.Contains(s.script, `grep -vE " : 0$"`) {
		t.Errorf("ecc nonzero lost its nonzero filter: %q", s.script)
	}
}

// runQuietCaptures: captures execute concurrently but every file is written
// and every line count recorded before the report lines print — and the
// report lines print in input order even when the first capture finishes
// last (EXECUTE CONCURRENTLY, PRINT SEQUENTIALLY).
func TestRunQuietCaptures(t *testing.T) {
	dir := t.TempDir()
	logFile, err := os.Create(filepath.Join(dir, "triage.log"))
	if err != nil {
		t.Fatal(err)
	}
	tr := &triage{c: palette{}, log: &logger{file: logFile}}

	caps := []quietCapture{
		{path: filepath.Join(dir, "a.txt"), script: `sleep 0.3; printf 'a1\na2\n'`},
		{path: filepath.Join(dir, "b.txt"), script: `printf 'b1\n'`},
		{path: filepath.Join(dir, "c.txt"), script: `printf 'c1\nc2\nc3\n'`},
	}
	tr.runQuietCaptures(caps)
	_ = logFile.Close()

	if b, err := os.ReadFile(caps[0].path); err != nil || string(b) != "a1\na2\n" {
		t.Errorf("a.txt = %q, %v; want %q", b, err, "a1\na2\n")
	}
	// Line counts match the legacy bashQuiet accounting (split on \n, so a
	// trailing newline counts one empty element).
	for i, want := range []int{3, 2, 4} {
		if caps[i].lines != want {
			t.Errorf("caps[%d].lines = %d; want %d", i, caps[i].lines, want)
		}
	}
	logBytes, err := os.ReadFile(filepath.Join(dir, "triage.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	ia, ib, ic := strings.Index(log, "a.txt"), strings.Index(log, "b.txt"), strings.Index(log, "c.txt")
	if ia < 0 || ib < 0 || ic < 0 {
		t.Fatalf("captured-to lines missing from log:\n%s", log)
	}
	if !(ia < ib && ib < ic) {
		t.Errorf("report lines out of input order (a=%d b=%d c=%d):\n%s", ia, ib, ic, log)
	}

	// Single capture takes the inline bashQuiet path — same file + report line.
	single := []quietCapture{{path: filepath.Join(dir, "d.txt"), script: `printf 'd1\n'`}}
	tr.runQuietCaptures(single)
	if b, err := os.ReadFile(single[0].path); err != nil || string(b) != "d1\n" {
		t.Errorf("single capture d.txt = %q, %v; want %q", b, err, "d1\n")
	}
}

func TestXidLinesFromDmesg(t *testing.T) {
	dmesg := `[Mon Aug  3 10:00:00 2026] pcieport 0000:53:10.0: DPC: containment event
[Mon Aug  3 10:00:01 2026] NVRM: Xid (PCI:0000:9b:00): 79, pid=1234, GPU has fallen off the bus.
[Mon Aug  3 10:00:02 2026] usb 1-1: new device
[Mon Aug  3 10:00:03 2026] NVRM: Xid (PCI:0000:9b:00): 154, pid=1234, GPU recovery action changed
`
	got := xidLinesFromDmesg(dmesg)
	want := []string{
		"[Mon Aug  3 10:00:01 2026] NVRM: Xid (PCI:0000:9b:00): 79, pid=1234, GPU has fallen off the bus.",
		"[Mon Aug  3 10:00:03 2026] NVRM: Xid (PCI:0000:9b:00): 154, pid=1234, GPU recovery action changed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("xidLinesFromDmesg() = %v; want %v", got, want)
	}
	if got := xidLinesFromDmesg(""); got != nil {
		t.Errorf("xidLinesFromDmesg(empty) = %v; want nil", got)
	}
	// Cap at the most recent 20 lines.
	var many string
	for i := 0; i < 30; i++ {
		many += "NVRM: Xid (PCI:0000:19:00): 13, line\n"
	}
	if got := xidLinesFromDmesg(many); len(got) != 20 {
		t.Errorf("xidLinesFromDmesg cap: got %d lines, want 20", len(got))
	}
}
