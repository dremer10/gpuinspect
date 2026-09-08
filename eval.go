package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Full GPU evaluation (--eval) — on-node port of gpu_eval.sh
//
// Safe by default: extended nvidia-smi inventory, retired pages, per-GPU
// PCIe endpoint detail, and short dmon/pmon sampling windows. DCGM diag and
// nvbandwidth stress the GPUs and therefore require the operator's explicit
// --yes-intrusive; nvidia-bug-report.sh is safe but slow. Everything lands
// in <bundle>/eval/ so the existing tar + retrieval path carries it back.
//
// Findings that indicate a real fault (GPU below max PCIe width, DCGM FAIL,
// nvbandwidth failure) land in triage.evalFindings and gate the verdict:
// they block the PEX890xx false-positive class and force at least DEGRADED.
// Heuristic observations (hot-idle clocks, missing tools) are evalNotes —
// logged and bundled, never verdict-changing.
// ---------------------------------------------------------------------------

// evalQuery is the extended per-GPU nvidia-smi inventory (matches gpu_eval.sh).
const evalQuery = "index,uuid,name,serial,pci.bus_id,pstate," +
	"pcie.link.gen.current,pcie.link.gen.max,pcie.link.width.current,pcie.link.width.max," +
	"temperature.gpu,utilization.gpu,utilization.memory,memory.total,memory.used," +
	"power.draw,power.limit,clocks.sm,clocks.mem," +
	"ecc.mode.current,ecc.errors.corrected.aggregate.total,ecc.errors.uncorrected.aggregate.total"

type evalGPU struct {
	Index    string `json:"index"`
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	Serial   string `json:"serial"`
	BDF      string `json:"bdf"`
	PState   string `json:"pstate"`
	GenCur   string `json:"pcie_gen_current"`
	GenMax   string `json:"pcie_gen_max"`
	WidthCur string `json:"pcie_width_current"`
	WidthMax string `json:"pcie_width_max"`
	TempC    string `json:"temp_c"`
	UtilGPU  string `json:"gpu_util"`
	UtilMem  string `json:"mem_util"`
	MemTotal string `json:"mem_total_mb"`
	MemUsed  string `json:"mem_used_mb"`
	PowerW   string `json:"power_draw_w"`
	PowerLim string `json:"power_limit_w"`
	SMClock  string `json:"sm_clock_mhz"`
	MemClock string `json:"mem_clock_mhz"`
	ECCMode  string `json:"ecc_mode"`
	ECCCorr  string `json:"ecc_corr_agg"`
	ECCUnc   string `json:"ecc_unc_agg"`
}

// smiBDF normalizes nvidia-smi's 8-hex-digit-domain bus id
// ("00000000:19:00.0") to the 4-digit sysfs/lspci form, lowercased.
func smiBDF(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) == 16 && s[8] == ':' {
		s = s[4:]
	}
	return s
}

// parseEvalCSV parses the noheader,nounits output of the evalQuery. Rows with
// fewer fields than the query (driver refused a field) are dropped. Pure.
func parseEvalCSV(csv string) []evalGPU {
	var out []evalGPU
	for _, line := range strings.Split(strings.TrimSpace(csv), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 22 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		out = append(out, evalGPU{
			Index: f[0], UUID: f[1], Name: f[2], Serial: f[3], BDF: smiBDF(f[4]), PState: f[5],
			GenCur: f[6], GenMax: f[7], WidthCur: f[8], WidthMax: f[9],
			TempC: f[10], UtilGPU: f[11], UtilMem: f[12], MemTotal: f[13], MemUsed: f[14],
			PowerW: f[15], PowerLim: f[16], SMClock: f[17], MemClock: f[18],
			ECCMode: f[19], ECCCorr: f[20], ECCUnc: f[21],
		})
	}
	return out
}

// evalWidthMismatches flags GPUs whose current PCIe width is below their max —
// a real link fault from the endpoint side (current gen below max is normal at
// idle via ASPM and deliberately NOT flagged). Pure.
func evalWidthMismatches(gpus []evalGPU) []string {
	var out []string
	for _, g := range gpus {
		cw, err1 := strconv.Atoi(g.WidthCur)
		mw, err2 := strconv.Atoi(g.WidthMax)
		if err1 != nil || err2 != nil || mw == 0 {
			continue
		}
		if cw < mw {
			out = append(out, fmt.Sprintf("GPU %s (%s): PCIe width x%d below max x%d", g.Index, g.BDF, cw, mw))
		}
	}
	return out
}

// evalHotIdle flags GPUs holding high SM clocks at zero utilization — a
// stuck-process/persistence-mode heuristic, informational only. Pure.
func evalHotIdle(gpus []evalGPU) []string {
	var out []string
	for _, g := range gpus {
		sm, err1 := strconv.ParseFloat(g.SMClock, 64)
		util, err2 := strconv.ParseFloat(g.UtilGPU, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		if sm > 500 && util == 0 {
			out = append(out, fmt.Sprintf("GPU %s (%s): SM clock %.0f MHz at 0%% util (temp %s C) — hot-idle candidate", g.Index, g.BDF, sm, g.TempC))
		}
	}
	return out
}

// printEvalTable renders the extended per-GPU inventory as a compact table in
// the report — the terminal view of what gpu_eval.sh used to bury in
// nvidia_smi_query.csv. PCIe width below max is highlighted (the verdict-
// gating eval finding); gen-current below max is normal at idle (ASPM).
func (t *triage) printEvalTable(gpus []evalGPU) {
	if len(gpus) == 0 {
		return
	}
	c := t.c
	t.log.p("%s%-3s %-14s %-3s %-7s %-9s %-5s %-5s %-9s %-8s %-8s %-8s%s", c.B,
		"idx", "bdf", "pst", "gen", "width", "temp", "util", "power W", "sm MHz", "eccUnc", "serial", c.X)
	for _, g := range gpus {
		width := fmt.Sprintf("x%s/x%s", g.WidthCur, g.WidthMax)
		hl, hlx := "", ""
		if cw, err1 := strconv.Atoi(g.WidthCur); err1 == nil {
			if mw, err2 := strconv.Atoi(g.WidthMax); err2 == nil && cw < mw {
				hl, hlx = c.R, c.X
			}
		}
		t.log.p("%s%-3s %-14s %-3s %-7s %-9s %-5s %-5s %-9s %-8s %-8s %-8s%s", hl,
			g.Index, g.BDF, g.PState,
			fmt.Sprintf("%s/%s", g.GenCur, g.GenMax), width,
			g.TempC, g.UtilGPU,
			fmt.Sprintf("%s/%s", g.PowerW, g.PowerLim),
			g.SMClock, g.ECCUnc, g.Serial, hlx)
	}
}

var dcgmFailRe = regexp.MustCompile(`\bFail\b`)

// dcgmDiagFailed reports whether a dcgmi diag run printed any Fail result. Pure.
func dcgmDiagFailed(out string) bool { return dcgmFailRe.MatchString(out) }

// runConcurrently runs the given functions concurrently and returns once ALL
// have finished. Used to overlap the dmon/pmon sampling windows so eval wall
// time is the longest window, not the sum — the functions must not touch the
// logger (it is not goroutine-safe); printing happens after this returns.
func runConcurrently(fns ...func()) {
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func(f func()) {
			defer wg.Done()
			f()
		}(fn)
	}
	wg.Wait()
}

// runEvalCmd runs a command with a timeout, capturing combined output to path.
// timedOut=true means the deadline hit — expected for the dmon/pmon sampling
// windows, a finding for DCGM/nvbandwidth.
func runEvalCmd(path string, sec int, name string, args ...string) (timedOut bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(sec)*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	_ = os.WriteFile(path, out, 0o644)
	if ctx.Err() == context.DeadlineExceeded {
		return true, nil
	}
	return false, err
}

func (t *triage) runEval() {
	o, c := t.o, t.c
	t.header("Full GPU evaluation (--eval)")
	t.evalRan = true
	dir := filepath.Join(t.bundle, "eval")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.log.p("%sWARN: cannot create eval dir %s: %v — skipping evaluation%s", c.Y, dir, err, c.X)
		return
	}
	t.log.p("%s-> %s%s", c.D, dir, c.X)

	if !t.haveNvsmi {
		t.log.p("%sWARN: nvidia-smi not found — skipping GPU evaluation%s", c.Y, c.X)
		return
	}

	t.echoCmd("nvidia-smi --query-gpu=" + evalQuery + " --format=csv,noheader,nounits")
	csv := cmdOut("nvidia-smi", "--query-gpu="+evalQuery, "--format=csv,noheader,nounits")
	_ = os.WriteFile(filepath.Join(dir, "nvidia_smi_query.csv"), []byte(csv), 0o644)
	gpus := parseEvalCSV(csv)
	if b, err := json.MarshalIndent(gpus, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "gpu_inventory.json"), b, 0o644)
	}
	t.log.p("Inventory : %d GPU(s) in extended query", len(gpus))
	t.printEvalTable(gpus)
	if len(gpus) == 0 {
		// The missing-GPU fault itself is the main GPU suite's verdict; this
		// note just stops the eval section from reading as a clean pass.
		t.evalNotes = append(t.evalNotes,
			"extended nvidia-smi query returned 0 GPUs — NVML/driver fault or no GPUs visible (eval/nvidia_smi_query.csv)")
	}

	t.bashShow(filepath.Join(dir, "retired_pages.txt"),
		"nvidia-smi --query-retired-pages=gpu_uuid,retired_page_address,retirement_cause --format=csv || true")

	// Per-GPU PCIe endpoint detail (config space + sysfs link/AER state).
	// GPU link state is shown inline; the raw config-space dump stays in the
	// bundle only.
	for _, g := range gpus {
		if g.BDF == "" || !dirExists("/sys/bus/pci/devices/"+g.BDF) {
			continue
		}
		tag := "gpu" + g.Index + "_" + sanitize(g.BDF)
		t.runQuiet(filepath.Join(dir, "lspci_"+tag+".txt"), "lspci", "-vvv", "-xxxx", "-s", g.BDF)
		var sb strings.Builder
		var brief []string
		for _, f := range []string{
			"current_link_speed", "current_link_width", "max_link_speed", "max_link_width",
			"aer_dev_correctable", "aer_dev_fatal", "aer_dev_nonfatal",
		} {
			p := "/sys/bus/pci/devices/" + g.BDF + "/" + f
			v := readSysfs(p)
			sb.WriteString("=== " + p + " ===\n" + v + "\n\n")
			if f == "current_link_speed" || f == "current_link_width" || f == "max_link_speed" || f == "max_link_width" {
				val := "<unavailable>"
				if fs := strings.Fields(v); len(fs) > 0 {
					val = fs[0]
				}
				brief = append(brief, f+"="+val)
			}
		}
		_ = os.WriteFile(filepath.Join(dir, "sysfs_"+tag+".txt"), []byte(sb.String()), 0o644)
		t.log.p("%s  GPU %s %s sysfs: %s (AER counters → eval/sysfs_%s.txt)%s", t.c.D, g.Index, g.BDF, strings.Join(brief, " "), tag, t.c.X)
	}

	dmonSec := envInt("GPUINSPECT_DMON_SECONDS", 20)
	pmonSec := envInt("GPUINSPECT_PMON_SECONDS", 10)
	t.log.p("Sampling  : dmon %ds, pmon %ds (concurrent — wall time is the longer window)", dmonSec, pmonSec)
	// EXECUTE CONCURRENTLY, PRINT SEQUENTIALLY: both sampling windows run at
	// once (each writes only its own file, neither touches the logger), then
	// the echoCmd/tail blocks print in the fixed dmon-then-pmon order.
	dmonPath := filepath.Join(dir, "nvidia_smi_dmon.txt")
	pmonPath := filepath.Join(dir, "nvidia_smi_pmon.txt")
	runConcurrently(
		func() { _, _ = runEvalCmd(dmonPath, dmonSec, "nvidia-smi", "dmon", "-s", "pucvmet", "-d", "1") },
		func() { _, _ = runEvalCmd(pmonPath, pmonSec, "nvidia-smi", "pmon", "-s", "umct") },
	)
	t.echoCmd(fmt.Sprintf("nvidia-smi dmon -s pucvmet -d 1   (%ds window)", dmonSec))
	t.showFileTail(dmonPath, 12, "eval/nvidia_smi_dmon.txt")
	t.echoCmd(fmt.Sprintf("nvidia-smi pmon -s umct   (%ds window)", pmonSec))
	t.showFileTail(pmonPath, 12, "eval/nvidia_smi_pmon.txt")

	t.evalFindings = append(t.evalFindings, evalWidthMismatches(gpus)...)
	t.evalNotes = append(t.evalNotes, evalHotIdle(gpus)...)

	if o.evalDCGM {
		if haveCmd("dcgmi") {
			t.runShow(filepath.Join(dir, "dcgmi_discovery.txt"), "dcgmi", "discovery", "-l")
			t.bashShow(filepath.Join(dir, "dcgmi_health.txt"), "dcgmi health -c || true; echo; dcgmi health -g 1 -c || true")
			dcgmSec := envInt("GPUINSPECT_DCGM_TIMEOUT", 600)
			t.log.p("DCGM      : dcgmi diag -r %d (timeout %ds) ...", o.dcgmLevel, dcgmSec)
			t.echoCmd(fmt.Sprintf("dcgmi diag -r %d", o.dcgmLevel))
			diagPath := filepath.Join(dir, "dcgmi_diag.txt")
			timedOut, runErr := runEvalCmd(diagPath, dcgmSec, "dcgmi", "diag", "-r", strconv.Itoa(o.dcgmLevel))
			diagOut, _ := os.ReadFile(diagPath)
			t.echoOutput(string(diagOut), "eval/dcgmi_diag.txt")
			switch {
			case timedOut:
				t.evalFindings = append(t.evalFindings, fmt.Sprintf("DCGM diag level %d timed out after %ds (eval/dcgmi_diag.txt)", o.dcgmLevel, dcgmSec))
			case dcgmDiagFailed(string(diagOut)):
				t.evalFindings = append(t.evalFindings, fmt.Sprintf("DCGM diag level %d reported FAIL (eval/dcgmi_diag.txt)", o.dcgmLevel))
			case runErr != nil:
				t.evalNotes = append(t.evalNotes, fmt.Sprintf("dcgmi diag exited with error: %v (eval/dcgmi_diag.txt)", runErr))
			default:
				t.log.p("%sOK      : DCGM diag level %d passed%s", c.G, o.dcgmLevel, c.X)
			}
		} else {
			t.evalNotes = append(t.evalNotes, "dcgmi not found on node — DCGM diag skipped")
		}
	}

	if o.evalNVBW {
		if haveCmd("nvbandwidth") {
			sec := envInt("GPUINSPECT_NVBW_TIMEOUT", 300)
			t.log.p("Bandwidth : nvbandwidth (timeout %ds) ...", sec)
			t.echoCmd("nvbandwidth")
			timedOut, runErr := runEvalCmd(filepath.Join(dir, "nvbandwidth.txt"), sec, "nvbandwidth")
			t.showFileTail(filepath.Join(dir, "nvbandwidth.txt"), 20, "eval/nvbandwidth.txt")
			switch {
			case timedOut:
				t.evalFindings = append(t.evalFindings, fmt.Sprintf("nvbandwidth timed out after %ds (eval/nvbandwidth.txt)", sec))
			case runErr != nil:
				t.evalFindings = append(t.evalFindings, fmt.Sprintf("nvbandwidth exited with error: %v (eval/nvbandwidth.txt)", runErr))
			}
		} else {
			t.evalNotes = append(t.evalNotes, "nvbandwidth not found on node — skipped")
		}
	}

	if o.evalBugReport {
		if haveCmd("nvidia-bug-report.sh") {
			sec := envInt("GPUINSPECT_BUGREPORT_TIMEOUT", 3600)
			t.log.p("BugReport : nvidia-bug-report.sh (timeout %ds — can take a while) ...", sec)
			timedOut, runErr := runEvalCmd(filepath.Join(dir, "nvidia-bug-report.stdout"), sec,
				"nvidia-bug-report.sh", "--output-file", filepath.Join(dir, "nvidia-bug-report.log"))
			if timedOut || runErr != nil {
				t.evalNotes = append(t.evalNotes, fmt.Sprintf("nvidia-bug-report.sh did not complete (timed_out=%v err=%v)", timedOut, runErr))
			}
		} else {
			t.evalNotes = append(t.evalNotes, "nvidia-bug-report.sh not found on node — skipped")
		}
	}

	summary := map[string]interface{}{
		"gpu_count": len(gpus),
		"findings":  append([]string{}, t.evalFindings...),
		"notes":     append([]string{}, t.evalNotes...),
	}
	if b, err := json.MarshalIndent(summary, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "eval_summary.json"), b, 0o644)
	}

	for _, f := range t.evalFindings {
		t.log.p("%sALERT   : %s%s", c.R, f, c.X)
	}
	for _, n := range t.evalNotes {
		t.log.p("%sNOTE    : %s%s", c.Y, n, c.X)
	}
	if len(t.evalFindings) == 0 {
		t.log.p("%sOK      : evaluation found no additional GPU issues%s", c.G, c.X)
	}
}
