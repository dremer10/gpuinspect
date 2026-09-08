package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

const (
	// GPU-focused default: only NVIDIA devices (GPUs, NVSwitches, NVIDIA
	// bridges) are auto-discovered. Alert BDFs are ALWAYS triaged regardless
	// — they are the fallout reason. --all-pci restores the wide sweep.
	discoverVendorsGPU = `^0x10de$`             // NVIDIA
	discoverVendorsAll = `^0x(10de|1000|15b3)$` // + Broadcom/LSI PEX, Mellanox
	defaultStateDir    = "/var/tmp/node_pci_triage"
)

// "BDF:width" pairs where narrower width is by design
var widthExceptions = map[string]bool{
	// "0000:5c:00.0:8": true,
}

// ---------------------------------------------------------------------------
// Local mode — the triage itself
// ---------------------------------------------------------------------------

type devResult struct {
	bdf, desc, lnksta, before string
	degraded, down            bool
	speedDeg, widthDeg        bool
	class                     string // switch | nvme | reseat | other (remediation route)
	maxSpeed, maxWidth        string // bridge/device LnkCap, for endpoint ceiling check
	slot                      string // physical Slot # from SltCap (include in tickets)
	childBDF, childDesc       string // endpoint behind a degraded bridge (width runbook)
	childClass                string // gpu | nic | nvme | other
	childCapBelow             bool   // endpoint LnkCap below bridge -> wrong part/FW, no DCT action
	childLinkDeg              bool   // endpoint LnkSta degraded too -> both sides agree: physical/signal integrity
	noChild                   bool   // bridge with nothing behind it (idle port false-positive pattern)
	dpcTrig, retrainNoise     bool   // AER/DPC indicators from error history (width runbook step 5)
}

type triage struct {
	o                                     *options
	c                                     palette
	log                                   *logger
	ts, logDir, stateDir, logFile, bundle string
	rerun, bundleShow, bundleNote         string
	switchDeg, nvmeDeg, otherDeg          []string
	reseatDeg                             []string
	linkDown, pcieErrors                  []string
	alertCleared                          []string // alert BDFs (PEX switch ports) healthy at re-check — self-cleared transient
	ibFault                               []string // IB-layer fabric ports not ACTIVE (dev/port + state)
	thermalFault                          []string // GPUs with HW/SW Thermal Slowdown Active (nvidia-smi -q)
	results                               map[string]*devResult
	eccUncorr, remapPend, remapFail       bool
	nvlinkAnom, aerFatal, xid             bool
	haveNvsmi, haveNvme, haveDmi          bool
	gpu                                   *gpuFindings // filled by analyzeGPUs (gpuanalysis.go)
	xidLines                              []string     // NVRM Xid dmesg lines (last 20) for the AI's XID Bible mapping
	evalRan                               bool         // --eval ran (eval.go)
	evalFindings                          []string     // eval faults — gate the verdict
	evalNotes                             []string     // eval observations — informational only
	pciTreeCache                          string       // lspci -vt, run once (width runbook step 2 option A)
	pciTreeDone                           bool
}

func runLocal(o *options) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "ERROR: run as root (sudo).")
		return 3
	}
	t := &triage{o: o, c: colors(), results: map[string]*devResult{}}
	t.ts = time.Now().Format("20060102_150405")
	t.logDir = envOr("LOGDIR", ".")
	t.stateDir = envOr("STATEDIR", defaultStateDir)
	t.logFile = filepath.Join(t.logDir, "pci_triage_"+t.ts+".log")
	t.bundle = filepath.Join(t.logDir, "collect_bundle_"+t.ts)

	for _, d := range []string{t.logDir, t.stateDir, t.bundle} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: mkdir %s: %v\n", d, err)
			return 3
		}
	}
	f, err := os.Create(t.logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot create log file: %v\n", err)
		return 3
	}
	defer func() { _ = f.Close() }()
	t.log = &logger{file: f}

	// Laptop-relative instructions when invoked via remote mode
	t.rerun = os.Getenv("RERUN_CMD")
	if t.rerun == "" {
		t.rerun = fmt.Sprintf("sudo %s --bmn %s", os.Args[0], o.bmn)
	}
	if hint := os.Getenv("LOCAL_HINT"); hint != "" {
		// The laptop archives the whole retrieved evidence dir as <dir>.tar.gz
		// — one dir + one archive per node for HO/DO tickets.
		t.bundleShow = hint + ".tar.gz"
		t.bundleNote = " (on your laptop after auto-retrieval)"
	} else {
		t.bundleShow = t.bundle + ".tar.gz"
	}

	c := t.c
	hostname, _ := os.Hostname()
	t.log.p("%sNodePCILinkSpeedUnexpected triage%s %s| host: %s | %s | v%s%s",
		c.B, c.X, c.D, hostname, time.Now().UTC().Format("2006-01-02T15:04:05Z"), version, c.X)
	t.log.p("%sflags: post_drain=%v post_swap=%v triaged=%v bmn=%s%s",
		c.D, o.postDrain, o.postSwap, o.triaged, o.bmn, c.X)

	t.haveNvsmi = haveCmd("nvidia-smi")
	t.haveNvme = haveCmd("nvme")
	t.haveDmi = haveCmd("dmidecode")

	// --temps: live thermal readout only — needs nvidia-smi, not lspci, and
	// none of the PCIe triage below.
	if o.temps {
		return t.runTemps()
	}

	if !haveCmd("lspci") {
		t.log.p("%sERROR: lspci not found (install pciutils).%s", c.R, c.X)
		return 3
	}

	bdfs, ok := t.discoverTargets()
	if !ok {
		// In BMN-driven runs (GPUINSPECT_EXPECTED_GPUS set by the laptop
		// orchestrator) an empty target list is legitimate — the node may
		// have GPU-only symptoms with no PCI link alert. Standalone runs
		// keep the fail-closed behavior.
		if envOr("GPUINSPECT_EXPECTED_GPUS", "") == "" {
			t.log.p("%sERROR: could not auto-discover PCIe targets — pass the BDF(s) from the alert explicitly:%s", c.R, c.X)
			t.log.p("    %s%s <alert-BDF>%s", c.B, t.rerun, c.X)
			return 3
		}
		t.log.p("%sNOTE: no PCIe targets (no alert BDFs, discovery empty) — running GPU analysis only.%s", c.Y, c.X)
		bdfs = nil
	}
	if len(bdfs) > 0 {
		t.header("PCIe link check (runbook step 2)")
		for _, b := range bdfs {
			t.checkBDF(b)
		}
	}
	t.collectBundle()
	code := t.verdict()
	t.writeVerdictJSON(code)
	return code
}

func (t *triage) header(s string) {
	t.log.p("")
	t.log.p("%s━━━ %s ━━━%s", t.c.C, s, t.c.X)
}

// discoverTargets returns the BDFs to triage. ok=false means auto-discovery
// found nothing and the user passed none — callers must fail closed (exit 3)
// rather than guess: BDFs borrowed from another SKU make nonexistent devices
// land in pcieErrors, cascading into a false PRESENT-FOR-RMA verdict.
func (t *triage) discoverTargets() ([]string, bool) {
	t.header("Target discovery")
	c := t.c
	if len(t.o.bdfs) > 0 {
		t.log.p("Using alert/user BDFs: %s%s%s", c.B, strings.Join(t.o.bdfs, " "), c.X)
		return t.o.bdfs, true
	}
	vendors := discoverVendorsGPU
	if t.o.allPCI {
		vendors = discoverVendorsAll
		t.log.p("%s--all-pci: wide sweep (NVIDIA + Broadcom PEX + Mellanox)%s", c.Y, c.X)
	} else {
		t.log.p("%sGPU-focused discovery (NVIDIA devices only; --all-pci widens the sweep)%s", c.D, c.X)
	}
	vendRe := regexp.MustCompile(vendors)
	var found []string
	entries, _ := filepath.Glob("/sys/bus/pci/devices/*")
	for _, d := range entries {
		if readSysfs(filepath.Join(d, "max_link_speed")) == "" {
			continue
		}
		if vendRe.MatchString(readSysfs(filepath.Join(d, "vendor"))) {
			found = append(found, filepath.Base(d))
		}
	}
	if len(found) == 0 {
		return nil, false
	}
	t.log.p("Auto-discovered %d device(s): %s", len(found), strings.Join(found, " "))
	return found, true
}

func classify(desc string) string {
	l := strings.ToLower(desc)
	switch {
	case regexp.MustCompile(`pex[0-9]+|plx`).MatchString(l):
		return "switch"
	case strings.Contains(l, "non-volatile memory") || strings.Contains(l, "nvme") || strings.Contains(l, "7450"):
		return "nvme"
	case regexp.MustCompile(`bluefield.*bridge`).MatchString(l):
		return "nvme"
	case strings.Contains(l, "nvidia") && (strings.Contains(l, "3d controller") || strings.Contains(l, "vga")):
		return "gpu"
	case strings.Contains(l, "connectx") ||
		strings.Contains(l, "ethernet controller") ||
		strings.Contains(l, "infiniband controller") ||
		strings.Contains(l, "network controller"):
		return "nic"
	}
	return "other"
}

func (t *triage) checkBDF(bdf string) {
	c := t.c
	devpath := "/sys/bus/pci/devices/" + bdf

	info := strings.TrimSpace(cmdOut("lspci", "-s", bdf))
	if info == "" || !dirExists(devpath) {
		t.log.p("%sERROR%s  %s  device not found", c.R, c.X, bdf)
		t.pcieErrors = append(t.pcieErrors, bdf)
		return
	}
	r := &devResult{bdf: bdf, desc: info}
	t.results[bdf] = r

	vvv := cmdOut("lspci", "-s", bdf, "-vvv")
	for _, line := range strings.Split(vvv, "\n") {
		if strings.Contains(line, "LnkSta:") {
			r.lnksta = strings.TrimSpace(line)
			break
		}
	}
	// Physical slot the chassis uses — include in any DCT or RMA ticket.
	if m := slotNumRe.FindStringSubmatch(vvv); m != nil {
		r.slot = "#" + m[1]
	}

	maxS := readSysfs(devpath + "/max_link_speed")
	maxW := readSysfs(devpath + "/max_link_width")
	curS := readSysfs(devpath + "/current_link_speed")
	curW := readSysfs(devpath + "/current_link_width")
	if maxS == "" || maxW == "" || curS == "" || curW == "" {
		t.log.p("%sERROR%s  %s  missing sysfs link attributes", c.R, c.X, bdf)
		t.pcieErrors = append(t.pcieErrors, bdf)
		return
	}
	// Fail closed on garbled sysfs reads: numLT/Atoi silently treat
	// unparseable values as "not degraded", which would print OK for a
	// device we could not actually assess.
	if _, okC := gtNum(curS); !okC {
		t.log.p("%sERROR%s  %s  unparseable link speed (current=%q max=%q)", c.R, c.X, bdf, curS, maxS)
		t.pcieErrors = append(t.pcieErrors, bdf)
		return
	}
	if _, okM := gtNum(maxS); !okM {
		t.log.p("%sERROR%s  %s  unparseable link speed (current=%q max=%q)", c.R, c.X, bdf, curS, maxS)
		t.pcieErrors = append(t.pcieErrors, bdf)
		return
	}
	curWInt, errCW := strconv.Atoi(curW)
	maxWInt, errMW := strconv.Atoi(maxW)
	if errCW != nil || errMW != nil {
		t.log.p("%sERROR%s  %s  unparseable link width (current=%q max=%q)", c.R, c.X, bdf, curW, maxW)
		t.pcieErrors = append(t.pcieErrors, bdf)
		return
	}
	r.maxSpeed, r.maxWidth = maxS, maxW

	// before/after snapshot
	snap := filepath.Join(t.stateDir, sanitize(bdf)+".lnksta")
	if b, err := os.ReadFile(snap); err == nil {
		r.before = strings.TrimSpace(string(b))
	}
	_ = os.WriteFile(snap, []byte(fmt.Sprintf("%s | %s x%s | %s\n",
		time.Now().UTC().Format("2006-01-02T15:04:05Z"), curS, curW, r.lnksta)), 0o644)

	shortDesc := descOnly(info)
	t.log.p("")
	t.log.p("%s┌─ %s%s", c.B, bdf, c.X)
	t.log.p("%s│%s Device  : %s", c.B, c.X, shortDesc)
	t.log.p("%s│%s Cap     : %s x%s", c.B, c.X, maxS, maxW)
	t.log.p("%s│%s Current : %s x%s", c.B, c.X, curS, curW)
	t.log.p("%s│%s LnkSta  : %s", c.B, c.X, orNA(r.lnksta))
	if r.slot != "" {
		t.log.p("%s│%s Slot    : %s", c.B, c.X, r.slot)
	}
	if r.before != "" {
		t.log.p("%s│%s Previous: %s%s%s", c.B, c.X, c.D, r.before, c.X)
	}

	if curWInt == 0 {
		t.log.p("%s│%s %sALERT   : LINK DOWN — width x0, no active lanes (link failed to train)%s", c.B, c.X, c.R, c.X)
		r.degraded, r.down, r.widthDeg = true, true, true
	} else {
		if numLT(curS, maxS) {
			t.log.p("%s│%s %sALERT   : speed degraded (%s < %s)%s", c.B, c.X, c.R, curS, maxS, c.X)
			r.degraded, r.speedDeg = true, true
		} else {
			t.log.p("%s│%s %sOK      : speed%s", c.B, c.X, c.G, c.X)
		}
		if curWInt < maxWInt {
			if widthExceptions[bdf+":"+curW] {
				t.log.p("%s│%s %sNOTE    : width x%s < x%s (known exception)%s", c.B, c.X, c.Y, curW, maxW, c.X)
			} else {
				t.log.p("%s│%s %sALERT   : width degraded (x%s < x%s)%s", c.B, c.X, c.R, curW, maxW, c.X)
				r.degraded, r.widthDeg = true, true
			}
		} else {
			t.log.p("%s│%s %sOK      : width%s", c.B, c.X, c.G, c.X)
		}
	}
	if strings.Contains(strings.ToLower(r.lnksta), "downgraded") {
		t.log.p("%s│%s %sALERT   : LnkSta reports (downgraded)%s", c.B, c.X, c.R, c.X)
		r.degraded = true
		if !r.widthDeg {
			r.speedDeg = true
		}
	}

	if r.degraded {
		// Width runbook step 1 — the raw bridge-side link state for the ticket.
		t.boxCmd("lspci -vvv -s " + bdf + " | grep -E 'LnkCap:|LnkSta:|Slot #'")
		t.boxOut(grepLines(vvv, bridgeLinkRe))
		// Width runbook steps 2-4: an alert BDF that is a bridge names the
		// downstream port, not the failing part — map it to the endpoint
		// behind it and route remediation by what we find there.
		route := ""
		if isBridge(bdf) {
			route = t.inspectBridge(r)
		}
		if route == "" {
			route = routeFor(classify(info))
		}
		r.class = route
		// Width-runbook interpretation cheat sheet (skip the idle-port
		// no-child pattern — its METAL-4742 note already says what to do).
		if !r.noChild {
			if cs := cheatSheet(r.speedDeg, r.widthDeg, r.down); cs != "" {
				t.log.p("%s│%s %sRunbook : %s%s", c.B, c.X, c.Y, cs, c.X)
			}
		}
		suffix := ""
		if r.down {
			suffix = fmt.Sprintf("  %s[LINK DOWN]%s", c.R, c.X)
			t.linkDown = append(t.linkDown, bdf)
		}
		t.log.p("%s└─%s Class   : %s%s%s%s", c.B, c.X, c.Y, r.class, c.X, suffix)
		switch r.class {
		case "switch":
			t.switchDeg = append(t.switchDeg, bdf)
		case "nvme":
			t.nvmeDeg = append(t.nvmeDeg, bdf)
		case "reseat":
			t.reseatDeg = append(t.reseatDeg, bdf)
		default:
			t.otherDeg = append(t.otherDeg, bdf)
		}
	} else {
		// An alert-provided PEX switch port that re-checks at full speed/width
		// is the self-cleared transient from the PEX890xx false-positive
		// runbook: the health sweep sampled the port while its link partner
		// was trained down or idle (ASPM / probe race).
		if len(t.o.bdfs) > 0 && classify(info) == "switch" {
			t.alertCleared = append(t.alertCleared, bdf)
			// Step 1 raw evidence that the alert BDF re-checks at full link.
			t.boxCmd("lspci -vvv -s " + bdf + " | grep -E 'LnkCap:|LnkSta:|Slot #'")
			t.boxOut(grepLines(vvv, bridgeLinkRe))
			t.log.p("%s└─%s %sHEALTHY%s  %s(alert BDF at full speed/width on re-check — PEX890xx transient sampling false positive)%s",
				c.B, c.X, c.G, c.X, c.Y, c.X)
		} else {
			t.log.p("%s└─%s %sHEALTHY%s", c.B, c.X, c.G, c.X)
		}
	}
}

func (t *triage) nvmeSlotMap(bdf string) {
	short := strings.TrimPrefix(bdf, "0000:")
	if t.haveNvme {
		// Width runbook step 3b — match the Address column to the child BDF.
		t.echoCmd("nvme list -v | grep " + short)
		for _, line := range strings.Split(cmdOut("nvme", "list", "-v"), "\n") {
			if strings.Contains(line, short) || strings.Contains(line, bdf) {
				t.log.p("  NVMe   : %s", strings.TrimSpace(line))
			}
		}
	}
	if t.haveDmi {
		t.echoCmd("dmidecode -t slot   # match slot designation for " + bdf)
		lastDesignation := ""
		for _, line := range strings.Split(cmdOut("dmidecode", "-t", "slot"), "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "Designation:") {
				lastDesignation = strings.TrimSpace(strings.TrimPrefix(l, "Designation:"))
			}
			if strings.Contains(l, bdf) && lastDesignation != "" {
				t.log.p("  Slot   : %s", lastDesignation)
			}
		}
	}
}
