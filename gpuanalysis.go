package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// GPU analysis (runs on-node; fills triage.gpu / gpuFindings)
//
// Count-based detection (nvidia-smi vs lspci vs expected) catches most missing
// GPUs; the platform golden-set table pins the exact BDF that vanished on SKUs
// we know, and "(rev ff)" flags devices still enumerated but fallen off the bus.
// ---------------------------------------------------------------------------

// gpuPlatform maps a (manufacturer, GPU model) pair to the short BDFs a healthy
// node of that SKU shows in lspci. Matching is substring on both keys — adding
// a SKU later is one line in gpuPlatforms.
type gpuPlatform struct {
	manufacturer string   // substring of `dmidecode -s system-manufacturer`
	model        string   // substring of the GPU model string
	bdfs         []string // short-form BDFs (no "0000:" prefix), lowercase
}

var gpuPlatforms = []gpuPlatform{
	// Dell XE9680 H100 SXM5 — healthy 8-GPU set
	{"Dell", "H100", []string{
		"19:00.0", "3b:00.0", "4c:00.0", "5d:00.0",
		"9b:00.0", "bb:00.0", "cb:00.0", "db:00.0",
	}},
}

// expectedGPUCount derives the expected GPU count from the node-reported
// allocatable and the GPU model. GPUINSPECT_EXPECTED_GPUS carries the BMN's
// allocatable nvidia.com/gpu — but kubelet re-reports that DOWN when a GPU
// falls off the bus, so the fault would define its own expectation (a 7-GPU
// node "expecting" 7). The 8-GPU platform default is therefore a FLOOR for
// known HGX models, never just a fallback. Pure function.
func expectedGPUCount(reported int, model string) int {
	// Grace superchips embed HGX accelerator names as substrings ("GH200"
	// contains "H200", "GB200" contains "B200") but are not 8-GPU HGX boards —
	// a GH200 node has ONE GPU (GB200 trays vary, allocatable is the truth).
	// Floor at 1 so a GPU fallen off the bus (which drags allocatable to 0)
	// is still caught.
	if strings.Contains(model, "GH200") || strings.Contains(model, "GB200") {
		if reported < 1 {
			return 1
		}
		return reported
	}
	for _, m := range []string{"H100", "H200", "B200"} {
		if strings.Contains(model, m) && reported < 8 {
			return 8
		}
	}
	return reported
}

// missingGPUBDFs returns the golden-set BDFs absent from lspci for the matched
// platform, prefixed "0000:". nil when the platform is not in the table (the
// count-based check covers those) or when nothing is missing. Pure function.
func missingGPUBDFs(manufacturer, gpuModel, lspciOut string) []string {
	var plat *gpuPlatform
	for i := range gpuPlatforms {
		p := &gpuPlatforms[i]
		if strings.Contains(manufacturer, p.manufacturer) && strings.Contains(gpuModel, p.model) {
			plat = p
			break
		}
	}
	if plat == nil {
		return nil
	}
	lines := strings.Split(strings.ToLower(lspciOut), "\n")
	var missing []string
	for _, bdf := range plat.bdfs {
		found := false
		for _, line := range lines {
			if strings.HasPrefix(line, strings.ToLower(bdf)) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, "0000:"+bdf)
		}
	}
	return missing
}

// gateMissingBDFs gates the golden-set result on an actually-short lspci
// count: the platform BDF table exists to pin WHICH BDF vanished when GPUs
// are missing, never to second-guess a full complement. Different configs of
// the same SKU place their GPUs at different addresses — Dell XE9680 s7xg5724
// has its 8 H100s at 1a/40/53/66/9c/c0/d2/e4 vs the table's 19/3b/… — which
// used to raise false "GPU MISSING" alerts on a healthy 8/8 node. Pure function.
func gateMissingBDFs(expected, countLspci int, missing []string) []string {
	if expected > 0 && countLspci < expected {
		return missing
	}
	return nil
}

// isLspciNvidiaGPU reports whether an lspci line is an NVIDIA 3D/VGA controller.
func isLspciNvidiaGPU(line string) bool {
	return strings.Contains(line, "NVIDIA") &&
		(strings.Contains(line, "3D controller") || strings.Contains(line, "VGA compatible controller"))
}

// countLspciNvidiaGPUs counts NVIDIA 3D/VGA controller lines. Pure function.
func countLspciNvidiaGPUs(lspciOut string) int {
	n := 0
	for _, line := range strings.Split(lspciOut, "\n") {
		if isLspciNvidiaGPU(line) {
			n++
		}
	}
	return n
}

// countSMIGPUs counts "GPU " lines from `nvidia-smi -L`. Pure function.
func countSMIGPUs(smiL string) int {
	n := 0
	for _, line := range strings.Split(smiL, "\n") {
		if strings.HasPrefix(line, "GPU ") {
			n++
		}
	}
	return n
}

// gpuModelFromSMI extracts the model from the first `nvidia-smi -L` line,
// e.g. "GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-…)" -> "NVIDIA H100 80GB HBM3".
// Pure function; "" when unparseable.
func gpuModelFromSMI(smiL string) string {
	for _, line := range strings.Split(smiL, "\n") {
		if !strings.HasPrefix(line, "GPU ") {
			continue
		}
		s := line
		if i := strings.Index(s, ": "); i >= 0 {
			s = s[i+2:]
		} else {
			return ""
		}
		if i := strings.Index(s, " (UUID"); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSpace(s)
	}
	return ""
}

// gpuModelFromLspci extracts the controller description from the first NVIDIA
// GPU line, e.g. "GH100 [H100 SXM5 80GB]". Pure function; "" when none.
func gpuModelFromLspci(lspciOut string) string {
	for _, line := range strings.Split(lspciOut, "\n") {
		if !isLspciNvidiaGPU(line) {
			continue
		}
		s := line
		if i := strings.Index(s, "NVIDIA Corporation "); i >= 0 {
			s = s[i+len("NVIDIA Corporation "):]
		} else {
			s = descOnly(s)
		}
		if i := strings.Index(s, " (rev"); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSpace(s)
	}
	return ""
}

// revFFBDFs returns the BDF (first field) of every lspci line reading
// "(rev ff)" — config space unreadable, device fell off the bus. Pure function.
func revFFBDFs(lspciOut string) []string {
	var out []string
	for _, line := range strings.Split(lspciOut, "\n") {
		if !strings.Contains(line, "(rev ff)") {
			continue
		}
		if f := strings.Fields(line); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

// parseGPUInventory returns the data lines (header dropped, trimmed) of
// `nvidia-smi --query-gpu=index,pci.bus_id,serial,uuid,name --format=csv`.
// Pure function.
func parseGPUInventory(csvOut string) []string {
	lines := strings.Split(strings.TrimSpace(csvOut), "\n")
	var out []string
	for i, line := range lines {
		l := strings.TrimSpace(line)
		if i == 0 || l == "" { // header
			continue
		}
		out = append(out, l)
	}
	return out
}

// analyzeGPUs builds the structured GPU findings and prints the colored
// "GPU analysis" terminal section. Returns nil when the node has no GPUs at
// all (no nvidia-smi AND nothing NVIDIA in lspci).
func (t *triage) analyzeGPUs() *gpuFindings {
	lspciOut := cmdOut("lspci")
	countL := countLspciNvidiaGPUs(lspciOut)
	if !t.haveNvsmi && countL == 0 {
		return nil
	}

	g := &gpuFindings{CountLspci: countL}
	if t.haveDmi {
		g.Manufacturer = strings.TrimSpace(cmdOut("dmidecode", "-s", "system-manufacturer"))
	}

	var smiL string
	if t.haveNvsmi {
		smiL = cmdOut("nvidia-smi", "-L")
		g.CountSMI = countSMIGPUs(smiL)
		g.Inventory = parseGPUInventory(cmdOut("nvidia-smi",
			"--query-gpu=index,pci.bus_id,serial,uuid,name", "--format=csv"))
	}
	g.GPUModel = gpuModelFromSMI(smiL)
	if g.GPUModel == "" {
		g.GPUModel = gpuModelFromLspci(lspciOut)
	}

	g.ReportedGPUs, _ = strconv.Atoi(envOr("GPUINSPECT_EXPECTED_GPUS", "0"))
	g.Expected = expectedGPUCount(g.ReportedGPUs, g.GPUModel)

	// Golden set consulted only when the count is actually short — it pins
	// WHICH BDF vanished, never second-guesses a full complement (some nodes
	// of a known SKU place their GPUs at different addresses).
	goldenMissing := missingGPUBDFs(g.Manufacturer, g.GPUModel, lspciOut)
	g.MissingBDFs = gateMissingBDFs(g.Expected, g.CountLspci, goldenMissing)
	goldenMismatch := len(goldenMissing) > 0 && len(g.MissingBDFs) == 0
	g.RevFF = revFFBDFs(lspciOut)

	t.printGPUAnalysis(g, goldenMismatch)
	return g
}

func (t *triage) printGPUAnalysis(g *gpuFindings, goldenMismatch bool) {
	c := t.c
	t.header("GPU analysis")
	t.log.p("Platform : %s | %s", orNA(g.Manufacturer), orNA(g.GPUModel))

	countLine := fmt.Sprintf("GPUs     : nvidia-smi %d | lspci %d | expected %d",
		g.CountSMI, g.CountLspci, g.Expected)
	below := g.Expected > 0 && (g.CountSMI < g.Expected || g.CountLspci < g.Expected)
	if below {
		t.log.p("%s%s%s", c.R, countLine, c.X)
	} else {
		t.log.p("%s", countLine)
	}

	if g.ReportedGPUs > 0 && g.ReportedGPUs < g.Expected {
		t.log.p("%sNOTE    : node self-reports allocatable %d GPU(s) — the missing GPU is already%s", c.Y, g.ReportedGPUs, c.X)
		t.log.p("%s          baked into the node's own report; platform default %d is used as expected%s", c.Y, g.Expected, c.X)
	}
	for _, bdf := range g.MissingBDFs {
		t.log.p("%sALERT   : GPU MISSING at %s (present in platform golden set, absent from lspci)%s",
			c.R, bdf, c.X)
	}
	if goldenMismatch {
		t.log.p("%sNOTE    : golden BDF set for this platform does not match this node's GPU addresses — count is full, ignoring%s",
			c.D, c.X)
	}
	for _, bdf := range g.RevFF {
		t.log.p("%sALERT   : %s reads (rev ff) — device fell off the bus%s", c.R, bdf, c.X)
	}
	if !below && len(g.MissingBDFs) == 0 && len(g.RevFF) == 0 && g.Expected > 0 {
		t.log.p("%sOK      : all %d GPUs present%s", c.G, g.Expected, c.X)
	}
	for _, line := range g.Inventory {
		t.log.p("%s  %s%s", c.D, line, c.X)
	}
}
