package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		desc string
		want string
	}{
		{"5c:00.0 PCI bridge: Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0)", "switch"},
		{"d8:00.0 PCI bridge: PLX Technology, Inc. PEX 8747 48-Lane, 5-Port PCI Express Gen 3 (8.0 GT/s) Switch (rev ca)", "switch"},
		{"01:00.0 Non-Volatile memory controller: Micron Technology Inc 7450 PRO NVMe SSD", "nvme"},
		{"02:00.0 Non-Volatile memory controller: Samsung Electronics Co Ltd NVMe SSD Controller PM9A1/PM9A3/980PRO", "nvme"},
		{"e1:00.0 PCI bridge: Mellanox Technologies MT43244 BlueField-3 SoC PCIe bridge (rev 01)", "nvme"},
		{"c1:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)", "gpu"},
		{"a0:00.0 Ethernet controller: Mellanox Technologies MT2910 Family [ConnectX-7]", "nic"},
		{"a1:00.0 Infiniband controller: Mellanox Technologies MT2910 Family [ConnectX-7]", "nic"},
		{"dc:00.0 Ethernet controller: Intel Corporation Ethernet Controller X550", "nic"},
		{"c2:00.0 Bridge: NVIDIA Corporation Device 22a3", "other"},
	}
	for _, tt := range tests {
		if got := classify(tt.desc); got != tt.want {
			t.Errorf("classify(%q) = %q; want %q", tt.desc, got, tt.want)
		}
	}
}

func TestRouteFor(t *testing.T) {
	tests := []struct{ class, want string }{
		{"gpu", "reseat"},
		{"nic", "reseat"},
		{"switch", "switch"},
		{"nvme", "nvme"},
		{"other", "other"},
	}
	for _, tt := range tests {
		if got := routeFor(tt.class); got != tt.want {
			t.Errorf("routeFor(%q) = %q; want %q", tt.class, got, tt.want)
		}
	}
}

// testTriage returns a triage safe for calling verdict() directly: zero-value
// palette (no color) and a logger with nil file (stdout only).
func testTriage(o *options) *triage {
	return &triage{o: o, log: &logger{}, results: map[string]*devResult{}}
}

// addResult registers a minimal devResult so printRMA/printDegraded can index
// t.results[b] for any BDF placed in switchDeg/nvmeDeg/otherDeg.
func addResult(tr *triage, bdf, desc string) {
	tr.results[bdf] = &devResult{
		bdf:    bdf,
		desc:   desc,
		lnksta: "LnkSta: Speed 8GT/s (downgraded), Width x16",
	}
}

func TestVerdict(t *testing.T) {
	const (
		swBDF   = "0000:5c:00.0"
		nvmeBDF = "0000:01:00.0"
		othBDF  = "0000:c1:00.0"
		swDesc  = "5c:00.0 PCI bridge: Broadcom / LSI PEX890xx PCIe Gen 5 Switch"
		nvDesc  = "01:00.0 Non-Volatile memory controller: Micron Technology Inc 7450 PRO NVMe SSD"
		otDesc  = "c1:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB]"
	)
	tests := []struct {
		name string
		mod  func(tr *triage)
		want int
	}{
		{"all clear", func(tr *triage) {}, 0},
		{"switch degraded", func(tr *triage) {
			tr.switchDeg = []string{swBDF}
			addResult(tr, swBDF, swDesc)
		}, 1},
		{"nvme degraded", func(tr *triage) {
			tr.nvmeDeg = []string{nvmeBDF}
			addResult(tr, nvmeBDF, nvDesc)
		}, 1},
		{"other degraded", func(tr *triage) {
			tr.otherDeg = []string{othBDF}
			addResult(tr, othBDF, otDesc)
		}, 1},
		{"remap pending alone", func(tr *triage) { tr.remapPend = true }, 1},
		{"nvlink anomaly alone", func(tr *triage) { tr.nvlinkAnom = true }, 1},
		{"pcie errors", func(tr *triage) {
			tr.pcieErrors = []string{"0000:d8:10.0"}
		}, 2},
		{"uncorrectable ECC", func(tr *triage) { tr.eccUncorr = true }, 2},
		{"remap failure", func(tr *triage) { tr.remapFail = true }, 2},
		{"fatal AER", func(tr *triage) { tr.aerFatal = true }, 2},
		{"post-drain switch still degraded", func(tr *triage) {
			tr.o.postDrain = true
			tr.switchDeg = []string{swBDF}
			addResult(tr, swBDF, swDesc)
		}, 2},
		{"post-drain other still degraded", func(tr *triage) {
			tr.o.postDrain = true
			tr.otherDeg = []string{othBDF}
			addResult(tr, othBDF, otDesc)
		}, 2},
		{"post-swap nvme still degraded", func(tr *triage) {
			tr.o.postSwap = true
			tr.nvmeDeg = []string{nvmeBDF}
			addResult(tr, nvmeBDF, nvDesc)
		}, 2},
		{"reseat endpoint degraded", func(tr *triage) {
			tr.reseatDeg = []string{othBDF}
			addResult(tr, othBDF, otDesc)
		}, 1},
		{"post-reseat still degraded", func(tr *triage) {
			tr.o.postReseat = true
			tr.reseatDeg = []string{othBDF}
			addResult(tr, othBDF, otDesc)
		}, 2},
		{"post-drain but nothing degraded", func(tr *triage) { tr.o.postDrain = true }, 0},
		{"post-reseat but nothing degraded", func(tr *triage) { tr.o.postReseat = true }, 0},
		{"IB fault alone", func(tr *triage) { tr.ibFault = []string{"mlx5_0/port1 1: DOWN"} }, 1},
		{"thermal fault alone", func(tr *triage) {
			tr.thermalFault = []string{"GPU 00000000:C1:00.0: SW Thermal Slowdown Active"}
		}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := testTriage(&options{bmn: "BMN_TEST"})
			tt.mod(tr)
			if got := tr.verdict(); got != tt.want {
				t.Errorf("verdict() = %d; want %d", got, tt.want)
			}
		})
	}
}

func TestPexFalsePositive(t *testing.T) {
	const (
		idleBDF = "0000:5c:00.0"
		clrBDF  = "0000:d8:10.0"
		swDesc  = "5c:00.0 PCI bridge: Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0)"
	)
	cleanGPU := func() *gpuFindings { return &gpuFindings{Expected: 8, CountSMI: 8} }
	newIdle := func() *triage {
		tr := testTriage(&options{bmn: "BMN_TEST", bdfs: []string{idleBDF}})
		tr.gpu = cleanGPU()
		tr.switchDeg = []string{idleBDF}
		tr.linkDown = []string{idleBDF}
		tr.results[idleBDF] = &devResult{
			bdf: idleBDF, desc: swDesc, class: "switch",
			degraded: true, down: true, widthDeg: true, noChild: true, dpcTrig: true,
		}
		return tr
	}

	t.Run("idle-port only classifies and verdicts 0", func(t *testing.T) {
		tr := newIdle()
		idle, cleared, ok := tr.pexFalsePositive()
		if !ok || !reflect.DeepEqual(idle, []string{idleBDF}) || len(cleared) != 0 {
			t.Fatalf("pexFalsePositive() = %v, %v, %v; want [%s], [], true", idle, cleared, ok, idleBDF)
		}
		if got := tr.verdict(); got != 0 {
			t.Errorf("verdict() = %d; want 0 (false positive → return to ready)", got)
		}
	})

	newCleared := func() *triage {
		tr := testTriage(&options{bmn: "BMN_TEST", bdfs: []string{clrBDF}})
		tr.gpu = cleanGPU()
		tr.alertCleared = []string{clrBDF}
		tr.results[clrBDF] = &devResult{bdf: clrBDF, desc: swDesc}
		return tr
	}

	t.Run("self-cleared alert BDF classifies", func(t *testing.T) {
		tr := newCleared()
		idle, cleared, ok := tr.pexFalsePositive()
		if !ok || len(idle) != 0 || !reflect.DeepEqual(cleared, []string{clrBDF}) {
			t.Fatalf("pexFalsePositive() = %v, %v, %v; want [], [%s], true", idle, cleared, ok, clrBDF)
		}
		if got := tr.verdict(); got != 0 {
			t.Errorf("verdict() = %d; want 0", got)
		}
	})

	t.Run("disqualified by retrain noise on self-cleared BDF", func(t *testing.T) {
		tr := newCleared()
		tr.results[clrBDF].retrainNoise = true
		if _, _, ok := tr.pexFalsePositive(); ok {
			t.Error("retrain noise on a self-cleared port means a flapping link — must disqualify")
		}
	})

	t.Run("disqualified by DPC on self-cleared BDF", func(t *testing.T) {
		tr := newCleared()
		tr.results[clrBDF].dpcTrig = true
		if _, _, ok := tr.pexFalsePositive(); ok {
			t.Error("DPC fired on a recovered port is real error history — must disqualify")
		}
	})

	disqualify := []struct {
		name string
		mod  func(tr *triage)
	}{
		{"xid in dmesg", func(tr *triage) { tr.xid = true }},
		{"nvlink anomaly", func(tr *triage) { tr.nvlinkAnom = true }},
		{"remap pending", func(tr *triage) { tr.remapPend = true }},
		{"gpu findings absent", func(tr *triage) { tr.gpu = nil }},
		{"missing GPU", func(tr *triage) { tr.gpu.MissingBDFs = []string{"0000:c1:00.0"} }},
		{"rev ff device", func(tr *triage) { tr.gpu.RevFF = []string{"0000:c1:00.0"} }},
		{"short GPU count", func(tr *triage) { tr.gpu.CountSMI = 7 }},
		{"unreachable device", func(tr *triage) { tr.pcieErrors = []string{"0000:18:02.0"} }},
		{"retrain noise anywhere", func(tr *triage) {
			tr.results["0000:18:02.0"] = &devResult{bdf: "0000:18:02.0", retrainNoise: true}
		}},
		{"wrong-part endpoint anywhere", func(tr *triage) {
			tr.results["0000:18:02.0"] = &devResult{bdf: "0000:18:02.0", childCapBelow: true}
		}},
		{"non-idle degraded device", func(tr *triage) {
			tr.otherDeg = append(tr.otherDeg, "0000:18:02.0")
			tr.results["0000:18:02.0"] = &devResult{bdf: "0000:18:02.0", degraded: true, speedDeg: true}
		}},
		{"post-drain run", func(tr *triage) { tr.o.postDrain = true }},
		{"IB port not ACTIVE", func(tr *triage) { tr.ibFault = []string{"mlx5_0/port1 1: DOWN"} }},
		{"thermal slowdown active", func(tr *triage) {
			tr.thermalFault = []string{"GPU 00000000:C1:00.0: HW Thermal Slowdown Active"}
		}},
	}
	for _, tt := range disqualify {
		t.Run("disqualified by "+tt.name, func(t *testing.T) {
			tr := newIdle()
			tt.mod(tr)
			if _, _, ok := tr.pexFalsePositive(); ok {
				t.Errorf("%s must disqualify the false-positive class", tt.name)
			}
		})
	}
}

func TestDiscoverTargetsUserBDFs(t *testing.T) {
	want := []string{"0000:5c:00.0", "0000:d8:10.0"}
	tr := testTriage(&options{bdfs: want})
	got, ok := tr.discoverTargets()
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("discoverTargets() = %v, %v; want %v, true", got, ok, want)
	}
}

func TestDiscoverTargetsFailClosed(t *testing.T) {
	// Hermetic only where /sys/bus/pci/devices is absent (e.g. macOS/CI
	// containers): the glob is empty, so with no user BDFs discovery must
	// report not-ok instead of falling back to guessed defaults.
	if entries, _ := filepath.Glob("/sys/bus/pci/devices/*"); len(entries) > 0 {
		t.Skip("host exposes real PCI devices; empty-discovery case not hermetic here")
	}
	tr := testTriage(&options{})
	got, ok := tr.discoverTargets()
	if ok || got != nil {
		t.Errorf("discoverTargets() = %v, %v; want nil, false", got, ok)
	}
}
