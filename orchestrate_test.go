package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func fpResult(bmn, bdf string) inspectResult {
	return inspectResult{
		BMN:         bmn,
		VerdictCode: 1,
		Issues: []string{
			"LINK DOWN (width x0) @ " + bdf + " (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0))",
			bdf + ": bridge has NO device behind it — idle-port x0 (METAL-4742 false-positive pattern)",
			bdf + ": DPC triggered",
		},
	}
}

func TestIsIdlePortFalsePositiveOnly(t *testing.T) {
	if !isIdlePortFalsePositiveOnly(fpResult("ss1", "0000:5c:00.0")) {
		t.Error("pure idle-port triplet should classify as false-positive-only")
	}

	// Mixed node (the ss892297x4307765 shape): idle-port bridges PLUS a real
	// wrong-part/FW finding on a different BDF must NOT roll up.
	mixed := fpResult("ss2", "0000:53:10.0")
	mixed.Issues = append(mixed.Issues,
		"speed degraded @ 0000:18:02.0 (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0))",
		"0000:18:02.0: endpoint LnkCap below bridge — wrong part/FW",
	)
	if isIdlePortFalsePositiveOnly(mixed) {
		t.Error("node with findings beyond the idle-port pattern must not classify as false-positive-only")
	}

	// A BDF-less finding (bundle state, GPU suite) disqualifies the node.
	bundle := fpResult("ss3", "0000:d8:10.0")
	bundle.Issues = append(bundle.Issues, "node-fwbundle current unset — legacy alert path")
	if isIdlePortFalsePositiveOnly(bundle) {
		t.Error("BDF-less finding must disqualify the false-positive-only rollup")
	}

	if isIdlePortFalsePositiveOnly(inspectResult{BMN: "ss4"}) {
		t.Error("node with no issues must not classify")
	}
}

func TestIsPexFalsePositiveOnly(t *testing.T) {
	// The on-node flag classifies regardless of issue wording (covers the
	// self-cleared transient, which has no METAL-4742 line).
	flagged := inspectResult{
		BMN: "ss1", VerdictCode: 0, FalsePositive: true,
		Issues: []string{"0000:d8:10.0: alert BDF at full speed/width on re-check — PEX890xx transient sampling false positive"},
	}
	if !isPexFalsePositiveOnly(flagged) {
		t.Error("on-node pex_false_positive flag must classify")
	}
	// Pattern fallback still works without the flag.
	if !isPexFalsePositiveOnly(fpResult("ss2", "0000:5c:00.0")) {
		t.Error("idle-port issue pattern must classify without the flag")
	}
	if isPexFalsePositiveOnly(inspectResult{BMN: "ss3", VerdictCode: 0}) {
		t.Error("clean node must not classify")
	}
}

func TestFleetAdvisory(t *testing.T) {
	if fleetAdvisory([]inspectResult{fpResult("ss1", "0000:5c:00.0")}) != nil {
		t.Error("single-BMN runs must not emit a fleet advisory")
	}

	mixed := fpResult("ss2", "0000:53:10.0")
	mixed.Issues = append(mixed.Issues, "0000:18:02.0: endpoint LnkCap below bridge — wrong part/FW")
	lines := fleetAdvisory([]inspectResult{
		fpResult("ss1", "0000:5c:00.0"),
		mixed,
		fpResult("ss3", "0000:d8:10.0"),
	})
	if len(lines) != 8 {
		t.Fatalf("want 8 advisory lines, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "2/3") {
		t.Errorf("rollup count wrong: %q", lines[0])
	}
	if !strings.Contains(lines[1], "golden-state") || !strings.Contains(lines[1], "return-to-ready") {
		t.Errorf("advice line missing golden-state/return-to-ready wording: %q", lines[1])
	}
	if !strings.Contains(lines[2], `cwctl flcc node -w return-to-ready -m "sending to ready" ss1`) {
		t.Errorf("missing per-node return-to-ready command: %q", lines[2])
	}
	if !strings.Contains(lines[3], "ss3") {
		t.Errorf("missing per-node return-to-ready command for ss3: %q", lines[3])
	}
	if !strings.Contains(lines[4], "BULK") || !strings.Contains(lines[4], "2") {
		t.Errorf("bulk label line wrong: %q", lines[4])
	}
	if !strings.Contains(lines[5], `for n in ss1 ss3; do cwctl flcc node -w return-to-ready -m "sending to ready" "$n"; done`) {
		t.Errorf("bulk one-paste loop wrong: %q", lines[5])
	}
	if strings.Contains(lines[5], "ss2") {
		t.Errorf("bulk loop must not include the needs-attention node: %q", lines[5])
	}
	if !strings.Contains(lines[6], "re-alerts across reboots") {
		t.Errorf("caveat line missing recurrence wording: %q", lines[6])
	}
	if !strings.Contains(lines[7], "ss2") || strings.Contains(lines[7], "ss1") {
		t.Errorf("attention line should name only the mixed node: %q", lines[7])
	}

	healthy := []inspectResult{{BMN: "a", VerdictCode: 0}, {BMN: "b", VerdictCode: 0}}
	if fleetAdvisory(healthy) != nil {
		t.Error("all-healthy fleet must not emit an advisory")
	}
}

func TestRollupFindings(t *testing.T) {
	if got := rollupFindings(nil); len(got) != 0 {
		t.Errorf("rollupFindings(nil) = %v; want empty", got)
	}

	// Repeated idle-port triplet across 3 BDFs plus a singleton and a
	// BDF-less finding: the triplet rolls up to 3 lines, the rest verbatim.
	bdfs := []string{"0000:16:10.0", "0000:5c:00.0", "0000:d8:10.0"}
	var findings []string
	for _, bdf := range bdfs {
		findings = append(findings,
			"LINK DOWN (width x0) @ "+bdf+" (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0))",
			bdf+": bridge has NO device behind it — idle-port x0 (METAL-4742 false-positive pattern)",
			bdf+": DPC triggered",
		)
	}
	findings = append(findings,
		"speed degraded @ 0000:18:02.0 (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0))",
		"node-fwbundle current unset — legacy alert path",
	)
	got := rollupFindings(findings)
	if len(got) != 5 {
		t.Fatalf("want 5 rolled-up lines, got %d: %v", len(got), got)
	}
	want0 := "LINK DOWN (width x0) @ <bdf> (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0)) — 3 BDFs: 0000:16:10.0 0000:5c:00.0 0000:d8:10.0"
	if got[0] != want0 {
		t.Errorf("rolled-up line = %q; want %q", got[0], want0)
	}
	for i, line := range got[:3] {
		for _, bdf := range bdfs {
			if !strings.Contains(line, bdf) {
				t.Errorf("line %d lost BDF %s: %q", i, bdf, line)
			}
		}
		if !strings.Contains(line, "3 BDFs:") {
			t.Errorf("line %d missing count: %q", i, line)
		}
	}
	if got[3] != findings[len(findings)-2] {
		t.Errorf("singleton must pass through verbatim, got %q", got[3])
	}
	if got[4] != "node-fwbundle current unset — legacy alert path" {
		t.Errorf("BDF-less finding must pass through verbatim, got %q", got[4])
	}

	// A finding carrying two BDFs is ambiguous for a rollup — verbatim.
	multi := []string{
		"0000:16:10.0 downstream of 0000:16:00.0: DPC triggered",
		"0000:5c:10.0 downstream of 0000:5c:00.0: DPC triggered",
	}
	if got := rollupFindings(multi); got[0] != multi[0] || got[1] != multi[1] {
		t.Errorf("multi-BDF findings must pass through verbatim, got %v", got)
	}
}

func TestSummarizeFindings(t *testing.T) {
	if got := summarizeFindings(nil, "issue"); got != "-" {
		t.Errorf("empty list = %q; want -", got)
	}
	short := []string{"DPC triggered", "reboot pending"}
	if got := summarizeFindings(short, "issue"); got != "DPC triggered; reboot pending" {
		t.Errorf("short list should pass through verbatim, got %q", got)
	}
}

func TestSummaryMdCompactTableAndDetailSections(t *testing.T) {
	longIssue := "LINK DOWN (width x0) @ 0000:5c:00.0 (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0))"
	sick := inspectResult{
		BMN: "ss-sick", VerdictCode: 1,
		Issues: []string{
			longIssue,
			"LINK DOWN (width x0) @ 0000:5d:00.0 (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0))",
			"speed degraded @ 0000:18:02.0 (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0))",
			"0000:5c:00.0: bridge has NO device behind it — idle-port x0 (METAL-4742 false-positive pattern)",
			"0000:5d:00.0: bridge has NO device behind it — idle-port x0 (METAL-4742 false-positive pattern)",
			"0000:18:02.0: endpoint LnkCap|below bridge — wrong part/FW, check BOM",
		},
		NextSteps: []string{
			"verify 0000:5c:00.0 against golden state / vendor topology BEFORE any physical action (likely false positive)",
			"verify 0000:5d:00.0 against golden state / vendor topology BEFORE any physical action (likely false positive)",
			"ADVISE: golden-state check a small sample, then bulk return-to-ready",
		},
	}
	healthy := inspectResult{BMN: "ss-ok", VerdictCode: 0}
	md := summaryMd([]inspectResult{sick, healthy})

	table, details, ok := strings.Cut(md, "\n### ")
	if !ok {
		t.Fatalf("summaryMd missing per-BMN detail section:\n%s", md)
	}
	// Table: compact rollup cells, no full finding strings, pipes escaped.
	if strings.Contains(table, longIssue) {
		t.Error("table must carry the rollup, not full issue strings")
	}
	if !strings.Contains(table, "6 issues: LINK DOWN ×2") {
		t.Errorf("table missing categorized issue rollup:\n%s", table)
	}
	if !strings.Contains(table, "3 steps:") {
		t.Errorf("table missing categorized next-step rollup:\n%s", table)
	}
	if !strings.Contains(table, `\|`) {
		t.Error("table cells must escape | characters")
	}
	// Details: repeated per-BDF findings roll up (all BDFs listed), the
	// rest verbatim; healthy node gets no section.
	if !strings.HasPrefix(details, "ss-sick — DEGRADED\n") {
		t.Errorf("first detail section should be ss-sick, got:\n%s", details)
	}
	if !strings.Contains(details, "**Issues**") || !strings.Contains(details, "**Next steps**") {
		t.Error("detail section missing Issues / Next steps lists")
	}
	if !strings.Contains(details, "- LINK DOWN (width x0) @ <bdf> (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0)) — 2 BDFs: 0000:5c:00.0 0000:5d:00.0\n") {
		t.Errorf("detail section missing rolled-up LINK DOWN bullet:\n%s", details)
	}
	if !strings.Contains(details, "- speed degraded @ 0000:18:02.0 (Broadcom / LSI PEX890xx PCIe Gen 5 Switch (rev b0))\n") {
		t.Errorf("detail section missing verbatim singleton bullet:\n%s", details)
	}
	if !strings.Contains(details, "- verify <bdf> against golden state / vendor topology BEFORE any physical action (likely false positive) — 2 BDFs: 0000:5c:00.0 0000:5d:00.0\n") {
		t.Errorf("detail section missing rolled-up next-step bullet:\n%s", details)
	}
	if !strings.Contains(details, "- ADVISE: golden-state check a small sample, then bulk return-to-ready\n") {
		t.Errorf("detail section missing verbatim next-step bullet:\n%s", details)
	}
	if strings.Contains(md, "### ss-ok") {
		t.Error("healthy node must not get a detail section")
	}
	// The txt summary and colored matrix paths are untouched — no markdown
	// artifacts may leak there.
	txt := summaryTxt([]inspectResult{sick, healthy})
	if strings.Contains(txt, "<bdf>") || strings.Contains(txt, "###") {
		t.Errorf("summaryTxt must not carry markdown rollups:\n%s", txt)
	}
}

func TestVerdictLabel(t *testing.T) {
	if got := verdictLabel(inspectResult{VerdictCode: 0, FalsePositive: true}); got != "FALSE-POS" {
		t.Errorf("verdictLabel(FP) = %q; want FALSE-POS", got)
	}
	if got := verdictLabel(inspectResult{VerdictCode: 1}); got != "DEGRADED" {
		t.Errorf("verdictLabel(degraded) = %q; want DEGRADED", got)
	}
}

func TestSummariesCarryFleetAdvisory(t *testing.T) {
	results := []inspectResult{fpResult("ss1", "0000:5c:00.0"), fpResult("ss2", "0000:d8:10.0")}
	if txt := summaryTxt(results); !strings.Contains(txt, "Fleet advisory: 2/2") {
		t.Error("summaryTxt missing fleet advisory")
	}
	if md := summaryMd(results); !strings.Contains(md, "Fleet advisory: 2/2") {
		t.Error("summaryMd missing fleet advisory")
	}
}

func TestSummaryMdFull(t *testing.T) {
	results := []inspectResult{
		{
			BMN: "ss-full", VerdictCode: 1,
			Issues: []string{"LINK DOWN (width x0) @ 0000:5c:00.0"},
			Output: "\x1b[1m━━━ ss-full ━━━\x1b[0m\nfull node report line\ninline fence ```danger```\n",
		},
		{BMN: "ss-noout", VerdictCode: 0}, // no Output → no full-report section
	}

	full := summaryMdFull(results)
	compact := summaryMd(results)

	// Full file = compact summary + fenced full report per node with output.
	if !strings.HasPrefix(full, compact) {
		t.Error("summaryMdFull must start with the compact summaryMd content")
	}
	if !strings.Contains(full, "## Full report — ss-full") {
		t.Errorf("summaryMdFull missing full-report section:\n%s", full)
	}
	if !strings.Contains(full, "```text\n━━━ ss-full ━━━\nfull node report line") {
		t.Errorf("summaryMdFull missing fenced report body:\n%s", full)
	}
	if strings.Contains(full, "\x1b") {
		t.Error("summaryMdFull must strip ANSI escapes from the report")
	}
	if strings.Contains(full, "```danger```") {
		t.Error("summaryMdFull must escape ``` inside the report so the fence cannot break")
	}
	if strings.Contains(full, "Full report — ss-noout") {
		t.Error("nodes without Output must not get a full-report section")
	}

	// The compact summary (terminal block / --copy) stays compact.
	if strings.Contains(compact, "Full report") || strings.Contains(compact, "full node report line") {
		t.Error("summaryMd must not carry the full report")
	}
}

func TestSummaryMdIsANSIFree(t *testing.T) {
	// summaryMd is copy-paste evidence: even when the terminal palette is in
	// play, the markdown must never carry escape sequences.
	t.Setenv("FORCE_COLOR", "1")
	results := []inspectResult{fpResult("ss1", "0000:5c:00.0"), fpResult("ss2", "0000:d8:10.0")}
	md := summaryMd(results)
	if strings.Contains(md, "\x1b") {
		t.Error("summaryMd contains ANSI escape sequences")
	}
	if !strings.Contains(md, "| BMN |") {
		t.Error("summaryMd missing matrix header row")
	}
	if !strings.Contains(md, "Fleet advisory: 2/2") {
		t.Error("summaryMd missing fleet advisory for 2 FP nodes")
	}
}

func TestPrintMatrixMarkdownDelimiters(t *testing.T) {
	results := []inspectResult{fpResult("ss1", "0000:5c:00.0"), fpResult("ss2", "0000:d8:10.0")}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	printMatrixMarkdown(results)
	w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("want delimiters plus body, got %d lines: %q", len(lines), out)
	}
	if lines[0] != "--- matrix (markdown — copy for evidence) ---" {
		t.Errorf("first line = %q; want the opening delimiter", lines[0])
	}
	if lines[len(lines)-1] != "--- end matrix ---" {
		t.Errorf("last line = %q; want the closing delimiter", lines[len(lines)-1])
	}
	body := strings.Join(lines[1:len(lines)-1], "\n")
	if !strings.Contains(body, "| BMN |") {
		t.Error("markdown body missing summaryMd header row")
	}
}

func TestSummariesSingleResult(t *testing.T) {
	results := []inspectResult{fpResult("ss1", "0000:5c:00.0")}
	txt, md := summaryTxt(results), summaryMd(results)
	if txt == "" || md == "" {
		t.Fatal("single-result summaries must be non-empty")
	}
	if !strings.Contains(txt, "ss1") {
		t.Error("summaryTxt missing the BMN")
	}
	if !strings.Contains(md, "ss1") {
		t.Error("summaryMd missing the BMN")
	}
	if strings.Contains(txt, "Fleet advisory") {
		t.Error("summaryTxt must not carry a fleet advisory for a single-BMN run")
	}
	if strings.Contains(md, "Fleet advisory") {
		t.Error("summaryMd must not carry a fleet advisory for a single-BMN run")
	}
}

func TestCopyToClipboardNoTools(t *testing.T) {
	// Deterministic failure path only: with an empty PATH no clipboard tool
	// resolves, so the error must name the tools it looked for.
	t.Setenv("PATH", t.TempDir())
	err := copyToClipboard("| BMN |\n")
	if err == nil {
		t.Fatal("copyToClipboard with no tools on PATH must return an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pbcopy") {
		t.Errorf("error should name the missing clipboard tools: %q", msg)
	}
}
