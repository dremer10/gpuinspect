package main

import (
	"reflect"
	"testing"
)

const dellH100Line = "3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)"

// 8 healthy Dell XE9680 H100 GPUs.
const lspciDellH100Full = `19:00.0 ` + dellH100Line + `
3b:00.0 ` + dellH100Line + `
4c:00.0 ` + dellH100Line + `
5d:00.0 ` + dellH100Line + `
9b:00.0 ` + dellH100Line + `
bb:00.0 ` + dellH100Line + `
cb:00.0 ` + dellH100Line + `
db:00.0 ` + dellH100Line + `
`

// Real-world failure: cb:00.0 gone entirely (7 GPUs left).
const lspciDellH100MissingCB = `19:00.0 ` + dellH100Line + `
3b:00.0 ` + dellH100Line + `
4c:00.0 ` + dellH100Line + `
5d:00.0 ` + dellH100Line + `
9b:00.0 ` + dellH100Line + `
bb:00.0 ` + dellH100Line + `
db:00.0 ` + dellH100Line + `
`

// Real-world healthy node: BMN s7xg5724 (Dell XE9680, GPU-H100-01) — a
// different XE9680 config whose 8 GPUs live at addresses that do NOT match
// the gpuPlatforms golden set. 8/8 in both nvidia-smi and lspci; must never
// produce "GPU MISSING" alerts.
const lspciDellH100AltAddrs = `1a:00.0 ` + dellH100Line + `
40:00.0 ` + dellH100Line + `
53:00.0 ` + dellH100Line + `
66:00.0 ` + dellH100Line + `
9c:00.0 ` + dellH100Line + `
c0:00.0 ` + dellH100Line + `
d2:00.0 ` + dellH100Line + `
e4:00.0 ` + dellH100Line + `
`

func TestMissingGPUBDFs(t *testing.T) {
	tests := []struct {
		name         string
		manufacturer string
		model        string
		lspci        string
		want         []string
	}{
		{
			name:         "dell h100 all present",
			manufacturer: "Dell Inc.",
			model:        "NVIDIA H100 80GB HBM3",
			lspci:        lspciDellH100Full,
			want:         nil,
		},
		{
			name:         "dell h100 cb:00.0 fell out of lspci",
			manufacturer: "Dell Inc.",
			model:        "GH100 [H100 SXM5 80GB]",
			lspci:        lspciDellH100MissingCB,
			want:         []string{"0000:cb:00.0"},
		},
		{
			name:         "supermicro not in platform table",
			manufacturer: "Supermicro",
			model:        "NVIDIA H100 80GB HBM3",
			lspci:        lspciDellH100MissingCB,
			want:         nil,
		},
		{
			name:         "dell non-h100 not in platform table",
			manufacturer: "Dell Inc.",
			model:        "NVIDIA A100-SXM4-80GB",
			lspci:        lspciDellH100MissingCB,
			want:         nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := missingGPUBDFs(tc.manufacturer, tc.model, tc.lspci)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("missingGPUBDFs(%q, %q) = %v, want %v",
					tc.manufacturer, tc.model, got, tc.want)
			}
		})
	}
}

func TestGateMissingBDFs(t *testing.T) {
	missing := []string{"0000:19:00.0", "0000:3b:00.0"}
	tests := []struct {
		name       string
		expected   int
		countLspci int
		missing    []string
		want       []string
	}{
		{"short count: golden set pins the vanished BDFs", 8, 7, missing, missing},
		{"full count: never second-guess a full complement", 8, 8, missing, nil},
		{"over count: nothing to pin", 8, 9, missing, nil},
		{"unknown expected: gate closed", 0, 7, missing, nil},
		{"short count, nothing missing", 8, 7, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gateMissingBDFs(tc.expected, tc.countLspci, tc.missing); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("gateMissingBDFs(%d, %d, %v) = %v, want %v",
					tc.expected, tc.countLspci, tc.missing, got, tc.want)
			}
		})
	}
}

// The s7xg5724 false-alert regression, end-to-end at the pure level: the
// golden set reports all 8 table BDFs "missing" (address mismatch, not a
// fault), the lspci count is full, so the gate must drop every alert. This
// mirrors exactly what analyzeGPUs computes for that node.
func TestGoldenSetMismatchFullComplement(t *testing.T) {
	manufacturer, model := "Dell Inc.", "NVIDIA H100 80GB HBM3"
	golden := missingGPUBDFs(manufacturer, model, lspciDellH100AltAddrs)
	if len(golden) != 8 {
		t.Fatalf("fixture: golden set should mismatch all 8 BDFs, got %v", golden)
	}
	countL := countLspciNvidiaGPUs(lspciDellH100AltAddrs)
	if countL != 8 {
		t.Fatalf("fixture: want 8 lspci GPUs, got %d", countL)
	}
	expected := expectedGPUCount(8, model)
	if expected != 8 {
		t.Fatalf("fixture: want expected 8, got %d", expected)
	}
	if got := gateMissingBDFs(expected, countL, golden); got != nil {
		t.Errorf("healthy 8/8 node produced MissingBDFs %v; want none (six false GPU-MISSING alerts on s7xg5724)", got)
	}
	// The real-fault path must be untouched: same platform with cb:00.0 gone
	// (7/8) still pins the missing BDF through the gate.
	goldenShort := missingGPUBDFs(manufacturer, model, lspciDellH100MissingCB)
	gotShort := gateMissingBDFs(expectedGPUCount(7, model), countLspciNvidiaGPUs(lspciDellH100MissingCB), goldenShort)
	if want := []string{"0000:cb:00.0"}; !reflect.DeepEqual(gotShort, want) {
		t.Errorf("short-count path: got %v, want %v", gotShort, want)
	}
}

func TestCountLspciNvidiaGPUs(t *testing.T) {
	if got := countLspciNvidiaGPUs(lspciDellH100Full); got != 8 {
		t.Errorf("full fixture: got %d, want 8", got)
	}
	if got := countLspciNvidiaGPUs(lspciDellH100MissingCB); got != 7 {
		t.Errorf("missing-cb fixture: got %d, want 7", got)
	}
	mixed := `00:00.0 Host bridge: Intel Corporation Device 09a2 (rev 01)
19:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)
41:00.0 Ethernet controller: Mellanox Technologies MT2910 Family [ConnectX-7]
b1:00.0 VGA compatible controller: NVIDIA Corporation Device 1234 (rev a1)
`
	if got := countLspciNvidiaGPUs(mixed); got != 2 {
		t.Errorf("mixed fixture: got %d, want 2", got)
	}
}

func TestCountSMIGPUs(t *testing.T) {
	smiL := `GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-aaaa)
GPU 1: NVIDIA H100 80GB HBM3 (UUID: GPU-bbbb)
GPU 2: NVIDIA H100 80GB HBM3 (UUID: GPU-cccc)
`
	if got := countSMIGPUs(smiL); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
	if got := countSMIGPUs(""); got != 0 {
		t.Errorf("empty: got %d, want 0", got)
	}
	if got := countSMIGPUs("NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\n"); got != 0 {
		t.Errorf("driver failure: got %d, want 0", got)
	}
}

func TestGPUModelFromSMI(t *testing.T) {
	smiL := "GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-11111111-2222-3333-4444-555555555555)\n"
	if got := gpuModelFromSMI(smiL); got != "NVIDIA H100 80GB HBM3" {
		t.Errorf("got %q, want %q", got, "NVIDIA H100 80GB HBM3")
	}
	if got := gpuModelFromSMI(""); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
}

func TestGPUModelFromLspci(t *testing.T) {
	if got := gpuModelFromLspci(lspciDellH100Full); got != "GH100 [H100 SXM5 80GB]" {
		t.Errorf("got %q, want %q", got, "GH100 [H100 SXM5 80GB]")
	}
	if got := gpuModelFromLspci("00:00.0 Host bridge: Intel Corporation Device 09a2\n"); got != "" {
		t.Errorf("no nvidia: got %q, want empty", got)
	}
}

func TestRevFFBDFs(t *testing.T) {
	lspci := `19:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)
cb:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev ff)
`
	want := []string{"cb:00.0"}
	if got := revFFBDFs(lspci); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := revFFBDFs(lspciDellH100Full); got != nil {
		t.Errorf("healthy fixture: got %v, want nil", got)
	}
}

func TestParseGPUInventory(t *testing.T) {
	csv := `index, pci.bus_id, serial, uuid, name
0, 00000000:19:00.0, 1650123456789, GPU-aaaa, NVIDIA H100 80GB HBM3
1, 00000000:3B:00.0, 1650123456790, GPU-bbbb, NVIDIA H100 80GB HBM3
`
	got := parseGPUInventory(csv)
	want := []string{
		"0, 00000000:19:00.0, 1650123456789, GPU-aaaa, NVIDIA H100 80GB HBM3",
		"1, 00000000:3B:00.0, 1650123456790, GPU-bbbb, NVIDIA H100 80GB HBM3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := parseGPUInventory(""); got != nil {
		t.Errorf("empty: got %v, want nil", got)
	}
}

func TestExpectedGPUCount(t *testing.T) {
	tests := []struct {
		name     string
		reported int
		model    string
		want     int
	}{
		{"healthy H100 node, allocatable 8", 8, "NVIDIA H100 80GB HBM3", 8},
		{"H100 node with a fallen GPU baked into allocatable", 7, "NVIDIA H100 80GB HBM3", 8},
		{"H100 node, allocatable unknown", 0, "NVIDIA H100 80GB HBM3", 8},
		{"B200 short count floored", 6, "NVIDIA B200", 8},
		{"unknown model trusts allocatable", 4, "NVIDIA A40", 4},
		{"unknown model, unknown allocatable", 0, "", 0},
		// GH200 contains "H200" and GB200 contains "B200" — Grace superchips
		// must NOT be floored to the 8-GPU HGX default (ss900770x4200951
		// false-alerted "only 1/8 GPUs visible" on a healthy 1-GPU GH200).
		{"GH200 superchip trusts allocatable 1", 1, "NVIDIA GH200 480GB", 1},
		{"GH200 superchip, allocatable unknown, floors at 1", 0, "NVIDIA GH200 480GB", 1},
		{"GB200 tray trusts allocatable", 4, "NVIDIA GB200", 4},
		{"GB200 tray, allocatable unknown, floors at 1", 0, "NVIDIA GB200", 1},
	}
	for _, tt := range tests {
		if got := expectedGPUCount(tt.reported, tt.model); got != tt.want {
			t.Errorf("%s: expectedGPUCount(%d, %q) = %d; want %d", tt.name, tt.reported, tt.model, got, tt.want)
		}
	}
}
