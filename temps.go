package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Live GPU thermal readout (--temps)
//
// Quick advisory mode for "what are the temps RIGHT NOW": every GPU's core and
// HBM temperature sampled live over a short window, checked against the
// driver's own thermal thresholds (nvidia-smi -q -d TEMPERATURE) and the
// thermal-throttle reason bits, plus the dmesg thermal/Xid history. No PCIe
// link checks, no bundle sweep, no AI/nodebot — designed to answer the
// thermal-runbook "capture live thermals, do NOT rely on an idle snapshot"
// step in seconds. Exit 0 = all GPUs within limits, 1 = thermal finding,
// 3 = tool error. Sampling window: GPUINSPECT_TEMP_SECONDS (default 10, 1 Hz).
// ---------------------------------------------------------------------------

// tempsQuery is the one-shot per-GPU thermal snapshot. clocks_throttle_reasons
// is the long-standing field family (newer drivers alias it to
// clocks_event_reasons; the old name still parses fleet-wide).
const tempsQuery = "index,pci.bus_id,name,temperature.gpu,temperature.memory," +
	"fan.speed,power.draw,power.limit,utilization.gpu,clocks.sm," +
	"clocks_throttle_reasons.hw_thermal_slowdown,clocks_throttle_reasons.sw_thermal_slowdown"

// tempsSampleQuery is the repeated live sample — kept minimal. On newer
// drivers temperature.gpu.tlimit is the throttle HEADROOM in C (lower =
// hotter, <=0 = throttling); older drivers print N/A, which parses to
// "unknown" and is ignored.
const tempsSampleQuery = "index,temperature.gpu,temperature.memory,temperature.gpu.tlimit"

type tempsGPU struct {
	Index, BDF, Name      string
	TempGPU, TempMem      string
	Fan, PowerW, PowerLim string
	Util, SMClock         string
	HWTherm, SWTherm      string
}

// parseTempsCSV parses the noheader,nounits output of tempsQuery. Rows with
// fewer fields than the query (driver refused a field) are dropped. Pure.
func parseTempsCSV(csv string) []tempsGPU {
	var out []tempsGPU
	for _, line := range strings.Split(strings.TrimSpace(csv), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 12 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		out = append(out, tempsGPU{
			Index: f[0], BDF: smiBDF(f[1]), Name: f[2],
			TempGPU: f[3], TempMem: f[4],
			Fan: f[5], PowerW: f[6], PowerLim: f[7],
			Util: f[8], SMClock: f[9],
			HWTherm: f[10], SWTherm: f[11],
		})
	}
	return out
}

// tempLimits are the driver-reported thermal thresholds for one GPU.
// -1 means the driver reported N/A (or the field was absent). Two threshold
// families exist in the fleet:
//   - absolute:  "GPU Slowdown Temp : 89 C" — fault when core temp RISES to it
//   - T.Limit:   "GPU Slowdown T.Limit Temp : -2 C" (H100/newer drivers) —
//     thresholds on the throttle HEADROOM; fault when the live headroom
//     (temperature.gpu.tlimit / "GPU T.Limit Temp") FALLS to it. TLOK marks
//     the T.Limit pair as parsed (their values are legitimately <= 0).
type tempLimits struct {
	Slowdown, Shutdown, MaxOp, MemMaxOp int
	SlowTL, MaxOpTL                     int
	TLOK                                bool
}

var tempsGPUHeadRe = regexp.MustCompile(`^GPU (\S+)`)

// atoiTempOK extracts the leading integer from a `-q` value like " 87 C" or
// "-2 C"; ok=false for N/A or anything non-numeric. Pure.
func atoiTempOK(s string) (int, bool) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(f[0])
	if err != nil {
		return 0, false
	}
	return n, true
}

// atoiTemp is atoiTempOK collapsed to -1 on unknown — for absolute
// temperatures only (never negative in practice). Pure.
func atoiTemp(s string) int {
	n, ok := atoiTempOK(s)
	if !ok {
		return -1
	}
	return n
}

// parseTempLimits extracts per-GPU thermal thresholds from
// `nvidia-smi -q -d TEMPERATURE` output, keyed by normalized BDF. Pure.
func parseTempLimits(out string) map[string]tempLimits {
	m := map[string]tempLimits{}
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		if h := tempsGPUHeadRe.FindStringSubmatch(line); h != nil {
			cur = smiBDF(h[1])
			m[cur] = tempLimits{Slowdown: -1, Shutdown: -1, MaxOp: -1, MemMaxOp: -1}
			continue
		}
		if cur == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		l := m[cur]
		switch {
		// T.Limit keys first — they are distinct strings, but keeping the
		// headroom family together makes the precedence explicit.
		case strings.Contains(k, "GPU Slowdown T.Limit"):
			if n, ok := atoiTempOK(v); ok {
				l.SlowTL, l.TLOK = n, true
			}
		case strings.Contains(k, "GPU Max Operating T.Limit"):
			if n, ok := atoiTempOK(v); ok {
				l.MaxOpTL, l.TLOK = n, true
			}
		case strings.Contains(k, "GPU Slowdown Temp"):
			l.Slowdown = atoiTemp(v)
		case strings.Contains(k, "GPU Shutdown Temp"):
			l.Shutdown = atoiTemp(v)
		case strings.Contains(k, "GPU Max Operating Temp"):
			l.MaxOp = atoiTemp(v)
		case strings.Contains(k, "Memory Max Operating Temp"):
			l.MemMaxOp = atoiTemp(v)
		}
		m[cur] = l
	}
	return m
}

// tempsFaults derives the verdict-gating thermal findings from the snapshot,
// the worst values seen across the sampling window (max core/HBM temp and MIN
// throttle headroom, keyed by GPU index — a minTL entry exists only when the
// driver reported one), and the driver thresholds (keyed by BDF). expected>0
// additionally gates the visible-GPU count (a GPU too broken to report temps
// must not read healthy). Pure.
func tempsFaults(gpus []tempsGPU, maxGPU, maxMem, minTL map[string]int, limits map[string]tempLimits, expected int) []string {
	var faults []string
	for _, g := range gpus {
		lim, haveLim := limits[g.BDF]
		if strings.EqualFold(g.HWTherm, "Active") {
			faults = append(faults, fmt.Sprintf("GPU %s (%s): HW thermal slowdown ACTIVE", g.Index, g.BDF))
		}
		if strings.EqualFold(g.SWTherm, "Active") {
			faults = append(faults, fmt.Sprintf("GPU %s (%s): SW thermal slowdown ACTIVE", g.Index, g.BDF))
		}
		if !haveLim {
			continue
		}
		mx := maxGPU[g.Index]
		switch {
		case lim.Slowdown > 0 && mx >= lim.Slowdown:
			faults = append(faults, fmt.Sprintf("GPU %s (%s): core %d C at/above SLOWDOWN threshold %d C", g.Index, g.BDF, mx, lim.Slowdown))
		case lim.MaxOp > 0 && mx >= lim.MaxOp:
			faults = append(faults, fmt.Sprintf("GPU %s (%s): core %d C at/above max operating %d C (DCGM thermal-violation class)", g.Index, g.BDF, mx, lim.MaxOp))
		}
		if tl, ok := minTL[g.Index]; ok && lim.TLOK {
			switch {
			case tl <= lim.SlowTL:
				faults = append(faults, fmt.Sprintf("GPU %s (%s): throttle headroom %d C at/below SLOWDOWN T.Limit %d C", g.Index, g.BDF, tl, lim.SlowTL))
			case tl <= lim.MaxOpTL:
				faults = append(faults, fmt.Sprintf("GPU %s (%s): throttle headroom %d C at/below max-operating T.Limit %d C (DCGM thermal-violation class)", g.Index, g.BDF, tl, lim.MaxOpTL))
			}
		}
		if mm := maxMem[g.Index]; lim.MemMaxOp > 0 && mm >= lim.MemMaxOp {
			faults = append(faults, fmt.Sprintf("GPU %s (%s): HBM %d C at/above memory max operating %d C", g.Index, g.BDF, mm, lim.MemMaxOp))
		}
	}
	if expected > 0 && len(gpus) < expected {
		faults = append(faults, fmt.Sprintf("only %d/%d GPUs visible to nvidia-smi — temps unverifiable for the missing GPU(s)", len(gpus), expected))
	}
	return faults
}

// runTemps is the on-node --temps entry point (called from runLocal instead of
// the full triage). Everything it prints goes through the tee'd logger; raw
// captures land in <bundle>/temps/ so the normal tar + retrieval carries them
// back for tickets.
func (t *triage) runTemps() int {
	c := t.c
	t.header("Live GPU thermal readout (--temps)")
	if !t.haveNvsmi {
		t.log.p("%sERROR: nvidia-smi not found — cannot read GPU temperatures.%s", c.R, c.X)
		t.writeTempsVerdict(3, []string{"nvidia-smi not found on node"})
		return 3
	}
	dir := filepath.Join(t.bundle, "temps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.log.p("%sERROR: mkdir %s: %v%s", c.R, dir, err, c.X)
		return 3
	}

	// 1. Snapshot: full thermal inventory of every GPU.
	csv := t.runShow(filepath.Join(dir, "snapshot.csv"), "nvidia-smi",
		"--query-gpu="+tempsQuery, "--format=csv,noheader,nounits")
	gpus := parseTempsCSV(csv)
	if len(gpus) == 0 {
		t.log.p("%sERROR: nvidia-smi returned no GPUs — driver down or GPUs off the bus.%s", c.R, c.X)
		t.writeTempsVerdict(1, []string{"nvidia-smi returned no GPUs (driver down or GPUs off the bus) — temps unreadable"})
		return 1
	}

	// 2. Driver thermal thresholds (slowdown/shutdown/max-operating per GPU).
	t.log.p("")
	qOut := t.runShow(filepath.Join(dir, "nvidia_smi_q_temperature.txt"),
		"nvidia-smi", "-q", "-d", "TEMPERATURE")
	limits := parseTempLimits(qOut)

	// 3. Live sampling window — worst temperature (and lowest throttle
	// headroom) per GPU across the window is what gates the verdict, not the
	// single idle snapshot. The window is TIME-bounded: on big HGX nodes a
	// single nvidia-smi query can take several seconds, so a fixed sample
	// count would silently multiply the runtime.
	secs := envInt("GPUINSPECT_TEMP_SECONDS", 10)
	maxGPU, maxMem, minTL := map[string]int{}, map[string]int{}, map[string]int{}
	for _, g := range gpus {
		maxGPU[g.Index] = atoiTemp(g.TempGPU)
		maxMem[g.Index] = atoiTemp(g.TempMem)
	}
	t.log.p("")
	t.log.p("%sLive sampling: %ds window at up to 1 Hz (GPUINSPECT_TEMP_SECONDS) — core C / HBM C per GPU%s", c.B, secs, c.X)
	head := "  time    "
	for _, g := range gpus {
		head += fmt.Sprintf(" %9s", "GPU"+g.Index)
	}
	t.log.p("%s%s%s", c.B, head, c.X)
	var samples strings.Builder
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for {
		start := time.Now()
		out := cmdOut("nvidia-smi", "--query-gpu="+tempsSampleQuery, "--format=csv,noheader,nounits")
		samples.WriteString(start.Format("15:04:05") + "\n" + out + "\n")
		type sample struct {
			core, mem, tl int
			tlOK          bool
		}
		byIdx := map[string]sample{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Split(line, ",")
			if len(f) < 3 {
				continue
			}
			idx := strings.TrimSpace(f[0])
			s := sample{core: atoiTemp(f[1]), mem: atoiTemp(f[2])}
			if len(f) >= 4 {
				s.tl, s.tlOK = atoiTempOK(f[3])
			}
			byIdx[idx] = s
			if s.core > maxGPU[idx] {
				maxGPU[idx] = s.core
			}
			if s.mem > maxMem[idx] {
				maxMem[idx] = s.mem
			}
			if s.tlOK {
				if cur, seen := minTL[idx]; !seen || s.tl < cur {
					minTL[idx] = s.tl
				}
			}
		}
		row := "  " + start.Format("15:04:05")
		for _, g := range gpus {
			s, ok := byIdx[g.Index]
			if !ok {
				row += fmt.Sprintf(" %9s", "-")
				continue
			}
			cell := fmt.Sprintf("%3d/%-4s", s.core, memCell(s.mem))
			if hot(s.core, s.mem, s.tl, s.tlOK, limits[g.BDF]) {
				cell = c.R + cell + c.X
			}
			row += " " + cell
		}
		t.log.p("%s", row)
		if !time.Now().Before(deadline) {
			break
		}
		if rest := time.Second - time.Since(start); rest > 0 {
			time.Sleep(rest)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "samples.txt"), []byte(samples.String()), 0o644)

	// 4. Per-GPU summary against the driver thresholds. "tlim min" is the
	// lowest throttle headroom seen in the window (T.Limit drivers only —
	// <=0 means the GPU is throttling).
	t.log.p("")
	t.log.p("%s%-3s %-12s %-9s %-9s %-9s %-14s %-9s %-13s %-10s%s", c.B,
		"idx", "bdf", "core max", "hbm max", "tlim min", "slow/maxop C", "fan", "power W", "throttle", c.X)
	for _, g := range gpus {
		lim := limits[g.BDF]
		throttle := "-"
		if strings.EqualFold(g.HWTherm, "Active") {
			throttle = "HW-THERMAL"
		} else if strings.EqualFold(g.SWTherm, "Active") {
			throttle = "SW-THERMAL"
		}
		tlMin, tlSeen := minTL[g.Index]
		tlCol := "-"
		if tlSeen {
			tlCol = fmt.Sprintf("%d C", tlMin)
		}
		thresholds := fmt.Sprintf("%s/%s", limStr(lim.Slowdown), limStr(lim.MaxOp))
		if lim.Slowdown < 0 && lim.MaxOp < 0 && lim.TLOK {
			thresholds = fmt.Sprintf("TL %d/%d", lim.SlowTL, lim.MaxOpTL)
		}
		hl, hlx := "", ""
		if hot(maxGPU[g.Index], maxMem[g.Index], tlMin, tlSeen, lim) || throttle != "-" {
			hl, hlx = c.R, c.X
		}
		t.log.p("%s%-3s %-12s %-9s %-9s %-9s %-14s %-9s %-13s %-10s%s", hl,
			g.Index, g.BDF,
			fmt.Sprintf("%d C", maxGPU[g.Index]), memCol(maxMem[g.Index]), tlCol,
			thresholds, g.Fan, fmt.Sprintf("%s/%s", g.PowerW, g.PowerLim), throttle, hlx)
	}

	// 5. Thermal/Xid history (informational — a past event is triage context,
	// not a live fault).
	t.log.p("")
	t.bashShow(filepath.Join(dir, "dmesg_thermal.txt"),
		"dmesg -T | grep -iE 'xid|thermal|slowdown|throttl' | tail -40")

	faults := tempsFaults(gpus, maxGPU, maxMem, minTL, limits, envInt("GPUINSPECT_EXPECTED_GPUS", 0))
	code := 0
	t.log.p("")
	if len(faults) == 0 {
		t.log.p("%sVERDICT: HEALTHY — all %d GPUs within thermal limits over the %ds window%s", c.G, len(gpus), secs, c.X)
	} else {
		code = 1
		t.log.p("%sVERDICT: THERMAL FINDING(S)%s", c.R, c.X)
		for _, f := range faults {
			t.log.p("  %s- %s%s", c.R, f, c.X)
		}
	}
	t.writeTempsVerdict(code, faults)
	return code
}

// hot reports whether a sample breaches the GPU's driver thresholds: core at
// slowdown/max-operating, HBM at memory max, or throttle headroom (tl, when
// the driver reports one — tlOK) down at its T.Limit thresholds. Pure.
func hot(core, mem, tl int, tlOK bool, lim tempLimits) bool {
	if lim.Slowdown > 0 && core >= lim.Slowdown {
		return true
	}
	if lim.MaxOp > 0 && core >= lim.MaxOp {
		return true
	}
	if lim.MemMaxOp > 0 && mem >= lim.MemMaxOp {
		return true
	}
	if tlOK && lim.TLOK && tl <= lim.MaxOpTL {
		return true
	}
	return false
}

func memCell(n int) string {
	if n < 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func memCol(n int) string {
	if n < 0 {
		return "-"
	}
	return fmt.Sprintf("%d C", n)
}

func limStr(n int) string {
	if n < 0 {
		return "?"
	}
	return strconv.Itoa(n)
}

// writeTempsVerdict writes the temps-mode verdict.json (the machine-readable
// mirror the laptop matrix reads). Advisory only — never executed.
func (t *triage) writeTempsVerdict(code int, faults []string) {
	v := verdictJSON{
		BMN: t.o.bmn, VerdictCode: code, Issues: faults,
		Flags: map[string]bool{"temps_only": true, "thermal_fault": code == 1},
	}
	if code == 1 {
		v.NextSteps = append(v.NextSteps,
			"ADVISE: confirm the thermal finding under load (cw-thermal workflow: move to debug + AMM per the GPU thermal runbook) — a single-GPU outlier points at heat-sink seating/TIM, RMA-eligible on confirmed tcriteria violations")
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
