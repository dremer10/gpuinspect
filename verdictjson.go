package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// verdictJSON is the machine-readable mirror of the printed verdict, written
// to <logDir>/verdict.json on the node and retrieved to the laptop, where
// orchestrate.go feeds it to the matrix and the AI layer. NextSteps are
// advisory text only — gpuinspect never executes them.
type verdictJSON struct {
	BMN         string          `json:"bmn"`
	VerdictCode int             `json:"verdict_code"`
	Issues      []string        `json:"issues"`
	NextSteps   []string        `json:"next_steps"`
	GPU         *gpuFindings    `json:"gpu,omitempty"`
	Flags       map[string]bool `json:"flags"`
	Devices     []verdictDevice `json:"devices,omitempty"`
	XidLines    []string        `json:"xid_lines,omitempty"` // NVRM Xid dmesg lines for XID Bible mapping
}

type verdictDevice struct {
	BDF      string `json:"bdf"`
	Desc     string `json:"desc"`
	LnkSta   string `json:"lnksta"`
	Class    string `json:"class"`
	Slot     string `json:"slot,omitempty"`
	Child    string `json:"child_bdf,omitempty"`
	SpeedDeg bool   `json:"speed_degraded"`
	WidthDeg bool   `json:"width_degraded"`
	Down     bool   `json:"link_down"`
}

func (t *triage) writeVerdictJSON(code int) {
	_, clearedFP, isFP := t.pexFalsePositive()
	v := verdictJSON{
		BMN:         t.o.bmn,
		VerdictCode: code,
		GPU:         t.gpu,
		Flags: map[string]bool{
			"ecc_uncorrectable":  t.eccUncorr,
			"remap_pending":      t.remapPend,
			"remap_failure":      t.remapFail,
			"nvlink_anomaly":     t.nvlinkAnom,
			"xid_in_dmesg":       t.xid,
			"aer_fatal":          t.aerFatal,
			"ib_fault":           len(t.ibFault) > 0,
			"thermal_fault":      len(t.thermalFault) > 0,
			"pex_false_positive": isFP,
			"self_cleared":       len(t.alertCleared) > 0,
			"eval_ran":           t.evalRan,
			"eval_findings":      len(t.evalFindings) > 0,
		},
		XidLines: t.xidLines,
	}

	for _, bdf := range append(append(append(append([]string{}, t.switchDeg...), t.otherDeg...), t.nvmeDeg...), t.reseatDeg...) {
		r := t.results[bdf]
		if r == nil {
			continue
		}
		v.Devices = append(v.Devices, verdictDevice{
			BDF: r.bdf, Desc: descOnly(r.desc), LnkSta: r.lnksta, Class: r.class,
			Slot: r.slot, Child: r.childBDF,
			SpeedDeg: r.speedDeg, WidthDeg: r.widthDeg, Down: r.down,
		})
		kind := "link degraded"
		switch {
		case r.down:
			kind = "LINK DOWN (width x0)"
		case r.widthDeg && r.speedDeg:
			kind = "speed+width degraded"
		case r.widthDeg:
			kind = "width degraded"
		case r.speedDeg:
			kind = "speed degraded"
		}
		v.Issues = append(v.Issues, fmt.Sprintf("%s @ %s (%s)", kind, r.bdf, descOnly(r.desc)))
		if r.noChild && r.down {
			v.Issues = append(v.Issues, fmt.Sprintf("%s: bridge has NO device behind it — idle-port x0 (METAL-4742 false-positive pattern)", r.bdf))
			v.NextSteps = append(v.NextSteps, fmt.Sprintf("verify %s against golden state / vendor topology BEFORE any physical action (likely false positive)", r.bdf))
		}
		if r.childCapBelow {
			v.Issues = append(v.Issues, fmt.Sprintf("%s: endpoint LnkCap below bridge — wrong part/FW", r.bdf))
			v.NextSteps = append(v.NextSteps, fmt.Sprintf("verify endpoint behind %s against BOM (no DCT action)", r.bdf))
		}
		if r.childLinkDeg {
			v.Issues = append(v.Issues, fmt.Sprintf("%s: endpoint %s reports the degraded link from its side too — physical/signal integrity", r.bdf, r.childBDF))
		}
		if !(r.noChild && r.down) {
			if cs := cheatSheet(r.speedDeg, r.widthDeg, r.down); cs != "" {
				v.NextSteps = append(v.NextSteps, fmt.Sprintf("%s cheat sheet: %s", r.bdf, cs))
			}
		}
		if r.dpcTrig {
			v.Issues = append(v.Issues, fmt.Sprintf("%s: DPC triggered", r.bdf))
		}
		if r.retrainNoise {
			v.Issues = append(v.Issues, fmt.Sprintf("%s: link retraining noise (BadTLP/Rollover)", r.bdf))
		}
	}
	// Self-cleared alert BDFs never enter the degraded lists above; surface
	// them so the orchestrator matrix and fleet rollup can see the pattern.
	for _, bdf := range clearedFP {
		r := t.results[bdf]
		if r == nil {
			continue
		}
		v.Devices = append(v.Devices, verdictDevice{
			BDF: r.bdf, Desc: descOnly(r.desc), LnkSta: r.lnksta, Class: "switch", Slot: r.slot,
		})
		v.Issues = append(v.Issues, fmt.Sprintf("%s: alert BDF at full speed/width on re-check — PEX890xx transient sampling false positive", r.bdf))
	}
	for _, bdf := range t.pcieErrors {
		v.Issues = append(v.Issues, "device unreachable: "+bdf)
	}
	for _, p := range t.ibFault {
		v.Issues = append(v.Issues, "IB/fabric port not ACTIVE: "+p)
	}
	for _, g := range t.thermalFault {
		v.Issues = append(v.Issues, "thermal slowdown: "+g)
	}
	for _, f := range t.evalFindings {
		v.Issues = append(v.Issues, "eval: "+f)
	}
	for name, on := range map[string]bool{
		"uncorrectable ECC errors":   t.eccUncorr,
		"GPU row-remap PENDING":      t.remapPend,
		"GPU row-remap FAILURE":      t.remapFail,
		"NVLink inactive/down links": t.nvlinkAnom,
		"NVRM Xid in dmesg":          t.xid,
		"fatal AER in dmesg":         t.aerFatal,
	} {
		if on {
			v.Issues = append(v.Issues, name)
		}
	}
	if g := t.gpu; g != nil {
		for _, m := range g.MissingBDFs {
			v.Issues = append(v.Issues, "GPU MISSING at "+m+" (absent from lspci)")
		}
		for _, m := range g.RevFF {
			v.Issues = append(v.Issues, m+" reads (rev ff) — fell off the bus")
		}
		if g.Expected > 0 && g.CountSMI < g.Expected {
			v.Issues = append(v.Issues, fmt.Sprintf("only %d/%d GPUs visible to nvidia-smi", g.CountSMI, g.Expected))
		}
	}

	// Advisory next steps mirroring the printed runbook paths.
	adv := func(s string) { v.NextSteps = append(v.NextSteps, "ADVISE: "+s) }
	swOther := append(append([]string{}, t.switchDeg...), t.otherDeg...)
	switch {
	case code == 2:
		adv(fmt.Sprintf("keep node in triage (cwctl flcc node -s triage %s) and present for RMA with the collect bundle + before/after LnkSta", t.o.bmn))
	case isFP:
		adv(fmt.Sprintf("PEX890xx false positive — return to ready, NO RMA: cwctl flcc node -w return-to-ready -m \"sending to ready\" %s (if the same BDF re-alerts across reboots or pairs with XID/AER, treat as genuine)", t.o.bmn))
	case len(swOther) > 0:
		adv(fmt.Sprintf("take node out of production (cwctl flcc node -s triage %s), request 10-15 min DCT AC power drain, re-run: gpuinspect %s (--post-drain)", t.o.bmn, t.o.bmn))
	}
	if code != 2 && len(t.nvmeDeg) > 0 {
		adv("have DCT swap two NVMe drives (not a power cycle), re-run with --post-swap; still degraded -> RMA")
	}
	if code != 2 && len(t.reseatDeg) > 0 {
		adv("request ONE DCT reseat of the GPU/NIC endpoint (slot # in report), re-run with --post-reseat; still degraded -> RMA")
	}
	if code != 2 && len(t.evalFindings) > 0 {
		adv("full GPU evaluation flagged issues — review eval/ in the collect bundle (dcgmi_diag.txt / nvbandwidth.txt / gpu_inventory.json) before returning the node")
	}
	if g := t.gpu; g != nil && code != 2 && (len(g.MissingBDFs) > 0 || len(g.RevFF) > 0) {
		adv("missing/fallen GPU: check dmesg Xid history in bundle, request power drain; if GPU still absent after drain -> RMA for missing GPU")
	}
	if g := t.gpu; g != nil && code != 2 && g.Expected > 0 && g.CountSMI < g.Expected && g.CountLspci >= g.Expected {
		adv("GPUs enumerate on PCIe but nvidia-smi sees fewer — driver/NVML fault path: check nvidia kernel modules + Xid history, driver reload/reboot BEFORE any hardware action")
	}
	if code == 0 && (t.o.postDrain || t.o.postSwap || t.o.postReseat) {
		adv(fmt.Sprintf("issue cleared — run verification (cwctl flcc node -w test -s test %s) then return to production", t.o.bmn))
	}

	if strings.TrimSpace(t.logDir) == "" {
		return
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(t.logDir, "verdict.json"), b, 0o644)
}
