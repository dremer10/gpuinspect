package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Default is the HGX/H100-era healthy NVLink line rate; NVLINK_EXPECTED overrides it on
// other SKUs. Blast radius is display/filtering only — the verdict flag still keys off
// inactive/down lines.
var nvlinkExpected = envOr("NVLINK_EXPECTED", "26.562 GB/s")

// ---------------------------------------------------------------------------
// Collect bundle (runbook step 3 — BEFORE power work)
// ---------------------------------------------------------------------------

func (t *triage) collectBundle() {
	c := t.c
	t.header("Collect bundle (runbook step 3)")
	t.log.p("%s-> %s%s", c.D, t.bundle, c.X)

	// Bulky whole-system captures: command echoed, output to bundle only —
	// the report stays GPU-focused (per-BDF blocks + GPU suite below).
	// EXECUTE CONCURRENTLY, PRINT SEQUENTIALLY: each capture writes its own
	// bundle file, so runQuietCaptures overlaps them in a bounded pool and
	// then prints the `$ cmd` / "captured to" lines in this fixed order.
	// runQuietCaptures returns only after EVERY capture is complete, so
	// nvidia_smi_q.txt exists before runGPUSuite/parseGPUHealth read it.
	caps := []quietCapture{
		{path: filepath.Join(t.bundle, "lspci_vv.txt"), script: "lspci -vv"},
		{path: filepath.Join(t.bundle, "lspci_tree.txt"), script: "lspci -tv"},
		{path: filepath.Join(t.bundle, "dmesg.txt"), script: "dmesg -T 2>/dev/null || dmesg"},
	}
	if t.haveNvme {
		caps = append(caps, quietCapture{path: filepath.Join(t.bundle, "nvme_list.txt"), script: "nvme list -v"})
	}
	if t.haveDmi {
		caps = append(caps, quietCapture{path: filepath.Join(t.bundle, "dmidecode_slots.txt"), script: "dmidecode -t slot"})
	}
	if t.haveNvsmi {
		// Full `nvidia-smi -q` is huge — its own file keeps gpu_full.txt readable.
		caps = append(caps, quietCapture{path: filepath.Join(t.bundle, "nvidia_smi_q.txt"), script: "nvidia-smi -q"})
	}
	t.runQuietCaptures(caps)

	if t.haveNvsmi {
		t.runGPUSuite()
		t.parseGPUHealth()
	}

	t.gpu = t.analyzeGPUs()
	if t.gpu != nil {
		if b, err := json.MarshalIndent(t.gpu, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(t.bundle, "gpu_findings.json"), b, 0o644)
		}
	}

	dmesg, _ := os.ReadFile(filepath.Join(t.bundle, "dmesg.txt"))
	t.xidLines = xidLinesFromDmesg(string(dmesg))
	t.xid = len(t.xidLines) > 0
	t.aerFatal = strings.Contains(string(dmesg), "severity=Fatal") ||
		strings.Contains(string(dmesg), "Uncorrected (Fatal)")

	t.checkFabricThermal()
	t.collectErrHistory()

	// Full GPU evaluation writes into <bundle>/eval/ — must run before the
	// archive step so the tarball carries it back to the laptop.
	if t.o.eval {
		t.runEval()
	}

	if os.Getenv("LOCAL_HINT") != "" {
		// Remote-orchestrated run: the laptop archives the ENTIRE retrieved
		// evidence dir after retrieval — an on-node tarball would only
		// duplicate the bundle inside it and waste transfer.
		t.log.p("%sBundle left unarchived — laptop archives the full evidence dir after retrieval%s", t.c.D, t.c.X)
	} else if err := exec.Command("tar", "-czf", t.bundle+".tar.gz",
		"-C", filepath.Dir(t.bundle), filepath.Base(t.bundle)).Run(); err == nil {
		t.log.p("%sBundle archived:%s %s.tar.gz %s(attach to HO/DO)%s", t.c.G, t.c.X, t.bundle, t.c.D, t.c.X)
	} else {
		t.log.p("%sWARN: bundle archiving FAILED (%v) — raw files kept at %s; tar it manually%s",
			t.c.Y, err, t.bundle, t.c.X)
		// Repoint the operator-facing bundle pointer so later output never
		// references a phantom .tar.gz.
		t.bundleShow = t.bundle + "  (UNARCHIVED — tar failed)"
		t.bundleNote = ""
	}
}

// quietCapture is one bulky bundle capture: the shell script to run and the
// file its output lands in. lines is filled by runQuietCaptures so the
// "captured to <file> (N lines)" report line can print after the fact.
type quietCapture struct {
	path, script string
	lines        int
}

// runQuietCaptures is the concurrent counterpart of bashQuiet: the captures
// execute in a bounded worker pool (each writes only its own file), then the
// `$ cmd` / "captured to" lines print sequentially in input order from the
// calling goroutine — the logger is not goroutine-safe and report ordering
// matters (EXECUTE CONCURRENTLY, PRINT SEQUENTIALLY). A single capture skips
// the pool and runs inline via bashQuiet (identical behavior, no goroutines).
func (t *triage) runQuietCaptures(caps []quietCapture) {
	if len(caps) == 1 {
		t.bashQuiet(caps[0].path, caps[0].script)
		return
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range caps {
		wg.Add(1)
		go func(c *quietCapture) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out := cmdOut("bash", "-c", c.script)
			_ = os.WriteFile(c.path, []byte(out), 0o644)
			c.lines = len(strings.Split(out, "\n"))
		}(&caps[i])
	}
	wg.Wait()
	for _, c := range caps {
		t.echoCmd(c.script)
		t.log.p("%s  captured to %s (%d lines)%s", t.c.D, c.path, c.lines, t.c.X)
	}
}

// gpuSuiteSection is one GPU-runbook command: the gpu_full.txt section title,
// the shell script actually run, and whether its output is shown inline.
type gpuSuiteSection struct {
	title  string
	script string
	show   bool // echo output inline; false = duplicate detail, file only
}

// gpuSuiteSections builds the runbook section list. The three nvidia-smi -q
// derived sections grep the ALREADY-CAPTURED qFile (nvidia_smi_q.txt from
// collectBundle) instead of re-invoking `nvidia-smi -q` (~2s each); the
// displayed command line is the command actually run. Pure function.
func gpuSuiteSections(qFile string) []gpuSuiteSection {
	stateGrep := `grep -E "(^GPU|GPU UUID|Serial Number|    State)" ` + qFile
	eccGrep := `grep -E "(^GPU|GPU UUID|Serial Number|ECC Errors|Volatile|Uncorrectable)" ` + qFile
	// platform.module_id is unsupported on some driver versions; capturing
	// the raw error in the bundle/report is fine.
	return []gpuSuiteSection{
		{"nvidia-smi", "nvidia-smi", true},
		{"list-gpus", "nvidia-smi --list-gpus", true},
		{"q grep state", stateGrep, false},
		{"ecc counters", eccGrep, false},
		{"ecc nonzero", eccGrep + ` | grep -vE " : 0$"`, true},
		{"gpu inventory (module id)", `nvidia-smi --query-gpu=index,name,gpu_serial,platform.module_id,pci.bus_id --format=csv`, true},
		{"gpu inventory", `nvidia-smi --query-gpu=index,pci.bus_id,serial,uuid,name --format=csv`, false},
		{"remapped rows", `nvidia-smi --query-remapped-rows=gpu_name,gpu_bus_id,remapped_rows.correctable,remapped_rows.uncorrectable,remapped_rows.pending,remapped_rows.failure --format=csv`, true},
		{"topo matrix", "nvidia-smi topo --matrix", true},
		{"p2p", "nvidia-smi topo -p2p n", false},
		{"nvlink status", "nvidia-smi nvlink -s", true},
		{"nvlink anomalies", `nvidia-smi nvlink -s | grep -v "` + nvlinkExpected + `"`, true},
		{"nvlink -R", "nvidia-smi nvlink -R", false},
	}
}

// runGPUSuite runs the GPU-runbook nvidia-smi suite. Every command line is
// echoed to the terminal + triage log, and the substantive outputs are shown
// inline (capped by GPUINSPECT_SHOW_MAX_LINES); pure-duplicate sections go to
// gpu_full.txt only. The whole suite is preserved in gpu_full.txt with the
// legacy "===== section =====" layout that parseGPUHealth-era tooling knows.
//
// EXECUTE CONCURRENTLY, PRINT SEQUENTIALLY: the sections are all read-only
// nvidia-smi queries / greps over the captured -q file, so a bounded pool
// overlaps their latency; the logger is not goroutine-safe and gpu_full.txt
// must keep its fixed section order, so all printing and file assembly happen
// afterwards from this goroutine in the original order.
func (t *triage) runGPUSuite() {
	t.header("GPU suite (nvidia-smi runbook commands)")
	sections := gpuSuiteSections(filepath.Join(t.bundle, "nvidia_smi_q.txt"))
	outs := make([]string, len(sections))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range sections {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outs[i] = cmdOut("bash", "-c", sections[i].script)
		}(i)
	}
	wg.Wait()
	path := filepath.Join(t.bundle, "gpu_full.txt")
	var full strings.Builder
	for i, s := range sections {
		fmt.Fprintf(&full, "===== %s =====\n%s\n", s.title, outs[i])
		t.echoCmd(s.script)
		if s.show {
			t.echoOutput(outs[i], "gpu_full.txt ("+s.title+")")
		} else {
			t.log.p("%s  → gpu_full.txt (%s)%s", t.c.D, s.title, t.c.X)
		}
	}
	_ = os.WriteFile(path, []byte(full.String()), 0o644)
}

var (
	dpcTrigRe = regexp.MustCompile(`DpcSta:.*Trigger\+|DpcCtl:.*Trigger:1`)
	retrainRe = regexp.MustCompile(`CESta:.*(BadTLP\+|BadDLLP\+|Rollover\+)`)
)

// xidLinesFromDmesg extracts the NVRM Xid lines (trimmed, most recent 20) so
// the AI layer can map each Xid number against the XID Bible — the boolean
// flag alone cannot say WHICH Xid fired. Pure function.
func xidLinesFromDmesg(dmesg string) []string {
	var out []string
	for _, line := range strings.Split(dmesg, "\n") {
		if strings.Contains(line, "NVRM: Xid") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	if len(out) > 20 {
		out = out[len(out)-20:]
	}
	return out
}

// "Not Active" also ends in "Active", so anchor on the colon.
var thermalReasonRe = regexp.MustCompile(`^(HW Thermal Slowdown|SW Thermal Slowdown)\s*:\s*Active$`)

// checkFabricThermal verifies the two return-to-ready runbook gates the PCIe
// and GPU suites don't cover — "no accompanying IB/fabric or thermal fault":
//   - IB/fabric: every InfiniBand-layer port under /sys/class/infiniband must
//     be ACTIVE (sysfs only, no ibstat dependency). Ethernet-layer ports are
//     reported in the bundle but don't gate — unused frontend ports are
//     legitimately down.
//   - Thermal: any GPU reporting HW/SW Thermal Slowdown Active in the
//     already-captured nvidia-smi -q output.
func (t *triage) checkFabricThermal() {
	ports, _ := filepath.Glob("/sys/class/infiniband/*/ports/*")
	var lines []string
	for _, p := range ports {
		layer := readSysfs(filepath.Join(p, "link_layer"))
		state := readSysfs(filepath.Join(p, "state"))     // e.g. "4: ACTIVE"
		phys := readSysfs(filepath.Join(p, "phys_state")) // e.g. "5: LinkUp"
		dev := filepath.Base(filepath.Dir(filepath.Dir(p)))
		port := dev + "/port" + filepath.Base(p)
		lines = append(lines, fmt.Sprintf("%-24s link_layer=%-10s state=%-12s phys_state=%s", port, layer, state, phys))
		if strings.EqualFold(layer, "InfiniBand") && !strings.Contains(state, "ACTIVE") {
			t.ibFault = append(t.ibFault, port+" "+state)
		}
	}
	if len(lines) > 0 {
		_ = os.WriteFile(filepath.Join(t.bundle, "ib_ports.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}

	q, _ := os.ReadFile(filepath.Join(t.bundle, "nvidia_smi_q.txt"))
	t.thermalFault = thermalEvents(string(q))
}

// thermalEvents returns one "GPU <busid>: <reason> Active" line per thermal
// slowdown currently asserted in nvidia-smi -q output. HW Power Brake and
// plain "HW Slowdown" are deliberately excluded — they are power events, not
// the runbook's thermal-fault gate.
func thermalEvents(q string) []string {
	var out []string
	gpu := "?"
	for _, line := range strings.Split(q, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "GPU 0000") {
			gpu = strings.TrimPrefix(l, "GPU ")
		}
		if m := thermalReasonRe.FindStringSubmatch(l); m != nil {
			out = append(out, "GPU "+gpu+": "+m[1]+" Active")
		}
	}
	return out
}

// collectErrHistory captures the per-BDF AER/DPC error history for every
// degraded bridge and its endpoint (width runbook step 5), plus every
// self-cleared alert BDF — a port that recovered by re-check time can still
// carry the retrain-noise/DPC counters that expose a flapping link. This must
// happen BEFORE any physical action: once the link retrains, the counters
// reset to zero and the bundle copy is the only record left for the ticket.
func (t *triage) collectErrHistory() {
	var targets []string
	for bdf, r := range t.results {
		if !r.degraded {
			continue
		}
		targets = append(targets, bdf)
		if r.childBDF != "" {
			targets = append(targets, r.childBDF)
		}
	}
	targets = append(targets, t.alertCleared...)
	if len(targets) == 0 {
		return
	}
	t.header("PCIe error history (width runbook step 5 — captured BEFORE physical action)")
	t.log.p("%sCounters reset when the link retrains; the bundle copy is the record.%s", t.c.D, t.c.X)
	for _, b := range targets {
		vvv := cmdOut("lspci", "-vvv", "-s", b)
		// BDFs are regex-validated at parse time / read from sysfs dir names,
		// so interpolating them into the grep pattern is safe.
		t.bashShow(filepath.Join(t.bundle, "errhist_"+sanitize(b)+".txt"), fmt.Sprintf(
			`echo "===== dmesg (%s | aer | dpc) ====="
(dmesg -T 2>/dev/null || dmesg) | grep -iE '%s|aer|dpc|corrected|fatal' | tail -100
echo; echo "===== lspci -vvv -s %s (AER section) ====="
lspci -vvv -s %s 2>/dev/null | grep -A 6 'Advanced Error Reporting'
echo; echo "===== lspci -vvv -s %s (DPC / HeaderLog / CESta indicators) ====="
lspci -vvv -s %s 2>/dev/null | grep -E 'DpcCtl:|DpcSta:|HeaderLog:|CESta:|UESta:'`,
			b, strings.TrimPrefix(b, "0000:"), b, b, b, b))
		owner := t.results[b]
		if owner == nil { // child BDF: attach indicators to its bridge
			for _, r := range t.results {
				if r.childBDF == b {
					owner = r
					break
				}
			}
		}
		if dpcTrigRe.MatchString(vvv) {
			t.log.p("%s%s: DPC has TRIGGERED (Downstream Port Containment fired)%s", t.c.R, b, t.c.X)
			if owner != nil {
				owner.dpcTrig = true
			}
		}
		if retrainRe.MatchString(vvv) {
			t.log.p("%s%s: correctable-error noise (BadTLP/BadDLLP/Rollover) — link is retraining%s", t.c.Y, b, t.c.X)
			if owner != nil {
				owner.retrainNoise = true
			}
		}
	}
}

var nonzeroRe = regexp.MustCompile(` : [1-9][0-9]*`)

// volatileUncorrNonzero reports nonzero Uncorrectable ECC counters in the
// VOLATILE section of nvidia-smi -q only. Aggregate counters are lifetime
// history (they survive remapping and resets) and are NOT grounds for an RMA
// verdict — keying off them falsely RMA'd healthy nodes with old ECC history.
func volatileUncorrNonzero(q string) bool {
	inVolatile := false
	for _, line := range strings.Split(q, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "Volatile":
			inVolatile = true
		case t == "Aggregate" || strings.HasPrefix(t, "Aggregate "):
			inVolatile = false
		}
		if inVolatile && strings.Contains(t, "Uncorrectable") && nonzeroRe.MatchString(t) {
			return true
		}
	}
	return false
}

func (t *triage) parseGPUHealth() {
	q, _ := os.ReadFile(filepath.Join(t.bundle, "nvidia_smi_q.txt"))
	if volatileUncorrNonzero(string(q)) {
		t.eccUncorr = true
	}

	rr := cmdOut("nvidia-smi",
		"--query-remapped-rows=gpu_name,gpu_bus_id,remapped_rows.correctable,remapped_rows.uncorrectable,remapped_rows.pending,remapped_rows.failure",
		"--format=csv")
	lines := strings.Split(strings.TrimSpace(rr), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // header
		}
		f := strings.Split(line, ",")
		if len(f) < 6 {
			continue
		}
		pend := strings.TrimSpace(strings.ToLower(f[4]))
		fail := strings.TrimSpace(strings.ToLower(f[5]))
		if pend == "yes" || pend == "1" {
			t.remapPend = true
		}
		if fail == "yes" || fail == "1" {
			t.remapFail = true
		}
	}

	nvlinkOut := cmdOut("nvidia-smi", "nvlink", "-s")
	t.nvlinkAnom = nvlinkAnomalous(nvlinkOut)
	if !t.nvlinkAnom && strings.Contains(strings.ToLower(nvlinkOut), nvlinkAllInactiveMsg) {
		t.log.p("%sNVLink  : all links inactive on a single-GPU node — no NVLink fabric, not an anomaly%s", t.c.D, t.c.X)
	}
}

// NVML prints this (per GPU) when NVLink is entirely unused on the node.
const nvlinkAllInactiveMsg = "unable to retrieve nvlink information as all links are inactive"

// nvlinkAnomalous reports NVLink links not at the expected rate in
// `nvidia-smi nvlink -s` output. A single-GPU node where NVML says all links
// are inActive has no NVLink fabric at all (a lone GH200 superchip has nothing
// to link to) — that shape is normal, not an anomaly. Multi-GPU nodes with the
// same message, or any per-link inactive/down line, stay anomalous. Pure function.
func nvlinkAnomalous(out string) bool {
	gpus, allInactive := 0, false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, nvlinkExpected) {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "GPU ") {
			gpus++
			continue
		}
		l := strings.ToLower(line)
		if strings.Contains(l, nvlinkAllInactiveMsg) {
			allInactive = true
			continue
		}
		if strings.Contains(l, "inactive") || strings.Contains(l, "down") {
			return true
		}
	}
	return allInactive && gpus > 1
}
