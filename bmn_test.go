package main

import (
	"reflect"
	"strings"
	"testing"
)

// Realistic BMN fixture: three qualifying alert conditions (deliberately out
// of chronological order to exercise the sort), one status=False condition
// that must be dropped, and a NodePCILinkSpeedUnexpected with a short-form
// BDF that normalizes to the same address as the width alert (dedup case).
const bmnFixture = `{
  "metadata": {
    "name": "abc123-h100",
    "labels": {
      "ds.coreweave.com/physical-topology.zone": "RNO2A",
      "ib.coreweave.cloud/fabric": "RNO2-FAB7",
      "node.coreweave.cloud/node-fwbundle.current": "h100-dell-2024.10.1",
      "node.coreweave.cloud/node-fwbundle.target": "h100-dell-2024.10.1",
      "node.coreweave.cloud/dpu-fwbundle.current": "bf3-2024.08.2"
    }
  },
  "spec": {
    "firmware": { "nodeBundle": "h100-dell-2024.10.1" }
  },
  "status": {
    "serial": "SN12345XY",
    "deviceSlot": "R12-U30",
    "sku": { "cwSku": "H100_SXM5_DELL" },
    "health": { "online": true },
    "flcc": { "state": "triage" },
    "reportedNodeInfo": {
      "nodeName": "g8f2ac001122",
      "status": {
        "allocatable": { "nvidia.com/gpu": "8" },
        "conditions": [
          {
            "status": "True",
            "reason": "Alert triggered NodePCILinkSpeedUnexpected",
            "message": "PCI link speed for PEX890xx PCIe Gen 5 Switch at 5C:00.0 is unexpected.",
            "lastTransitionTime": "2026-07-21T11:00:00Z"
          },
          {
            "status": "True",
            "reason": "Alert triggered NodePCILinkWidthUnexpected",
            "message": "PCI link width for PEX890xx PCIe Gen 5 Switch at 0000:5c:00.0 is unexpected.",
            "lastTransitionTime": "2026-07-20T10:00:00Z"
          },
          {
            "status": "False",
            "reason": "Alert triggered NodePCILinkWidthUnexpected",
            "message": "resolved, must be ignored at 0000:aa:00.0 is unexpected.",
            "lastTransitionTime": "2026-07-22T12:00:00Z"
          },
          {
            "status": "True",
            "reason": "Alert triggered KubeNodeNotReady",
            "message": "Node abc123-h100 is not ready.",
            "lastTransitionTime": "2026-07-19T09:00:00Z"
          }
        ]
      }
    }
  }
}`

func TestParseBMN(t *testing.T) {
	got, err := parseBMN([]byte(bmnFixture))
	if err != nil {
		t.Fatalf("parseBMN() error = %v", err)
	}
	want := &bmnInfo{
		Name:         "abc123-h100",
		GMAC:         "g8f2ac001122",
		Serial:       "SN12345XY",
		DeviceSlot:   "R12-U30",
		SKU:          "H100_SXM5_DELL",
		State:        "triage",
		Zone:         "RNO2A",
		Region:       "RNO2",
		Fabric:       "RNO2-FAB7",
		Online:       "true",
		ExpectedGPUs: 8,
		Alerts: []bmnAlert{
			{
				Name:           "KubeNodeNotReady",
				BDF:            "",
				Message:        "Node abc123-h100 is not ready.",
				LastTransition: "2026-07-19T09:00:00Z",
			},
			{
				Name:           "NodePCILinkWidthUnexpected",
				BDF:            "0000:5c:00.0",
				Message:        "PCI link width for PEX890xx PCIe Gen 5 Switch at 0000:5c:00.0 is unexpected.",
				LastTransition: "2026-07-20T10:00:00Z",
			},
			{
				Name:           "NodePCILinkSpeedUnexpected",
				BDF:            "0000:5c:00.0",
				Message:        "PCI link speed for PEX890xx PCIe Gen 5 Switch at 5C:00.0 is unexpected.",
				LastTransition: "2026-07-21T11:00:00Z",
			},
		},
		PCIAlertBDFs:     []string{"0000:5c:00.0"},
		BundleCurrent:    "h100-dell-2024.10.1",
		BundleTarget:     "h100-dell-2024.10.1",
		BundleSpec:       "h100-dell-2024.10.1",
		DPUBundleCurrent: "bf3-2024.08.2",
		BundleFlags:      nil, // current==target, no legacy alert names
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBMN() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseBMNSparse(t *testing.T) {
	// No labels, no health.online, no allocatable: everything degrades to
	// zero values, Online to "-", Region to "".
	got, err := parseBMN([]byte(`{"metadata":{"name":"bare"},"status":{}}`))
	if err != nil {
		t.Fatalf("parseBMN() error = %v", err)
	}
	if got.Name != "bare" || got.Online != "-" || got.Region != "" || got.ExpectedGPUs != 0 {
		t.Errorf("sparse parse: Name=%q Online=%q Region=%q ExpectedGPUs=%d",
			got.Name, got.Online, got.Region, got.ExpectedGPUs)
	}
	// Unset node-fwbundle.current must be flagged.
	if len(got.BundleFlags) != 1 || !strings.Contains(got.BundleFlags[0], "UNSET") {
		t.Errorf("sparse parse BundleFlags = %v; want single UNSET flag", got.BundleFlags)
	}
}

// Real shape from ss892297x4304555 (2026-08-13): node mid-RMA-workflow with
// fieldiag running — the class of node where nvidia-smi blocks and gpuinspect
// used to hang silently.
func TestParseBMNFieldDiagWorkflow(t *testing.T) {
	got, err := parseBMN([]byte(`{"metadata":{"name":"fd"},"status":{"flcc":{"state":"fail","workflow":"node-update","workflowStep":"fielddiag"}}}`))
	if err != nil {
		t.Fatalf("parseBMN() error = %v", err)
	}
	if got.Workflow != "node-update" || got.WorkflowStep != "fielddiag" {
		t.Errorf("Workflow=%q WorkflowStep=%q; want node-update / fielddiag", got.Workflow, got.WorkflowStep)
	}
	if !got.fieldDiagActive() {
		t.Error("fieldDiagActive() = false; want true")
	}
	if w := got.fieldDiagWarning(); !strings.Contains(w, "nvidia-smi") || !strings.Contains(w, "fielddiag") {
		t.Errorf("fieldDiagWarning() = %q; want mention of nvidia-smi and fielddiag", w)
	}
}

func TestFieldDiagActive(t *testing.T) {
	cases := []struct {
		step string
		want bool
	}{
		{"fielddiag", true},
		{"fielddiag-reboot", true},
		{"FieldDiag", true}, // case-insensitive
		{"test", false},
		{"", false},
	}
	for _, tc := range cases {
		info := &bmnInfo{WorkflowStep: tc.step}
		if got := info.fieldDiagActive(); got != tc.want {
			t.Errorf("fieldDiagActive(%q) = %v; want %v", tc.step, got, tc.want)
		}
		if warn := info.fieldDiagWarning(); (warn != "") != tc.want {
			t.Errorf("fieldDiagWarning(%q) = %q; want non-empty=%v", tc.step, warn, tc.want)
		}
	}
}

func TestParseBMNBadJSON(t *testing.T) {
	if _, err := parseBMN([]byte("not json")); err == nil {
		t.Error("parseBMN(garbage) = nil error; want error")
	}
}

func TestBundleCheck(t *testing.T) {
	tests := []struct {
		desc string
		info bmnInfo
		want []string // one substring expected per flag, in order
	}{
		{
			desc: "current unset -> legacy alert path flag",
			info: bmnInfo{BundleTarget: "h100-2024.10"},
			want: []string{"node-fwbundle.current is UNSET"},
		},
		{
			desc: "current != target -> mid-update flag",
			info: bmnInfo{BundleCurrent: "h100-2024.09", BundleTarget: "h100-2024.10"},
			want: []string{"fwbundle mid-update: current=h100-2024.09 target=h100-2024.10"},
		},
		{
			desc: "legacy NodePCIWidth alert name",
			info: bmnInfo{
				BundleCurrent: "h100-2024.10", BundleTarget: "h100-2024.10",
				Alerts: []bmnAlert{{Name: "NodePCIWidth"}},
			},
			want: []string{"legacy NodePCIWidth/Speed alert firing"},
		},
		{
			desc: "legacy NodePCISpeed alert name, flagged once",
			info: bmnInfo{
				Alerts: []bmnAlert{{Name: "NodePCISpeed"}, {Name: "NodePCIWidth"}},
			},
			want: []string{"node-fwbundle.current is UNSET", "legacy NodePCIWidth/Speed alert firing"},
		},
		{
			desc: "modern alert name is not the legacy flag",
			info: bmnInfo{
				BundleCurrent: "h100-2024.10", BundleTarget: "h100-2024.10",
				Alerts: []bmnAlert{{Name: "NodePCILinkWidthUnexpected"}},
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		info := tt.info
		bundleCheck(&info)
		if len(info.BundleFlags) != len(tt.want) {
			t.Errorf("%s: got %d flags %v; want %d", tt.desc, len(info.BundleFlags), info.BundleFlags, len(tt.want))
			continue
		}
		for i, sub := range tt.want {
			if !strings.Contains(info.BundleFlags[i], sub) {
				t.Errorf("%s: flag[%d] = %q; want substring %q", tt.desc, i, info.BundleFlags[i], sub)
			}
		}
	}
}

func TestBMNRegionFromZone(t *testing.T) {
	tests := []struct{ zone, want string }{
		{"RNO2A", "RNO2"},
		{"RNO2B", "RNO2"},
		{"LGA1C", "LGA1"},
		{"RNO2", ""}, // no trailing zone letter
		{"", ""},
	}
	for _, tt := range tests {
		if got := bmnRegionFromZone(tt.zone); got != tt.want {
			t.Errorf("bmnRegionFromZone(%q) = %q; want %q", tt.zone, got, tt.want)
		}
	}
}

func TestBMNNormBDF(t *testing.T) {
	tests := []struct{ in, want string }{
		{"5C:00.0", "0000:5c:00.0"},
		{"0000:5c:00.0", "0000:5c:00.0"},
		{"c1:00.0", "0000:c1:00.0"},
	}
	for _, tt := range tests {
		if got := bmnNormBDF(tt.in); got != tt.want {
			t.Errorf("bmnNormBDF(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}
