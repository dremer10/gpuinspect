package main

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Analysis + verdict (runbook steps 1, 4-7)
// ---------------------------------------------------------------------------

func (t *triage) cnt(n int) string {
	if n > 0 {
		return fmt.Sprintf("%s%d%s", t.c.R, n, t.c.X)
	}
	return fmt.Sprintf("%s0%s", t.c.G, t.c.X)
}

// pexFalsePositive classifies the node against the PEX890xx false-positive
// runbook (RNO2 morning standup 2026-07-14 — same class as XID 109): link
// speed/width "unexpected" on a PEX890xx switch port almost always means the
// health sweep sampled the port while its link partner was idle or trained
// down. Two observable shapes:
//   - idle-port x0: bridge with NO device behind it (METAL-4742), and
//   - self-cleared: the alert BDF re-checks at full speed/width.
//
// The node classifies only when EVERY degraded device is the idle-port shape
// and nothing else is wrong anywhere — no XID/AER/ECC/NVLink/missing-GPU, no
// unreachable devices, no retrain noise, no wrong-part endpoints — mirroring
// the runbook's "confirm the only fallout reason" gate. GPU findings must be
// PRESENT and clean: no evidence is not evidence of health. post-drain/swap/
// reseat runs never classify (already in a physical-remediation flow).
func (t *triage) pexFalsePositive() (idle, cleared []string, ok bool) {
	if t.o.postDrain || t.o.postSwap || t.o.postReseat {
		return nil, nil, false
	}
	if t.eccUncorr || t.remapPend || t.remapFail || t.nvlinkAnom || t.xid || t.aerFatal {
		return nil, nil, false
	}
	if len(t.evalFindings) > 0 {
		return nil, nil, false
	}
	if len(t.ibFault) > 0 || len(t.thermalFault) > 0 {
		return nil, nil, false
	}
	if len(t.pcieErrors) > 0 || t.gpu == nil {
		return nil, nil, false
	}
	if len(t.gpu.MissingBDFs) > 0 || len(t.gpu.RevFF) > 0 {
		return nil, nil, false
	}
	if t.gpu.Expected > 0 && t.gpu.CountSMI < t.gpu.Expected {
		return nil, nil, false
	}
	clearedSet := map[string]bool{}
	for _, b := range t.alertCleared {
		clearedSet[b] = true
	}
	for _, r := range t.results {
		if r.retrainNoise || r.childCapBelow {
			return nil, nil, false
		}
		// DPC on an idle port is part of the known METAL-4742 triplet, but on
		// a port that was alerting and then recovered it is real error
		// history — containment fired — so the self-cleared claim is off.
		if clearedSet[r.bdf] && r.dpcTrig {
			return nil, nil, false
		}
		if !r.degraded {
			continue
		}
		if !r.noChild || !r.down {
			return nil, nil, false
		}
		idle = append(idle, r.bdf)
	}
	sort.Strings(idle)
	cleared = append([]string{}, t.alertCleared...)
	sort.Strings(cleared)
	return idle, cleared, len(idle)+len(cleared) > 0
}

func (t *triage) verdict() int {
	o := t.o
	t.header("ANALYSIS SUMMARY")
	t.log.p("Degraded PEX/switch links : %s  %s", t.cnt(len(t.switchDeg)), strings.Join(t.switchDeg, " "))
	t.log.p("Degraded NVMe/BlueField   : %s  %s", t.cnt(len(t.nvmeDeg)), strings.Join(t.nvmeDeg, " "))
	t.log.p("Degraded GPU/NIC endpoint : %s  %s", t.cnt(len(t.reseatDeg)), strings.Join(t.reseatDeg, " "))
	t.log.p("Degraded other devices    : %s  %s", t.cnt(len(t.otherDeg)), strings.Join(t.otherDeg, " "))
	t.log.p("Links fully DOWN (x0)     : %s  %s", t.cnt(len(t.linkDown)), strings.Join(t.linkDown, " "))
	t.log.p("Unreachable devices       : %s %s", t.cnt(len(t.pcieErrors)), strings.Join(t.pcieErrors, " "))
	t.log.p("GPU: ecc_uncorr=%s remap_pending=%s remap_fail=%s nvlink=%s xid=%s aer_fatal=%s",
		t.cnt(b2i(t.eccUncorr)), t.cnt(b2i(t.remapPend)), t.cnt(b2i(t.remapFail)),
		t.cnt(b2i(t.nvlinkAnom)), t.cnt(b2i(t.xid)), t.cnt(b2i(t.aerFatal)))
	t.log.p("IB ports not ACTIVE       : %s  %s", t.cnt(len(t.ibFault)), strings.Join(t.ibFault, " "))
	t.log.p("GPU thermal slowdown      : %s  %s", t.cnt(len(t.thermalFault)), strings.Join(t.thermalFault, " "))
	if t.evalRan {
		t.log.p("Full-eval findings        : %s  (notes: %d — see eval/ in bundle)", t.cnt(len(t.evalFindings)), len(t.evalNotes))
	}

	var rma []string
	if t.remapFail {
		rma = append(rma, "GPU row-remapping FAILURE (GPU RMA path, independent of PCIe alert)")
	}
	if t.eccUncorr {
		rma = append(rma, "Uncorrectable ECC errors present")
	}
	if t.aerFatal {
		rma = append(rma, "Fatal AER errors in dmesg")
	}
	if len(t.pcieErrors) > 0 {
		rma = append(rma, "PCIe device(s) unreachable: "+strings.Join(t.pcieErrors, " "))
	}
	swOther := append(append([]string{}, t.switchDeg...), t.otherDeg...)
	if o.postDrain && len(swOther) > 0 {
		rma = append(rma, "Link still degraded/down AFTER 10-15 min AC power drain: "+strings.Join(swOther, " "))
	}
	if o.postSwap && len(t.nvmeDeg) > 0 {
		rma = append(rma, "NVMe link still degraded AFTER drive swap: "+strings.Join(t.nvmeDeg, " "))
	}
	if o.postReseat && len(t.reseatDeg) > 0 {
		rma = append(rma, "Link still degraded AFTER the one DCT reseat attempt (bay/riser/cable/"+
			"backplane suspected — out of DCT scope): "+strings.Join(t.reseatDeg, " "))
	}

	anyDeg := len(t.switchDeg)+len(t.nvmeDeg)+len(t.reseatDeg)+len(t.otherDeg) > 0
	idleFP, clearedFP, isFP := t.pexFalsePositive()
	t.log.p("")

	switch {
	case len(rma) > 0:
		t.printRMA(rma)
		t.footer()
		return 2
	case isFP:
		t.printFalsePositive(idleFP, clearedFP)
		t.footer()
		return 0
	case anyDeg || t.remapPend || t.nvlinkAnom || len(t.ibFault) > 0 || len(t.thermalFault) > 0 ||
		len(t.evalFindings) > 0 || t.gpuDeficit():
		t.printDegraded()
		t.footer()
		return 1
	default:
		t.printHealthy()
		t.footer()
		return 0
	}
}

// gpuDeficit reports fewer GPUs than the platform expects — to nvidia-smi
// (driver/NVML fault or GPU lost to the driver) or to lspci (missing from the
// bus). A GPU-inspection verdict must never read HEALTHY while GPUs are not
// visible, even when every PCIe link that IS up checks clean.
func (t *triage) gpuDeficit() bool {
	g := t.gpu
	return g != nil && g.Expected > 0 && (g.CountSMI < g.Expected || g.CountLspci < g.Expected)
}

func (t *triage) footer() {
	t.log.p("")
	t.log.p("%sFull log: %s%s", t.c.D, t.logFile, t.c.X)
	t.log.p("%sBundle  : %s%s%s", t.c.D, t.bundleShow, t.bundleNote, t.c.X)
}

func (t *triage) printHealthy() {
	c, o := t.c, t.o
	t.log.p("%s╔══════════════════════════════════════════════════════════╗%s", c.G, c.X)
	t.log.p("%s║  VERDICT : HEALTHY — links at expected speed/width       ║%s", c.G, c.X)
	t.log.p("%s╚══════════════════════════════════════════════════════════╝%s", c.G, c.X)
	t.log.p("")
	if o.postDrain || o.postSwap || o.postReseat {
		t.log.p("%sThe power drain / drive swap / reseat CLEARED the issue. Next (runbook step 6):%s", c.B, c.X)
		t.log.p("  1. Confirm NodePCILinkSpeedUnexpected alert has cleared.")
		t.log.p("  2. Run node verification from mgmt (FLCC):")
		t.log.p("       %scwctl flcc node -w test -s test %s%s", c.B, o.bmn, c.X)
		t.log.p("  3. If tests pass, transition node back to ready/production.")
	} else {
		t.log.p("No degradation found. If the alert still fires, re-run against the")
		t.log.p("exact BDF from the alert:  %s%s <alert-BDF>%s", c.B, t.rerun, c.X)
	}
}

// printFalsePositive renders the PEX890xx false-positive verdict: the node
// can go back to ready without an RMA or hardware ticket. Advisory only —
// the cwctl command is text for the operator, never executed.
func (t *triage) printFalsePositive(idle, cleared []string) {
	c, o := t.c, t.o
	t.log.p("%s╔══════════════════════════════════════════════════════════╗%s", c.G, c.X)
	t.log.p("%s║  VERDICT : FALSE POSITIVE — PEX890xx, return to ready    ║%s", c.G, c.X)
	t.log.p("%s╚══════════════════════════════════════════════════════════╝%s", c.G, c.X)
	t.log.p("")
	if len(idle) > 0 {
		t.log.p("Idle-port x0, no device behind bridge (METAL-4742) : %s%s%s", c.B, strings.Join(idle, " "), c.X)
	}
	if len(cleared) > 0 {
		t.log.p("Self-cleared — full speed/width at re-check        : %s%s%s", c.B, strings.Join(cleared, " "), c.X)
	}
	t.log.p("")
	t.log.p("%sVERIFIED ON-NODE (runbook return-to-ready gates):%s", c.B, c.X)
	t.log.p("  %s✓%s no XID in dmesg, no fatal AER", c.G, c.X)
	t.log.p("  %s✓%s IB/fabric ports all ACTIVE (ib_ports.txt in bundle)", c.G, c.X)
	t.log.p("  %s✓%s no GPU thermal slowdown (nvidia_smi_q.txt in bundle)", c.G, c.X)
	t.log.p("  %s✓%s GPU suite clean — ECC, remap, NVLink, count, no (rev ff)/missing GPUs", c.G, c.X)
	t.log.p("  %s✓%s AER/DPC history sampled on every alert BDF — no flapping-link noise", c.G, c.X)
	t.log.p("")
	t.log.p("%sThe health sweep sampled the PEX890xx switch port while its link partner%s", c.D, c.X)
	t.log.p("%swas idle/trained down (low-power state or probe race) — known benign,%s", c.D, c.X)
	t.log.p("%ssame class as XID 109. It clears on its own on the next read/reboot.%s", c.D, c.X)
	t.log.p("")
	t.log.p("%sOK TO RETURN — no RMA, no hardware ticket (RNO2 runbook, Jul 14 2026):%s", c.G, c.X)
	t.log.p("       %scwctl flcc node -w return-to-ready -m \"sending to ready\" %s%s", c.B, o.bmn, c.X)
	t.log.p("  %sCAVEAT:%s if the SAME BDF re-alerts across reboots, treat it as a genuine", c.Y, c.X)
	t.log.p("  link fault — reseat via OFR/DCT or open a HW ticket. (In BMN mode the")
	t.log.p("  orchestrator also verifies no non-PCI-link alert is firing on the BMN.)")
}

func (t *triage) printDegraded() {
	c, o := t.c, t.o
	t.log.p("%s╔══════════════════════════════════════════════════════════╗%s", c.Y, c.X)
	t.log.p("%s║  VERDICT : DEGRADED — FOLLOW RUNBOOK REMEDIATION         ║%s", c.Y, c.X)
	t.log.p("%s╚══════════════════════════════════════════════════════════╝%s", c.Y, c.X)
	t.log.p("")
	if o.triaged {
		t.log.p("%sSTEP 0-1: skipped (--triaged: node already in triage).%s", c.D, c.X)
	} else {
		t.log.p("%sSTEP 0%s (manual): Verify node is NOT in a customer cluster (Node", c.B, c.X)
		t.log.p("Timeline in Grafana) before proceeding.")
		t.log.p("")
		t.log.p("%sSTEP 1%s: Take the node out of production. On mgmt (FLCC):", c.B, c.X)
		t.log.p("    %scwctl flcc node -s triage %s%s", c.B, o.bmn, c.X)
	}
	t.log.p("")
	t.log.p("%sSTEP 2%s: Bad link confirmed by this tool (LnkSta above).", c.B, c.X)
	t.log.p("")
	t.log.p("%sSTEP 3%s: Collect bundle captured: %s%s%s%s%s%s", c.B, c.X, c.B, t.bundleShow, c.X, c.D, t.bundleNote, c.X)
	t.log.p("    Also run the standard AWX collect job; attach its URL to the HO/DO.")

	swOther := append(append([]string{}, t.switchDeg...), t.otherDeg...)
	if len(swOther) > 0 {
		t.log.p("")
		t.log.p("%sSTEP 4%s: Open/update the DCT ticket and request a power drain:", c.B, c.X)
		t.log.p("  %s------------------------------------------------------------------%s", c.D, c.X)
		t.log.p("  Please perform a 10-15 minute power drain (full AC off) on this")
		t.log.p("  node and power it back on.")
		for _, b := range swOther {
			t.alertReason(b)
		}
		t.log.p("  %s------------------------------------------------------------------%s", c.D, c.X)
		t.log.p("")
		t.log.p("%sSTEP 5%s: After the drain, re-run FROM YOUR LAPTOP:", c.B, c.X)
		t.log.p("    %s%s --post-drain %s%s", c.B, t.rerun, strings.Join(swOther, " "), c.X)
		t.log.p("    %sRecovered%s -> verification workflow.  %sStill degraded%s -> RMA verdict.", c.G, c.X, c.R, c.X)
	}
	if len(t.reseatDeg) > 0 {
		t.log.p("")
		t.log.p("%sGPU/NIC ENDPOINT PATH%s (width runbook): request %sONE DCT reseat%s of the", c.B, c.X, c.B, c.X)
		t.log.p("endpoint behind the bridge. Affected device(s):")
		for _, b := range t.reseatDeg {
			r := t.results[b]
			slot := ""
			if r.slot != "" {
				slot = "  Slot " + r.slot
			}
			t.log.p("  Bridge : %s  (%s)%s", b, descOnly(r.desc), slot)
			if r.childBDF != "" {
				t.log.p("  Endpoint: %s  (%s)  — identity details logged above / in bundle", r.childBDF, r.childDesc)
			}
			if r.childCapBelow {
				t.log.p("  %sNOTE   : endpoint LnkCap is below the bridge's — the endpoint is the", c.Y)
				t.log.p("           ceiling. Wrong part or FW: verify against BOM. NO DCT action.%s", c.X)
			}
			if r.childLinkDeg {
				t.log.p("  %sBoth sides report the degraded link — physical/signal-integrity;%s", c.D, c.X)
				t.log.p("  %sreseat path confirmed from the endpoint side (runbook step 4).%s", c.D, c.X)
			}
			if cs := cheatSheet(r.speedDeg, r.widthDeg, r.down); cs != "" {
				t.log.p("  Cheat sheet: %s", cs)
			}
		}
		t.log.p("  %s------------------------------------------------------------------%s", c.D, c.X)
		t.log.p("  Please reseat the device listed above (one attempt). Include the")
		t.log.p("  slot number in the ticket.")
		for _, b := range t.reseatDeg {
			t.alertReason(b)
		}
		t.log.p("  %s------------------------------------------------------------------%s", c.D, c.X)
		t.log.p("After the reseat, re-run FROM YOUR LAPTOP:")
		t.log.p("    %s%s --post-reseat %s%s", c.B, t.rerun, strings.Join(t.reseatDeg, " "), c.X)
		t.log.p("%sOnly ONE reseat attempt per alert%s — if it does not clear, the likely", c.B, c.X)
		t.log.p("culprit is the bay/riser/cable/backplane (out of DCT scope) -> RMA verdict.")
	}
	t.runbookNotes()
	if len(t.nvmeDeg) > 0 {
		t.log.p("")
		t.log.p("%sNVMe/BlueField PATH%s (per runbook): have DCT %sSWAP TWO NVMe DRIVES%s", c.B, c.X, c.B, c.X)
		t.log.p("rather than power cycle. Affected device(s) and physical slots:")
		for _, b := range t.nvmeDeg {
			t.log.p("  BDF    : %s  (%s)", b, descOnly(t.results[b].desc))
			t.nvmeSlotMap(b)
		}
		t.log.p("After the swap, re-run FROM YOUR LAPTOP:")
		t.log.p("    %s%s --post-swap %s%s", c.B, t.rerun, strings.Join(t.nvmeDeg, " "), c.X)
	}
	if t.gpuDeficit() {
		g := t.gpu
		t.log.p("")
		if g.CountLspci >= g.Expected && g.CountSMI < g.Expected {
			t.log.p("%sGPU VISIBILITY PATH:%s nvidia-smi sees %d/%d GPUs but lspci enumerates %d/%d —", c.R, c.X,
				g.CountSMI, g.Expected, g.CountLspci, g.Expected)
			t.log.p("the PCIe endpoints are present; this is a driver/NVML fault, not missing hardware.")
			t.log.p("Check: lsmod | grep nvidia; dmesg Xid history (bundle); systemctl status nvidia-persistenced.")
			t.log.p("Driver reload / reboot path first — do NOT open a hardware ticket on this alone.")
		} else {
			t.log.p("%sMISSING GPU PATH:%s only %d/%d GPUs on the PCIe bus (nvidia-smi %d/%d) —", c.R, c.X,
				g.CountLspci, g.Expected, g.CountSMI, g.Expected)
			t.log.p("check dmesg Xid history in the bundle, request the DCT power drain; if the GPU is")
			t.log.p("still absent after the drain -> RMA for missing GPU.")
		}
	}
	if t.remapPend {
		t.log.p("")
		t.log.p("%sNOTE:%s GPU row remap PENDING — needs GPU reset/reboot (separate runbook).", c.Y, c.X)
	}
	if t.nvlinkAnom {
		t.log.p("")
		t.log.p("%sNOTE:%s NVLink not at %s — see gpu_full.txt in bundle.", c.Y, c.X, nvlinkExpected)
	}
	if len(t.ibFault) > 0 {
		t.log.p("")
		t.log.p("%sNOTE:%s IB/fabric port(s) not ACTIVE: %s — check the fabric side", c.Y, c.X, strings.Join(t.ibFault, ", "))
		t.log.p("(ib_ports.txt in bundle) BEFORE returning this node; not a PCIe-runbook item.")
	}
	if len(t.thermalFault) > 0 {
		t.log.p("")
		t.log.p("%sNOTE:%s GPU thermal slowdown ACTIVE: %s — see nvidia_smi_q.txt in bundle.", c.Y, c.X, strings.Join(t.thermalFault, ", "))
	}
	if len(t.evalFindings) > 0 {
		t.log.p("")
		t.log.p("%sNOTE:%s full GPU evaluation findings (details under eval/ in bundle):", c.Y, c.X)
		for _, f := range t.evalFindings {
			t.log.p("  - %s", f)
		}
	}
}

// alertReason prints the DCT-ticket reason line(s) naming the alert that
// matches what actually degraded (speed, width, or both).
func (t *triage) alertReason(bdf string) {
	r := t.results[bdf]
	if r.speedDeg || !r.widthDeg { // default to speed wording when unsure
		t.log.p("  Reason: NodePCILinkSpeedUnexpected -")
		t.log.p("  PCI link speed for %s at %s is unexpected.", descOnly(r.desc), bdf)
	}
	if r.widthDeg {
		t.log.p("  Reason: NodePCILinkWidthUnexpected -")
		t.log.p("  PCI link width for %s at %s is unexpected.", descOnly(r.desc), bdf)
	}
}

// runbookNotes surfaces the width-runbook interpretation cues that change
// what the operator should do next.
func (t *triage) runbookNotes() {
	c := t.c
	for _, r := range t.results {
		if !r.degraded {
			continue
		}
		if r.noChild && r.down {
			t.log.p("")
			t.log.p("%sCAUTION:%s %s is a bridge with NOTHING behind it — x0 is the expected idle", c.Y, c.X, r.bdf)
			t.log.p("state for an unused PEX downstream port. This is the known false-positive")
			t.log.p("pattern (METAL-4742: raw saturated width 63 on idle ports). Verify against")
			t.log.p("golden state / vendor topology doc BEFORE requesting any physical work.")
		}
		if r.dpcTrig {
			t.log.p("")
			t.log.p("%sNOTE:%s %s — DPC triggered (Downstream Port Containment fired). Error", c.Y, c.X, r.bdf)
			t.log.p("history is in the bundle; attach it to the ticket before any physical action.")
		}
		if r.retrainNoise {
			t.log.p("")
			t.log.p("%sNOTE:%s %s — correctable-error noise (BadTLP/Rollover): the link is", c.Y, c.X, r.bdf)
			t.log.p("retraining. Marginal link — if it recurs after one reseat, RMA after documenting.")
		}
	}
}

func (t *triage) printRMA(reasons []string) {
	c, o := t.c, t.o
	t.log.p("%s╔══════════════════════════════════════════════════════════╗%s", c.R, c.X)
	t.log.p("%s║  VERDICT : PRESENT FOR RMA  (runbook step 7)             ║%s", c.R, c.X)
	t.log.p("%s╚══════════════════════════════════════════════════════════╝%s", c.R, c.X)
	for _, r := range reasons {
		t.log.p("  %s✗%s %s", c.R, c.X, r)
	}
	t.log.p("")
	t.log.p("%sEXACT NEXT STEPS:%s", c.B, c.X)
	t.log.p("  1. Keep the node in triage/broken:")
	t.log.p("       %scwctl flcc node -s triage %s%s", c.B, o.bmn, c.X)
	t.log.p("  2. Update the HO/DO with:")
	t.log.p("     - Node: gMAC, BMN (%s), serial, deviceslot, region/rack/RU", o.bmn)
	t.log.p("     - Physical slot #, bridge BDF + endpoint BDF, vendor PN/SN of the")
	t.log.p("       endpoint (identity lines above / in bundle), dmesg AER/DPC snippet")
	t.log.p("       (errhist_*.txt in bundle)")
	t.log.p("     - BEFORE/AFTER LnkSta output:")
	all := append(append(append(append([]string{}, t.switchDeg...), t.otherDeg...), t.nvmeDeg...), t.reseatDeg...)
	for _, b := range all {
		r := t.results[b]
		before := r.before
		if before == "" {
			before = "<no prior snapshot - attach alert-time lspci>"
		}
		extra := ""
		if r.slot != "" {
			extra = "  Slot " + r.slot
		}
		if r.childBDF != "" {
			extra += "  endpoint " + r.childBDF + " (" + r.childDesc + ")"
		}
		t.log.p("         %s%s%s%s", c.B, b, c.X, extra)
		t.log.p("           before: %s", before)
		t.log.p("           after : %s%s%s", c.R, orNA(r.lnksta), c.X)
	}
	t.log.p("     - Collect bundle: %s%s (+ AWX collect URL)", t.bundleShow, t.bundleNote)
	t.log.p("  3. Request %sRMA of the PEX890xx switch / baseboard%s (or affected", c.B, c.X)
	t.log.p("     NVMe/drive per path) per vendor guidance.")
	t.log.p("  4. Escalate to %s@metal-on-call [#Metal]%s if needed.", c.B, c.X)
	t.log.p("")
	t.log.p("%sOnly hardware replacement will clear the alert at this point.%s", c.R, c.X)
}
