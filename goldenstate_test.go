package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func goldenTestInfo(alertBDFs ...string) *bmnInfo {
	return &bmnInfo{
		Name: "ss900000x0000000", GMAC: "ge71c24", SKU: "GPU-H100-12",
		BundleCurrent: "smc-421GE-LCC-phoenix-2.0.0",
		PCIAlertBDFs:  alertBDFs,
	}
}

func pex(speed, width float64) *goldenDev {
	return &goldenDev{
		Speed: speed, Width: width, HasSpeed: true, HasWidth: true,
		DeviceName: "PEX890xx PCIe Gen 5 Switch", VendorID: "1000", DeviceID: "c030",
	}
}

func x550(speed, width float64) *goldenDev {
	return &goldenDev{
		Speed: speed, Width: width, HasSpeed: true, HasWidth: true,
		DeviceName: "Ethernet Controller X550", VendorID: "8086", DeviceID: "1563",
	}
}

func joined(ss []string) string { return strings.Join(ss, "\n") }

// The ge71c24 case (2026-08-17): live PEX switch at full 32 GT/s x16 where the
// golden row expects an X550 NIC — an enumeration/layout mismatch, not a
// degraded link. Must classify as a golden-state error with the fix advice.
func TestClassifyGoldenIdentityMismatch(t *testing.T) {
	d := &goldenData{
		Expected: map[string]*goldenDev{"0000:b1:00.0": x550(8e9, 4)},
		Live:     map[string]*goldenDev{"0000:b1:00.0": pex(32e9, 16)},
	}
	gc := classifyGolden(goldenTestInfo("0000:b1:00.0"), d, map[string]bool{}, true)
	if !gc.Mismatch {
		t.Fatalf("identity mismatch must set Mismatch; issues: %v", gc.Issues)
	}
	all := joined(gc.Issues)
	if !strings.Contains(all, "MISMATCH @ 0000:b1:00.0") || !strings.Contains(all, "comparing two different devices") {
		t.Fatalf("missing identity-mismatch issue: %v", gc.Issues)
	}
	if !strings.Contains(joined(gc.NextSteps), goldenFixAdvice) {
		t.Fatalf("missing golden fix advice: %v", gc.NextSteps)
	}
	// Layout sweep also counts the mismatching BDF.
	if !strings.Contains(all, "enumerate a different device than the golden table") {
		t.Fatalf("missing layout-sweep issue: %v", gc.Issues)
	}
}

// Same device but the golden row carries wrong numbers while the on-node
// triage saw the link at full capability -> stale golden values.
func TestClassifyGoldenStaleValues(t *testing.T) {
	d := &goldenData{
		Expected: map[string]*goldenDev{"0000:b1:00.0": pex(8e9, 4)},
		Live:     map[string]*goldenDev{"0000:b1:00.0": pex(32e9, 16)},
	}
	gc := classifyGolden(goldenTestInfo("0000:b1:00.0"), d, map[string]bool{"0000:b1:00.0": false}, true)
	if !gc.Mismatch || !strings.Contains(joined(gc.Issues), "golden-state STALE @ 0000:b1:00.0") {
		t.Fatalf("expected STALE classification, got mismatch=%v issues=%v", gc.Mismatch, gc.Issues)
	}
}

// Golden and live disagree AND the on-node triage confirmed degradation: the
// golden table corroborates the alert — genuine fault, no golden error.
func TestClassifyGoldenAgreesWithDegradation(t *testing.T) {
	d := &goldenData{
		Expected: map[string]*goldenDev{"0000:b1:00.0": pex(32e9, 16)},
		Live:     map[string]*goldenDev{"0000:b1:00.0": pex(8e9, 4)},
	}
	gc := classifyGolden(goldenTestInfo("0000:b1:00.0"), d, map[string]bool{"0000:b1:00.0": true}, true)
	if gc.Mismatch {
		t.Fatalf("corroborated degradation must not be a golden mismatch: %v", gc.Issues)
	}
	if !strings.Contains(joined(gc.Issues), "golden-state agrees with the alert @ 0000:b1:00.0") {
		t.Fatalf("missing corroboration issue: %v", gc.Issues)
	}
}

// Values disagree but there is no on-node verdict: the link state is
// unverified, so nothing may be blamed on the table yet.
func TestClassifyGoldenUnverifiedLink(t *testing.T) {
	d := &goldenData{
		Expected: map[string]*goldenDev{"0000:b1:00.0": pex(8e9, 4)},
		Live:     map[string]*goldenDev{"0000:b1:00.0": pex(32e9, 16)},
	}
	gc := classifyGolden(goldenTestInfo("0000:b1:00.0"), d, nil, false)
	if gc.Mismatch {
		t.Fatalf("unverified link state must not claim a golden mismatch: %v", gc.Issues)
	}
	if !strings.Contains(joined(gc.Issues), "unverified") {
		t.Fatalf("missing unverified issue: %v", gc.Issues)
	}
}

// Golden refreshed after the alert fired: both sides now agree — surface it so
// the operator re-checks the alert instead of acting on it.
func TestClassifyGoldenMatchAfterRefresh(t *testing.T) {
	d := &goldenData{
		Expected: map[string]*goldenDev{"0000:b1:00.0": pex(32e9, 16)},
		Live:     map[string]*goldenDev{"0000:b1:00.0": pex(32e9, 16)},
	}
	gc := classifyGolden(goldenTestInfo("0000:b1:00.0"), d, map[string]bool{}, true)
	if gc.Mismatch {
		t.Fatalf("agreeing sides must not be a mismatch: %v", gc.Issues)
	}
	if !strings.Contains(joined(gc.Issues), "golden-state now MATCHES live @ 0000:b1:00.0") {
		t.Fatalf("missing match issue: %v", gc.Issues)
	}
}

// No golden rows at all for the (SKU, fwbundle) — the NodeMissingPCIGoldenState class.
func TestClassifyGoldenNoRows(t *testing.T) {
	d := &goldenData{Expected: map[string]*goldenDev{}, Live: map[string]*goldenDev{"0000:b1:00.0": pex(32e9, 16)}}
	gc := classifyGolden(goldenTestInfo("0000:b1:00.0"), d, map[string]bool{}, true)
	if !gc.Mismatch || !strings.Contains(joined(gc.Issues), "NodeMissingPCIGoldenState") {
		t.Fatalf("expected missing-golden-state classification: %v", gc.Issues)
	}
}

// Idle-port raw width (63, METAL-4742) vs an expected-idle 0 must count as
// agreement — the alert rules special-case it the same way.
func TestGoldenValuesIdleWidth(t *testing.T) {
	exp := &goldenDev{Width: 0, HasWidth: true}
	live := &goldenDev{Width: idleWidthRaw, HasWidth: true}
	if goldenValuesDiffer(exp, live) {
		t.Fatal("idle-port width 63 vs expected 0 must not differ")
	}
	if !goldenValuesDiffer(&goldenDev{Width: 16, HasWidth: true}, &goldenDev{Width: 4, HasWidth: true}) {
		t.Fatal("x4 vs x16 must differ")
	}
}

// A layout shift beyond the alert BDFs (bundle-flip signature) must be caught
// by the node-wide sweep even when every alert BDF checks clean.
func TestClassifyGoldenLayoutSweep(t *testing.T) {
	d := &goldenData{
		Expected: map[string]*goldenDev{
			"0000:b1:00.0": pex(32e9, 16),
			"0000:ac:00.0": x550(8e9, 4),
		},
		Live: map[string]*goldenDev{
			"0000:b1:00.0": pex(32e9, 16),
			"0000:ac:00.0": pex(32e9, 16),
		},
	}
	gc := classifyGolden(goldenTestInfo("0000:b1:00.0"), d, map[string]bool{}, true)
	if !gc.Mismatch || !strings.Contains(joined(gc.Issues), "0000:ac:00.0") {
		t.Fatalf("layout sweep must flag the non-alert BDF: %v", gc.Issues)
	}
}

func TestMergeGoldenRowsConflict(t *testing.T) {
	raw := `{"status":"success","data":{"result":[
	  {"metric":{"device":"0000:b1:00.0","device_name":"PEX890xx PCIe Gen 5 Switch","vendor_id":"1000","device_id":"c030"},"value":[1,"32000000000"]},
	  {"metric":{"device":"0000:b1:00.0","device_name":"PEX890xx PCIe Gen 5 Switch","vendor_id":"1000","device_id":"c030"},"value":[1,"32000000000"]},
	  {"metric":{"device":"0000:ac:00.0","device_name":"X","vendor_id":"1000","device_id":"c030"},"value":[1,"8000000000"]},
	  {"metric":{"device":"0000:ac:00.0","device_name":"X","vendor_id":"1000","device_id":"c030"},"value":[1,"16000000000"]}]}}`
	var resp promResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	into, conflicts := map[string]*goldenDev{}, map[string]bool{}
	mergeGoldenRows(into, resp.Data.Result, true, conflicts)
	if into["0000:b1:00.0"] == nil || into["0000:b1:00.0"].Speed != 32e9 || !into["0000:b1:00.0"].HasSpeed {
		t.Fatalf("bad merge: %+v", into["0000:b1:00.0"])
	}
	if conflicts["0000:b1:00.0"] {
		t.Fatal("identical duplicate rows must not conflict")
	}
	if !conflicts["0000:ac:00.0"] {
		t.Fatal("disagreeing duplicate rows must conflict")
	}
}

func TestFmtGoldenHelpers(t *testing.T) {
	if got := fmtGT(32e9); got != "32 GT/s" {
		t.Fatalf("fmtGT: %q", got)
	}
	if got := fmtGoldenVals(pex(32e9, 16)); got != "32 GT/s x16" {
		t.Fatalf("fmtGoldenVals: %q", got)
	}
	if got := fmtGoldenVals(&goldenDev{Width: idleWidthRaw, HasWidth: true}); !strings.Contains(got, "idle-port raw") {
		t.Fatalf("idle width must be annotated: %q", got)
	}
}

func TestVerdictLabelGoldenMismatch(t *testing.T) {
	if got := verdictLabel(inspectResult{VerdictCode: 1, GoldenMismatch: true}); got != "GOLDEN-MIS" {
		t.Fatalf("verdictLabel: %q", got)
	}
}

func TestFleetAdvisoryGolden(t *testing.T) {
	results := []inspectResult{
		{BMN: "bmn-a", VerdictCode: 1, GoldenMismatch: true, Issues: []string{"golden-state MISMATCH @ 0000:b1:00.0: ..."}},
		{BMN: "bmn-b", VerdictCode: 0},
	}
	lines := joined(fleetAdvisory(results))
	if !strings.Contains(lines, "Golden-state errors: 1/2 node(s) — bmn-a") {
		t.Fatalf("fleet advisory missing golden rollup:\n%s", lines)
	}
	if !strings.Contains(lines, "do NOT return-to-ready") {
		t.Fatalf("fleet advisory missing golden caveat:\n%s", lines)
	}
	// summaryMd must carry the advisory (and thereby the golden mention).
	if md := summaryMd(results); !strings.Contains(md, "Golden-state errors: 1/2") || !strings.Contains(md, "GOLDEN-MIS") {
		t.Fatalf("summaryMd missing golden mention:\n%s", md)
	}
}

func TestDegradedBDFSet(t *testing.T) {
	vf := &verdictFile{Devices: []verdictDevice{
		{BDF: "0000:b1:00.0", SpeedDeg: true},
		{BDF: "0000:ac:00.0"}, // self-cleared/healthy at re-check
	}}
	m := degradedBDFSet(vf)
	if !m["0000:b1:00.0"] || m["0000:ac:00.0"] {
		t.Fatalf("degradedBDFSet: %v", m)
	}
	if degradedBDFSet(nil) != nil {
		t.Fatal("nil verdict file must yield nil map")
	}
}

func TestGoldenSkipGates(t *testing.T) {
	info := goldenTestInfo("0000:b1:00.0")
	info.SKU = ""
	if gc := goldenStateCheck(info, nil, true); gc.Skipped == "" {
		t.Fatal("missing SKU must skip the check")
	}
	info = goldenTestInfo("0000:b1:00.0")
	info.BundleCurrent = ""
	if gc := goldenStateCheck(info, nil, true); gc.Skipped == "" {
		t.Fatal("unset fwbundle must skip the check")
	}
}
