package main

import (
	"strings"
	"testing"
)

const tempsCSVFixture = `0, 00000000:1B:00.0, NVIDIA H100 80GB HBM3, 36, 52, N/A, 71.23, 700.00, 0, 345, Not Active, Not Active
7, 00000000:DB:00.0, NVIDIA H100 80GB HBM3, 88, 94, N/A, 320.50, 700.00, 97, 1980, Active, Not Active`

const tempsQFixture = `==============NVSMI LOG==============

Timestamp                                 : Mon Aug  4 12:00:00 2026
Driver Version                            : 550.90.07

Attached GPUs                             : 2
GPU 00000000:1B:00.0
    Temperature
        GPU Current Temp                  : 36 C
        GPU T.Limit Temp                  : N/A
        GPU Shutdown Temp                 : 92 C
        GPU Slowdown Temp                 : 89 C
        GPU Max Operating Temp            : 87 C
        GPU Target Temperature            : N/A
        Memory Current Temp               : 52 C
        Memory Max Operating Temp         : 95 C

GPU 00000000:DB:00.0
    Temperature
        GPU Current Temp                  : 88 C
        GPU Shutdown Temp                 : 92 C
        GPU Slowdown Temp                 : 89 C
        GPU Max Operating Temp            : 87 C
        Memory Current Temp               : 94 C
        Memory Max Operating Temp         : 95 C
`

func TestParseTempsCSV(t *testing.T) {
	gpus := parseTempsCSV(tempsCSVFixture)
	if len(gpus) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(gpus))
	}
	g := gpus[1]
	if g.Index != "7" || g.BDF != "0000:db:00.0" || g.TempGPU != "88" || g.TempMem != "94" {
		t.Fatalf("GPU7 parsed wrong: %+v", g)
	}
	if g.HWTherm != "Active" || g.SWTherm != "Not Active" {
		t.Fatalf("GPU7 throttle bits parsed wrong: %+v", g)
	}
	// short rows are dropped, not mis-parsed
	if got := parseTempsCSV("0, 00000000:1B:00.0, broken row"); len(got) != 0 {
		t.Fatalf("short row not dropped: %+v", got)
	}
}

func TestParseTempLimits(t *testing.T) {
	m := parseTempLimits(tempsQFixture)
	if len(m) != 2 {
		t.Fatalf("want 2 GPUs, got %d: %v", len(m), m)
	}
	l := m["0000:1b:00.0"]
	if l.Slowdown != 89 || l.Shutdown != 92 || l.MaxOp != 87 || l.MemMaxOp != 95 {
		t.Fatalf("limits parsed wrong: %+v", l)
	}
	// N/A fields stay -1
	na := parseTempLimits("GPU 00000000:2A:00.0\n        GPU Slowdown Temp                 : N/A\n")
	if got := na["0000:2a:00.0"].Slowdown; got != -1 {
		t.Fatalf("N/A slowdown should be -1, got %d", got)
	}
}

// Real H100 s7xg5724 shape: driver reports T.Limit (headroom) thresholds and
// N/A-free absolute lines are absent entirely.
const tempsQTLimitFixture = `GPU 00000000:1A:00.0
    Temperature
        GPU Current Temp                               : 29 C
        GPU T.Limit Temp                               : 58 C
        GPU Shutdown T.Limit Temp                      : -8 C
        GPU Slowdown T.Limit Temp                      : -2 C
        GPU Max Operating T.Limit Temp                 : 0 C
        GPU Target Temperature                         : N/A
        Memory Current Temp                            : 35 C
        Memory Max Operating T.Limit Temp              : 0 C
`

func TestParseTempLimitsTLimit(t *testing.T) {
	m := parseTempLimits(tempsQTLimitFixture)
	l := m["0000:1a:00.0"]
	if !l.TLOK || l.SlowTL != -2 || l.MaxOpTL != 0 {
		t.Fatalf("T.Limit thresholds parsed wrong: %+v", l)
	}
	// absolute thresholds must stay unknown — "GPU Slowdown T.Limit Temp"
	// must not be misread as "GPU Slowdown Temp"
	if l.Slowdown != -1 || l.MaxOp != -1 {
		t.Fatalf("absolute thresholds should be unknown on a T.Limit driver: %+v", l)
	}
	// "Memory Max Operating T.Limit Temp" is headroom, not an absolute HBM cap
	if l.MemMaxOp != -1 {
		t.Fatalf("MemMaxOp should be unknown on a T.Limit driver, got %d", l.MemMaxOp)
	}
}

func TestTempsFaultsTLimit(t *testing.T) {
	gpus := parseTempsCSV("0, 00000000:1A:00.0, H100, 29, 35, N/A, 120, 700, 0, 345, Not Active, Not Active")
	limits := parseTempLimits(tempsQTLimitFixture)

	// healthy: plenty of headroom
	if got := tempsFaults(gpus, map[string]int{"0": 33}, map[string]int{"0": 38}, map[string]int{"0": 55}, limits, 1); len(got) != 0 {
		t.Fatalf("healthy T.Limit GPU faulted: %v", got)
	}
	// headroom collapsed to 0 → max-operating T.Limit violation
	got := tempsFaults(gpus, map[string]int{"0": 85}, map[string]int{"0": 60}, map[string]int{"0": 0}, limits, 1)
	if len(got) != 1 || !strings.Contains(got[0], "at/below max-operating T.Limit 0 C") {
		t.Fatalf("want max-operating T.Limit fault, got %v", got)
	}
	// headroom at -2 → slowdown-level fault wins
	got = tempsFaults(gpus, map[string]int{"0": 88}, map[string]int{"0": 60}, map[string]int{"0": -2}, limits, 1)
	if len(got) != 1 || !strings.Contains(got[0], "at/below SLOWDOWN T.Limit -2 C") {
		t.Fatalf("want slowdown T.Limit fault, got %v", got)
	}
	// no headroom reported (old driver mixing) → no TL fault
	if got := tempsFaults(gpus, map[string]int{"0": 85}, map[string]int{"0": 60}, map[string]int{}, limits, 1); len(got) != 0 {
		t.Fatalf("no-headroom GPU must not TL-fault: %v", got)
	}
}

func TestTempsFaults(t *testing.T) {
	gpus := parseTempsCSV(tempsCSVFixture)
	limits := parseTempLimits(tempsQFixture)
	maxGPU := map[string]int{"0": 41, "7": 88}
	maxMem := map[string]int{"0": 55, "7": 94}

	faults := tempsFaults(gpus, maxGPU, maxMem, nil, limits, 8)
	joined := strings.Join(faults, "\n")
	for _, want := range []string{
		"GPU 7 (0000:db:00.0): HW thermal slowdown ACTIVE",
		"core 88 C at/above max operating 87 C",
		"only 2/8 GPUs visible",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing fault %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "GPU 0 (") {
		t.Fatalf("cool GPU0 must not fault:\n%s", joined)
	}

	// healthy: cool GPUs, full complement, no throttle
	cool := parseTempsCSV("0, 00000000:1B:00.0, H100, 36, 52, N/A, 71, 700, 0, 345, Not Active, Not Active")
	if got := tempsFaults(cool, map[string]int{"0": 44}, map[string]int{"0": 60}, nil, limits, 1); len(got) != 0 {
		t.Fatalf("healthy GPU faulted: %v", got)
	}
	// slowdown threshold beats max-operating in the message
	hotter := tempsFaults(cool, map[string]int{"0": 90}, map[string]int{"0": 60}, nil, limits, 1)
	if len(hotter) != 1 || !strings.Contains(hotter[0], "SLOWDOWN threshold 89") {
		t.Fatalf("want slowdown fault, got %v", hotter)
	}
}

func TestHot(t *testing.T) {
	lim := tempLimits{Slowdown: 89, Shutdown: 92, MaxOp: 87, MemMaxOp: 95}
	for _, tc := range []struct {
		core, mem int
		want      bool
	}{
		{36, 52, false},
		{87, 52, true},  // core at max operating
		{86, 95, true},  // HBM at memory max
		{86, 94, false}, // just under both
	} {
		if got := hot(tc.core, tc.mem, 0, false, lim); got != tc.want {
			t.Fatalf("hot(%d,%d)=%v want %v", tc.core, tc.mem, got, tc.want)
		}
	}
	// unknown thresholds never flag
	if hot(99, 99, 0, false, tempLimits{Slowdown: -1, MaxOp: -1, MemMaxOp: -1}) {
		t.Fatal("unknown thresholds must not flag")
	}
	// T.Limit: headroom at/below max-operating flags, healthy headroom doesn't
	tlim := tempLimits{Slowdown: -1, MaxOp: -1, MemMaxOp: -1, SlowTL: -2, MaxOpTL: 0, TLOK: true}
	if !hot(85, 60, 0, true, tlim) {
		t.Fatal("headroom 0 must flag on a T.Limit driver")
	}
	if hot(85, 60, 55, true, tlim) {
		t.Fatal("headroom 55 must not flag")
	}
	if hot(85, 60, 0, false, tlim) {
		t.Fatal("unreported headroom must not flag")
	}
}

func TestParseArgsTemps(t *testing.T) {
	o, err := parseArgs([]string{"s7xg5724", "--temps"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.temps || !o.noAI || o.nodebot {
		t.Fatalf("--temps must imply no-ai + no-nodebot: %+v", o)
	}
	if _, err := parseArgs([]string{"s7xg5724", "--temps", "--eval"}); err == nil {
		t.Fatal("--temps --eval must be rejected")
	}
}
