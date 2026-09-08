package main

// Orchestration for BMN-first mode (gpuinspect <BMN> [<BMN>...]): fetch each
// BMN from the mgmt cluster, inspect the node over ssh (parallel worker
// pool), collect the on-node verdict.json, optionally run the AI analysis
// and nodebot, and render a fleet matrix plus summary files.
//
// ADVISORY ONLY — orchestration never executes cwctl or any state-changing
// command; NextSteps are text for the operator to copy-paste.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// matrixCellMax caps the ISSUES / NEXT STEPS columns in the terminal matrix.
// The summary files always carry the full, untruncated contents.
const matrixCellMax = 60

// verdictFile is the subset of <localDir>/verdict.json (written on-node by
// verdictjson.go) that the orchestrator consumes. The raw bytes are kept
// verbatim for the AI layer.
type verdictFile struct {
	BMN         string          `json:"bmn"`
	VerdictCode *int            `json:"verdict_code"`
	Issues      []string        `json:"issues"`
	NextSteps   []string        `json:"next_steps"`
	Flags       map[string]bool `json:"flags"`
	Devices     []verdictDevice `json:"devices"`
}

// degradedBDFSet maps each on-node device to whether its link actually
// degraded — the golden-state check uses it to tell "stale golden values"
// (link at full capability, table wrong) from genuine degradation.
func degradedBDFSet(vf *verdictFile) map[string]bool {
	if vf == nil {
		return nil
	}
	m := make(map[string]bool, len(vf.Devices))
	for _, d := range vf.Devices {
		m[d.BDF] = d.SpeedDeg || d.WidthDeg || d.Down
	}
	return m
}

func verdictWord(code int) string {
	switch code {
	case 0:
		return "HEALTHY"
	case 1:
		return "DEGRADED"
	case 2:
		return "RMA"
	default:
		return "ERROR"
	}
}

// verdictLabel is the matrix VERDICT cell: the PEX890xx benign class gets its
// own word so a fleet run answers "which nodes go back to ready" at a glance,
// and golden-state errors get theirs so nobody requests a drain/RMA for a
// table problem.
func verdictLabel(r inspectResult) string {
	if r.GoldenMismatch {
		return "GOLDEN-MIS"
	}
	if r.FalsePositive {
		return "FALSE-POS"
	}
	return verdictWord(r.VerdictCode)
}

func verdictColor(c palette, code int) string {
	switch code {
	case 0:
		return c.G
	case 1:
		return c.Y
	default:
		return c.R
	}
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func orchestrateBMNs(o *options) int {
	c := palette{}
	if !o.noColor && isTTY(os.Stdout) {
		c = colors()
	}

	total := len(o.bmns)
	par := o.parallel
	if par > total {
		par = total
	}
	if par < 1 {
		par = 1
	}

	fmt.Printf("%sgpuinspect — %d BMN(s), parallel %d, out dir %s%s\n", c.B, total, par, o.outDir, c.X)
	if o.outDir != "" {
		if err := os.MkdirAll(o.outDir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR: cannot create out dir:", err)
			return 3
		}
	}

	results := make([]inspectResult, total)
	var wg sync.WaitGroup
	sem := make(chan struct{}, par)

	// The live progress display animates one line per BMN while workers run;
	// finishAndPrint atomically replays each node's buffered section above it.
	prog := newProgress(o.bmns, c, !o.noColor && isTTY(os.Stdout))

	for idx, bmn := range o.bmns {
		wg.Add(1)
		go func(idx int, bmn string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := inspectOneBMN(o, c, bmn, func(phase string) { prog.set(idx, phase) })
			results[idx] = res
			prog.finishAndPrint(idx, res)
		}(idx, bmn)
	}
	wg.Wait()
	prog.close()

	printMatrix(results, c)
	printFleetAdvisory(results, c)
	fmt.Println()
	printMatrixMarkdown(results)
	txtPath, mdPath := writeSummaries(o.outDir, results)

	fmt.Println()
	if txtPath != "" {
		fmt.Printf("%sSummary (txt)%s : %s\n", c.B, c.X, txtPath)
	}
	if mdPath != "" {
		fmt.Printf("%sSummary (md)%s  : %s\n", c.B, c.X, mdPath)
	}
	for _, r := range results {
		if r.OutDir != "" {
			fmt.Printf("%s  %s → %s%s\n", c.D, r.BMN, r.OutDir, c.X)
		}
	}

	if o.copyMatrix {
		if err := copyToClipboard(summaryMd(results)); err != nil {
			fmt.Fprintln(os.Stderr, "WARN: clipboard copy failed —", err)
		} else {
			fmt.Println("matrix copied to clipboard")
		}
	}

	maxCode := 0
	for _, r := range results {
		if r.VerdictCode > maxCode {
			maxCode = r.VerdictCode
		}
	}
	return maxCode
}

// ---------------------------------------------------------------------------
// Per-node worker
// ---------------------------------------------------------------------------

// inspectOneBMN runs the full pipeline for a single BMN, writing everything
// into a private buffer so the section can be flushed atomically. phase
// feeds the live progress display.
func inspectOneBMN(o *options, c palette, bmn string, phase func(string)) inspectResult {
	buf := &bytes.Buffer{}
	fmt.Fprintf(buf, "\n%s━━━ %s ━━━%s\n\n", c.B, bmn, c.X)

	phase("🔎 mgmt-cluster BMN lookup")
	info, err := fetchBMN(bmn, o.kubeContext)
	if err != nil {
		fmt.Fprintf(buf, "%sERROR:%s BMN lookup failed: %v\n", c.R, c.X, err)
		return inspectResult{
			BMN: bmn, VerdictCode: 3, Err: err,
			Issues: []string{"BMN lookup failed: " + err.Error()},
			Output: buf.String(),
		}
	}

	writeIdentitySection(buf, c, info)
	writeBundleInline(buf, c, info)

	if info.GMAC == "" {
		issue := "no reportedNodeInfo.nodeName (node not reporting) — cannot ssh"
		fmt.Fprintf(buf, "%sERROR:%s %s\n", c.R, c.X, issue)
		return inspectResult{
			BMN: bmn, Info: info, VerdictCode: 3,
			Issues: append([]string{issue}, info.BundleFlags...),
			Output: buf.String(),
		}
	}

	opts := *o
	opts.bmn = bmn
	opts.remote = info.GMAC
	opts.bdfs = append(append([]string{}, o.bdfs...), info.PCIAlertBDFs...)
	opts.expectedGPUs = info.ExpectedGPUs

	// The buffered report only prints when the node finishes — a fielddiag
	// hang would otherwise look like silence, so the live phase line carries
	// the warning too.
	onNode := "🔬 on-node inspection via " + info.GMAC
	if info.fieldDiagActive() {
		onNode += " — ⚠️ fielddiag holds the GPUs, nvidia-smi will block (expect a hang; Ctrl-C and re-run after fielddiag)"
	}
	phase(onNode)
	code, localDir := runRemoteTo(buf, &opts)

	// Resolve the piped external context ONCE, now that the on-node
	// inspection is done — this is the first point where blocking on the
	// upstream producer (frop dissect, minutes-long) is acceptable.
	if o.extCtx != nil && !o.extCtx.ready() {
		phase("📥 waiting for piped context (frop dissect still running)")
	}
	ext := o.extCtx.wait()
	if ext != "" {
		fmt.Fprintf(buf, "%sexternal context: %d bytes piped in (frop/NodeBot advisory)%s\n", c.D, len(ext), c.X)
	}

	verdictBytes, vf := readVerdictJSON(localDir)
	vcode := code
	var issues, nextSteps []string
	if vf != nil {
		if vf.VerdictCode != nil {
			vcode = *vf.VerdictCode
		}
		issues = append(issues, vf.Issues...)
		nextSteps = append(nextSteps, vf.NextSteps...)
	}
	issues = append(issues, info.BundleFlags...)
	if warn := info.fieldDiagWarning(); warn != "" {
		issues = append(issues, warn)
	}
	if len(nextSteps) == 0 && vcode > 0 {
		nextSteps = []string{"follow the printed runbook steps"}
	}

	// Golden-state check (goldenstate.go): reproduce the alert's
	// expected_pci_link_* vs node_pci_cur_* comparison per BDF, device
	// identity included — enumeration/layout mismatches and stale golden
	// values fire the exact same alerts as real link faults but are fixed on
	// the golden side, never by a drain or RMA.
	goldenText := ""
	goldenMismatch := false
	if !o.noGolden {
		phase("📐 golden-state check (GloQL expected vs live)")
		gc := goldenStateCheck(info, degradedBDFSet(vf), vf != nil)
		writeGoldenSection(buf, c, gc)
		if gc.Skipped != "" {
			goldenText = "skipped: " + gc.Skipped
		} else {
			issues = append(issues, gc.Issues...)
			nextSteps = append(nextSteps, gc.NextSteps...)
			goldenMismatch = gc.Mismatch
			goldenText = strings.Join(gc.Lines, "\n")
		}
	}

	// The on-node classifier cannot see the mgmt alert feed or history: three
	// laptop-side gates can each withdraw the false-positive claim so a node
	// is never advised back to ready against off-node evidence.
	//   1. Runbook "only fallout reason": any non-PCI-link alert firing.
	//   2. External context (frop dissect piped in): open RMA / failed power
	//      drain / persistence signals.
	//   3. Self-cleared age gate: a "transient" whose alert has been firing
	//      continuously for days is flapping-link suspicion, not a transient
	//      (runbook recurrence caveat). Idle-port-only FPs persist by design
	//      and are exempt.
	fp := vf != nil && vf.Flags["pex_false_positive"]
	fpWithdrawn := false
	if fp {
		var other []string
		for _, a := range info.Alerts {
			if !bmnPCIAlertRe.MatchString(a.Name) {
				other = append(other, a.Name)
			}
		}
		if len(other) > 0 {
			fp, fpWithdrawn = false, true
			issues = append(issues, "non-PCI-link alert(s) also firing: "+strings.Join(other, ", ")+" — excluded from the PEX890xx false-positive class")
		}
	}
	extSignals := externalContextSignals(ext)
	if len(extSignals) > 0 {
		issues = append(issues, extSignals...)
		if fp {
			fp, fpWithdrawn = false, true
			issues = append(issues, "false-positive claim WITHDRAWN — external context (frop/NodeBot) shows genuine-fault history")
			nextSteps = append(nextSteps, "ADVISE: follow the RMA/hardware path per the external diagnosis history — do NOT return to ready")
		}
	}
	if fp && vf.Flags["self_cleared"] {
		if age, oldest := oldestPCIAlertAge(info.Alerts); age > fpMaxAlertAge() {
			fp, fpWithdrawn = false, true
			issues = append(issues, fmt.Sprintf("self-cleared claim WITHDRAWN — PCI link alert firing continuously since %s (%.0fh > %.0fh gate): flapping-link suspicion, treat as genuine (runbook recurrence caveat)",
				oldest, age.Hours(), fpMaxAlertAge().Hours()))
			nextSteps = append(nextSteps, "ADVISE: do not bulk return-to-ready; check alert/ticket history (frop dissect) and follow the degraded-link runbook path")
		}
	}
	// A golden-state error explains the alert better than a transient: the
	// alert keeps firing (failure_cause) until the golden side is fixed, so
	// "OK TO RETURN" would just bounce the node back to triage.
	if fp && goldenMismatch {
		fp, fpWithdrawn = false, true
		issues = append(issues, "false-positive claim WITHDRAWN — golden-state check found a golden/enumeration error: the alert will keep firing and return-to-ready will NOT stick until golden state or enumeration is fixed")
	}
	// A withdrawn claim means the on-node exit 0 is not trustworthy — the
	// matrix must not read HEALTHY.
	if fpWithdrawn && vcode == 0 {
		vcode = 1
	}
	// A golden-state error is never HEALTHY either: the hardware may be fine,
	// but the node cannot hold ready until the golden side is corrected.
	if goldenMismatch && vcode == 0 {
		vcode = 1
	}

	res := inspectResult{
		BMN: bmn, Info: info, VerdictCode: vcode, FalsePositive: fp,
		GoldenMismatch: goldenMismatch,
		Issues:         issues, NextSteps: nextSteps, OutDir: localDir,
	}

	findingsJSON := "{}"
	if len(verdictBytes) > 0 {
		findingsJSON = string(verdictBytes)
	}
	// The golden-state check runs on the laptop, after verdict.json was
	// written on-node — append it so the AI and nodebot layers see it.
	if goldenText != "" {
		findingsJSON += "\n\nGolden-state check (laptop-side, GloQL expected_pci_link_* vs live node_pci_cur_*):\n" + goldenText
	}

	if !o.noAI {
		phase("🤖 AI analysis of findings")
		analysis, aiErr := aiAnalyze(findingsJSON, info, ext)
		if aiErr != nil {
			fmt.Fprintf(buf, "%sWARN: AI analysis skipped: %v%s\n", c.Y, aiErr, c.X)
		} else {
			if localDir != "" {
				_ = os.WriteFile(filepath.Join(localDir, "analysis.md"), []byte(analysis), 0o644)
			}
			fmt.Fprintf(buf, "\n%s── Claude %s Advisory Analysis ──%s\n", c.C, aiModel(), c.X)
			fmt.Fprintln(buf, analysis)
			res.AISummary = aiFirstLine(analysis)
		}
	}

	// frop dissect IS a nodebot diagnose — when its output was piped in,
	// running gpuinspect's own nodebot afterwards duplicates minutes of work.
	// --nodebot forces it anyway.
	if o.nodebot && (ext == "" || o.nodebotForce) {
		phase("🛰️  nodebot diagnose (12h Grafana history)")
		if nbErr := runNodebot(info.GMAC, bmn, nodebotContextAppend(findingsJSON, ext), buf); nbErr != nil {
			fmt.Fprintf(buf, "%sWARN: nodebot: %v%s\n", c.Y, nbErr, c.X)
		}
	} else if o.nodebot && ext != "" {
		fmt.Fprintf(buf, "%snodebot skipped — frop dissect context already piped in (--nodebot to force)%s\n", c.D, c.X)
	}

	// Archive last so late artifacts (analysis.md) land inside the tarball.
	archiveEvidence(buf, localDir)

	res.Output = buf.String()
	return res
}

// fpMaxAlertAge is the self-cleared false-positive age gate: a transient
// sampling artifact does not fire continuously for days. Override with
// GPUINSPECT_FP_MAX_ALERT_AGE_HOURS.
func fpMaxAlertAge() time.Duration {
	return time.Duration(envInt("GPUINSPECT_FP_MAX_ALERT_AGE_HOURS", 72)) * time.Hour
}

// oldestPCIAlertAge returns how long the longest-firing PCI link alert has
// been active, plus its lastTransitionTime. Zero when no PCI alert parses.
func oldestPCIAlertAge(alerts []bmnAlert) (time.Duration, string) {
	var age time.Duration
	oldest := ""
	for _, a := range alerts {
		if !bmnPCIAlertRe.MatchString(a.Name) {
			continue
		}
		ts, err := time.Parse(time.RFC3339, a.LastTransition)
		if err != nil {
			continue
		}
		if d := time.Since(ts); d > age {
			age, oldest = d, a.LastTransition
		}
	}
	return age, oldest
}

// writeIdentitySection renders the BMN identity block (from the mgmt-cluster
// BareMetalNode object) into the node's buffered section.
func writeIdentitySection(w io.Writer, c palette, info *bmnInfo) {
	label := func(name, val string) {
		fmt.Fprintf(w, "%s%-11s%s: %s\n", c.C, name, c.X, scanOrDash(val))
	}
	label("gMAC", info.GMAC)
	label("Serial", info.Serial)
	label("DeviceSlot", info.DeviceSlot)
	label("SKU", info.SKU)
	label("State", info.State)
	if info.Workflow != "" || info.WorkflowStep != "" {
		label("Workflow", scanOrDash(info.Workflow)+" / "+scanOrDash(info.WorkflowStep))
	}
	label("Online", info.Online)
	label("Zone", info.Zone)
	if warn := info.fieldDiagWarning(); warn != "" {
		fmt.Fprintf(w, "%sWARN: %s%s\n", c.R, warn, c.X)
	}
	for _, a := range info.Alerts {
		bdf := a.BDF
		if bdf == "" {
			bdf = "-"
		}
		fmt.Fprintf(w, "%s%-11s%s: %s  BDF %s  (since %s)\n", c.Y, "Alert", c.X, a.Name, bdf, scanOrDash(a.LastTransition))
	}
}

// writeGoldenSection renders the golden-state check into the node's buffered
// section: the comparison lines when it ran, a dim one-liner when skipped.
func writeGoldenSection(w io.Writer, c palette, gc goldenCheck) {
	fmt.Fprintf(w, "\n%s── Golden-state check (GloQL expected_pci_link_* vs live) ──%s\n", c.C, c.X)
	if gc.Skipped != "" {
		fmt.Fprintf(w, "%sskipped: %s%s\n", c.D, gc.Skipped, c.X)
		return
	}
	for _, l := range gc.Lines {
		fmt.Fprintf(w, "  %s\n", l)
	}
	if gc.Mismatch {
		fmt.Fprintf(w, "%sGOLDEN-STATE ERROR — %s%s\n", c.Y, goldenFixAdvice, c.X)
	} else {
		fmt.Fprintf(w, "%sgolden state consistent with the live node%s\n", c.G, c.X)
	}
}

// writeBundleInline renders the firmware-bundle block inline (same content
// as bmn.go's printBundleSection, which prints via logger to stdout and is
// therefore not buffer-safe).
func writeBundleInline(w io.Writer, c palette, info *bmnInfo) {
	label := func(name, val string) {
		fmt.Fprintf(w, "%s%-11s%s: %s\n", c.C, name, c.X, scanOrDash(val))
	}
	label("Bundle cur", info.BundleCurrent)
	label("Bundle tgt", info.BundleTarget)
	label("Bundle spec", info.BundleSpec)
	label("DPU bundle", info.DPUBundleCurrent)
	for _, f := range info.BundleFlags {
		fmt.Fprintf(w, "%sFLAG: %s%s\n", c.Y, f, c.X)
	}
	fmt.Fprintln(w)
}

// readVerdictJSON loads <localDir>/verdict.json (falling back to one level
// of subdirectory globbing) and returns the raw bytes plus the parsed subset.
func readVerdictJSON(localDir string) ([]byte, *verdictFile) {
	if localDir == "" {
		return nil, nil
	}
	paths := []string{filepath.Join(localDir, "verdict.json")}
	if matches, err := filepath.Glob(filepath.Join(localDir, "*", "verdict.json")); err == nil {
		paths = append(paths, matches...)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var vf verdictFile
		if json.Unmarshal(b, &vf) != nil {
			return b, nil
		}
		return b, &vf
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Matrix rendering
// ---------------------------------------------------------------------------

var matrixHeaders = []string{"BMN", "GMAC", "SKU", "STATE", "VERDICT", "ISSUES", "NEXT STEPS"}

// matrixCells builds one row. When truncate is set the ISSUES / NEXT STEPS
// cells are capped at matrixCellMax for terminal display.
func matrixCells(r inspectResult, truncate bool) []string {
	gmac, sku, state := "-", "-", "-"
	if r.Info != nil {
		gmac, sku, state = scanOrDash(r.Info.GMAC), scanOrDash(r.Info.SKU), scanOrDash(r.Info.State)
	}
	issues := strings.Join(r.Issues, "; ")
	next := strings.Join(r.NextSteps, "; ")
	if issues == "" {
		issues = "-"
	}
	if next == "" {
		next = "-"
	}
	if truncate {
		issues = truncateCell(issues, matrixCellMax)
		next = truncateCell(next, matrixCellMax)
	}
	return []string{r.BMN, gmac, sku, state, verdictLabel(r), issues, next}
}

func truncateCell(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func matrixWidths(results []inspectResult, truncate bool) []int {
	widths := make([]int, len(matrixHeaders))
	for i, h := range matrixHeaders {
		widths[i] = len(h)
	}
	for _, r := range results {
		for i, v := range matrixCells(r, truncate) {
			if n := len([]rune(v)); n > widths[i] {
				widths[i] = n
			}
		}
	}
	return widths
}

func printMatrix(results []inspectResult, c palette) {
	widths := matrixWidths(results, true)
	pad := func(s string, w int) string {
		return s + strings.Repeat(" ", w-len([]rune(s)))
	}

	fmt.Println()
	var hdr strings.Builder
	for i, h := range matrixHeaders {
		hdr.WriteString(pad(h, widths[i]) + "  ")
	}
	fmt.Printf("%s%s%s\n", c.B, strings.TrimRight(hdr.String(), " "), c.X)

	for _, r := range results {
		cells := matrixCells(r, true)
		var line strings.Builder
		for i, v := range cells {
			cell := pad(v, widths[i])
			if i == 4 { // VERDICT
				cell = verdictColor(c, r.VerdictCode) + cell + c.X
			}
			line.WriteString(cell + "  ")
		}
		fmt.Println(strings.TrimRight(line.String(), " "))
	}
}

// printMatrixMarkdown prints the summary matrix as an ANSI-free markdown
// block — the same content as the .md summary file, fleet advisory included —
// so evidence can be copy-pasted straight out of any terminal (colored,
// piped, or --no-color alike).
func printMatrixMarkdown(results []inspectResult) {
	fmt.Println("--- matrix (markdown — copy for evidence) ---")
	fmt.Print(summaryMd(results))
	fmt.Println("--- end matrix ---")
}

// ---------------------------------------------------------------------------
// Fleet advisory (idle-port false-positive rollup)
// ---------------------------------------------------------------------------

var (
	issueBDFRe    = regexp.MustCompile(`[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]`)
	idlePortIssue = regexp.MustCompile(`^([0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]): .*METAL-4742`)
)

// isPexFalsePositiveOnly reports whether the node belongs to the PEX890xx
// benign false-positive class: either the on-node classifier said so (flag
// confirmed against the mgmt alert feed in inspectOneBMN), or — as a
// pattern-matching fallback for verdicts without the flag — every finding
// matches the METAL-4742 idle-port triplet.
func isPexFalsePositiveOnly(r inspectResult) bool {
	return r.FalsePositive || isIdlePortFalsePositiveOnly(r)
}

// isIdlePortFalsePositiveOnly reports whether every finding on the node
// belongs to the METAL-4742 idle-port false-positive triplet — LINK DOWN
// (width x0) / bridge has NO device behind it / DPC — for a bridge BDF that
// carries the idle-port flag. Any finding on another BDF, or any finding
// without a BDF (bundle state, GPU suite, NVLink), disqualifies the node so
// real problems are never rolled up as false positives.
func isIdlePortFalsePositiveOnly(r inspectResult) bool {
	if r.Err != nil || len(r.Issues) == 0 {
		return false
	}
	idle := map[string]bool{}
	for _, issue := range r.Issues {
		if m := idlePortIssue.FindStringSubmatch(issue); m != nil {
			idle[m[1]] = true
		}
	}
	if len(idle) == 0 {
		return false
	}
	for _, issue := range r.Issues {
		bdf := issueBDFRe.FindString(issue)
		if bdf == "" || !idle[bdf] {
			return false
		}
	}
	return true
}

// fleetAdvisory rolls the per-node results up into fleet-level advice for
// multi-BMN runs: nodes matching only the PEX890xx false-positive class
// (idle-port x0 / self-cleared transient) can be returned to ready in bulk
// per the RNO2 runbook (2026-07-14 standup, same class as XID 109) — no RMA,
// no hardware ticket — after a sampled golden-state check.
func fleetAdvisory(results []inspectResult) []string {
	if len(results) < 2 {
		return nil
	}
	var ready, golden, attention []string
	for _, r := range results {
		switch {
		case r.GoldenMismatch:
			golden = append(golden, r.BMN)
		case isPexFalsePositiveOnly(r):
			ready = append(ready, r.BMN)
		default:
			if len(r.Issues) > 0 {
				attention = append(attention, r.BMN)
			}
		}
	}
	if len(ready) == 0 && len(golden) == 0 {
		return nil
	}
	var lines []string
	if len(ready) > 0 {
		lines = append(lines,
			fmt.Sprintf("Fleet advisory: %d/%d node(s) match ONLY the PEX890xx false-positive pattern (idle-port x0 METAL-4742 / self-cleared transient).", len(ready), len(results)),
			"ADVISE: golden-state check a small sample, then bulk return-to-ready — no RMA, no hardware ticket (RNO2 runbook 2026-07-14, same class as XID 109):")
		for _, bmn := range ready {
			lines = append(lines, fmt.Sprintf("    cwctl flcc node -w return-to-ready -m \"sending to ready\" %s", bmn))
		}
		lines = append(lines,
			fmt.Sprintf("BULK (one paste returns all %d false positive(s) to production):", len(ready)),
			fmt.Sprintf("    for n in %s; do cwctl flcc node -w return-to-ready -m \"sending to ready\" \"$n\"; done",
				strings.Join(ready, " ")))
		lines = append(lines, "CAVEAT: don't bulk-clear blindly — skip any node whose same BDF re-alerts across reboots or that pairs with XID/AER/fallen-off-bus findings.")
	}
	if len(golden) > 0 {
		lines = append(lines,
			fmt.Sprintf("Golden-state errors: %d/%d node(s) — %s.", len(golden), len(results), strings.Join(golden, ", ")),
			"ADVISE: "+goldenFixAdvice+"; do NOT return-to-ready until the golden side is fixed (the alert re-fails the node).")
	}
	if len(attention) > 0 {
		lines = append(lines, "Needs individual attention (findings beyond the false-positive pattern): "+strings.Join(attention, ", "))
	}
	return lines
}

func printFleetAdvisory(results []inspectResult, c palette) {
	lines := fleetAdvisory(results)
	if len(lines) == 0 {
		return
	}
	fmt.Println()
	for i, l := range lines {
		if i == 0 {
			fmt.Printf("%s%s%s\n", c.Y, l, c.X)
		} else {
			fmt.Println(l)
		}
	}
}

// ---------------------------------------------------------------------------
// Summary files (untruncated, ANSI-free)
// ---------------------------------------------------------------------------

// writeSummaries writes summary_<ts>.txt and summary_<ts>.md into outDir and
// returns their paths ("" on write failure). The .md file carries the FULL
// per-node run output (summaryMdFull); the .txt matrix is unchanged.
func writeSummaries(outDir string, results []inspectResult) (txtPath, mdPath string) {
	if outDir == "" {
		return "", ""
	}
	ts := time.Now().Format("20060102_150405")
	txtPath = filepath.Join(outDir, "summary_"+ts+".txt")
	mdPath = filepath.Join(outDir, "summary_"+ts+".md")

	if err := os.WriteFile(txtPath, []byte(summaryTxt(results)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "WARN: cannot write", txtPath, "—", err)
		txtPath = ""
	}
	if err := os.WriteFile(mdPath, []byte(summaryMdFull(results)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "WARN: cannot write", mdPath, "—", err)
		mdPath = ""
	}
	return txtPath, mdPath
}

func summaryTxt(results []inspectResult) string {
	var b strings.Builder
	widths := matrixWidths(results, true)
	pad := func(s string, w int) string {
		return s + strings.Repeat(" ", w-len([]rune(s)))
	}
	for i, h := range matrixHeaders {
		b.WriteString(pad(h, widths[i]) + "  ")
	}
	b.WriteString("\n")
	for _, r := range results {
		for i, v := range matrixCells(r, true) {
			b.WriteString(pad(v, widths[i]) + "  ")
		}
		b.WriteString("\n")
		if len(r.Issues) > 0 {
			b.WriteString("  issues: " + strings.Join(r.Issues, "; ") + "\n")
		}
		if len(r.NextSteps) > 0 {
			b.WriteString("  next:   " + strings.Join(r.NextSteps, "; ") + "\n")
		}
		if r.AISummary != "" {
			b.WriteString("  ai:     " + r.AISummary + "\n")
		}
		if r.OutDir != "" {
			b.WriteString("  out:    " + r.OutDir + "\n")
		}
	}
	for i, l := range fleetAdvisory(results) {
		if i == 0 {
			b.WriteString("\n")
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

// summaryCatMax / summaryCatCount bound the ISSUES / NEXT STEPS rollup cells
// in the markdown matrix: each category label is capped at summaryCatMax
// runes and at most summaryCatCount categories are listed before "…".
const (
	summaryCatMax   = 40
	summaryCatCount = 4
)

// findingCategory reduces one finding string to a short grouping key: drop
// the leading "<bdf>: " prefix, cut at the first " @ ", " — " or " (" so
// variants of the same failure collapse into one bucket, and normalize any
// remaining BDF so per-device findings group across devices.
func findingCategory(s string) string {
	if loc := issueBDFRe.FindStringIndex(s); loc != nil && loc[0] == 0 && strings.HasPrefix(s[loc[1]:], ": ") {
		s = s[loc[1]+2:]
	}
	for _, sep := range []string{" @ ", " — ", " ("} {
		if i := strings.Index(s, sep); i >= 0 {
			s = s[:i]
		}
	}
	s = issueBDFRe.ReplaceAllString(strings.TrimSpace(s), "[bdf]")
	if s == "" {
		s = "finding"
	}
	return truncateCell(s, summaryCatMax)
}

// summarizeFindings compresses a findings list into a short markdown table
// cell — "22 issues: LINK DOWN ×10, speed degraded ×9, …" — grouping by
// findingCategory. Lists that already fit a terminal cell pass through
// verbatim; the full strings always live in the per-BMN detail section
// below the table, so nothing is lost.
func summarizeFindings(items []string, noun string) string {
	if len(items) == 0 {
		return "-"
	}
	joined := strings.Join(items, "; ")
	if len([]rune(joined)) <= matrixCellMax {
		return joined
	}
	var cats []string
	counts := map[string]int{}
	for _, it := range items {
		c := findingCategory(it)
		if counts[c] == 0 {
			cats = append(cats, c)
		}
		counts[c]++
	}
	var parts []string
	for i, c := range cats {
		if i == summaryCatCount {
			parts = append(parts, "…")
			break
		}
		if counts[c] > 1 {
			c = fmt.Sprintf("%s ×%d", c, counts[c])
		}
		parts = append(parts, c)
	}
	if len(items) != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s: %s", len(items), noun, strings.Join(parts, ", "))
}

// rollupFindings deduplicates findings that are identical modulo a single PCI
// BDF: each such group collapses to "<pattern> — N BDFs: bdf1 bdf2 ..." with
// the BDF in the pattern replaced by "<bdf>". Findings without exactly one
// BDF, and groups of one, pass through verbatim. Output order follows first
// occurrence and every BDF stays listed, so nothing is lost — a node with 10
// identical idle-port triplets shrinks from 30 lines to 3.
func rollupFindings(findings []string) []string {
	type group struct {
		first string // verbatim finding, used when the group stays a singleton
		slot  int    // index in out reserved for this group
		bdfs  []string
	}
	out := make([]string, 0, len(findings))
	groups := map[string]*group{}
	for _, f := range findings {
		bdfs := issueBDFRe.FindAllString(f, -1)
		if len(bdfs) != 1 {
			out = append(out, f)
			continue
		}
		pattern := issueBDFRe.ReplaceAllString(f, "<bdf>")
		g, ok := groups[pattern]
		if !ok {
			g = &group{first: f, slot: len(out)}
			groups[pattern] = g
			out = append(out, "") // placeholder, filled below
		}
		g.bdfs = append(g.bdfs, bdfs[0])
	}
	for _, g := range groups {
		if len(g.bdfs) == 1 {
			out[g.slot] = g.first
		} else {
			out[g.slot] = fmt.Sprintf("%s — %d BDFs: %s", issueBDFRe.ReplaceAllString(g.first, "<bdf>"), len(g.bdfs), strings.Join(g.bdfs, " "))
		}
	}
	return out
}

// summaryMd renders the markdown summary: the matrix with short category
// rollups in the ISSUES / NEXT STEPS cells, then one "### <BMN>" detail
// section per node with findings (repeated per-BDF findings rolled up, every
// BDF listed), then the fleet advisory. ANSI-free by construction — this is
// the copy-paste evidence behind printMatrixMarkdown, writeSummaries, --copy.
func summaryMd(results []inspectResult) string {
	esc := func(s string) string { return strings.ReplaceAll(s, "|", "\\|") }
	var b strings.Builder
	b.WriteString("| " + strings.Join(matrixHeaders, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat(" --- |", len(matrixHeaders)) + "\n")
	for _, r := range results {
		cells := matrixCells(r, false)
		cells[5] = summarizeFindings(r.Issues, "issue")   // ISSUES
		cells[6] = summarizeFindings(r.NextSteps, "step") // NEXT STEPS
		for i := range cells {
			cells[i] = esc(cells[i])
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	for _, r := range results {
		if len(r.Issues) == 0 && len(r.NextSteps) == 0 {
			continue
		}
		b.WriteString("\n### " + r.BMN + " — " + verdictLabel(r) + "\n")
		if len(r.Issues) > 0 {
			b.WriteString("\n**Issues**\n\n")
			for _, s := range rollupFindings(r.Issues) {
				b.WriteString("- " + s + "\n")
			}
		}
		if len(r.NextSteps) > 0 {
			b.WriteString("\n**Next steps**\n\n")
			for _, s := range rollupFindings(r.NextSteps) {
				b.WriteString("- " + s + "\n")
			}
		}
	}
	if lines := fleetAdvisory(results); len(lines) > 0 {
		b.WriteString("\n**" + lines[0] + "**\n")
		for _, l := range lines[1:] {
			b.WriteString("\n" + l + "\n")
		}
	}
	return b.String()
}

// summaryMdFull is summaryMd plus each node's complete buffered run report
// (ANSI stripped) in a fenced block — the summary_<ts>.md evidence file
// carries the full story, while the terminal markdown block and --copy
// clipboard stay compact (summaryMd).
func summaryMdFull(results []inspectResult) string {
	var b strings.Builder
	b.WriteString(summaryMd(results))
	for _, r := range results {
		if r.Output == "" {
			continue
		}
		out := ansiRe.ReplaceAllString(r.Output, "")
		// A literal ``` inside the report would terminate the fence early.
		out = strings.ReplaceAll(out, "```", "`` `")
		b.WriteString("\n\n## Full report — " + r.BMN + "\n\n```text\n")
		b.WriteString(out)
		b.WriteString("\n```\n")
	}
	return b.String()
}
