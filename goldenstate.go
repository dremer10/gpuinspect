package main

// goldenstate.go — laptop-side golden-state verification (GloQL).
//
// The NodePCILinkSpeed/WidthUnexpected alerts compare node_pci_cur_* against
// expected_pci_link_* — the per-(cw_sku, fwbundle, device) "golden" table
// (fleetops-common-ansible get_pci_golden_state, served by trino-exporter).
// The join is purely on the BDF string, so two whole classes of alert are
// golden-state ERRORS, not hardware faults:
//   - enumeration/layout mismatch: the node enumerates a DIFFERENT device at
//     the alert BDF than the golden row expects (bundle change / BIOS
//     enumeration shift) — the live link is perfect and the alert compares
//     apples to oranges;
//   - stale golden values: same device, wrong expected speed/width numbers.
//
// Neither clears with a power drain or an RMA — the fix is an enumeration fix
// (BIOS settings/NVRAM reset + reapply profile) or a golden-state refresh.
// These alerts carry failure_cause="true", so a node returned to ready just
// fails again until the golden side is fixed.
//
// This check reproduces the alert's comparison per BDF — device identity
// included, which the alert rule itself never looks at — using the same
// Grafana token machinery as the nodebot preflight. Read-only (HTTP GET),
// advisory-only, never fatal to the run. Skip with --no-golden.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// goldenDSUID is the GloQL (fleet Prometheus) datasource behind the Grafana
// proxy — the same instance/datasource the nodebot preflight validates
// against. Override with GPUINSPECT_GOLDEN_DS_UID.
const goldenDSUID = "benz42hlglhj4a"

// goldenFixAdvice is the remediation line for every golden-state error class.
const goldenFixAdvice = "fix enumeration (BIOS settings/NVRAM reset + reapply profile, then reboot) OR refresh the golden state for this SKU+bundle (fleetops-common-ansible get_pci_golden_state) — a power drain or RMA will NOT clear a golden-state alert"

// idleWidthRaw is the kernel's saturated raw width on idle bridge ports
// (METAL-4742): a downstream port with no trained link partner reports 63.
const idleWidthRaw = 63

// goldenDev is one BDF's view from either side of the comparison: the golden
// table (expected_pci_link_*) or the live exporter (node_pci_cur_*).
type goldenDev struct {
	Speed, Width       float64 // Speed in raw exporter units (32 GT/s = 32e9)
	HasSpeed, HasWidth bool
	DeviceName         string
	VendorID, DeviceID string
}

// goldenData is the fetched comparison input, keyed by BDF.
type goldenData struct {
	Expected  map[string]*goldenDev
	Live      map[string]*goldenDev
	Conflicts []string // BDFs whose golden rows disagree across core-services clusters
}

// goldenCheck is the classification result consumed by orchestrate.go.
type goldenCheck struct {
	Skipped   string // non-empty = check did not run (reason); everything else empty
	Mismatch  bool   // a golden-state error was found (drives GOLDEN-MIS verdict)
	Lines     []string
	Issues    []string
	NextSteps []string
}

// goldenStateCheck fetches the golden table for the BMN's (SKU, fwbundle) and
// the node's live PCI series, then classifies the alert BDFs. degraded maps
// BDF -> on-node degraded flag (from verdict.json devices); onNodeOK reports
// whether the on-node inspection produced a verdict at all — without it a
// value disagreement cannot be blamed on the table (the link state is
// unverified).
func goldenStateCheck(info *bmnInfo, degraded map[string]bool, onNodeOK bool) goldenCheck {
	switch {
	case info.SKU == "":
		return goldenCheck{Skipped: "BMN has no cwSku — cannot select golden rows"}
	case info.BundleCurrent == "":
		return goldenCheck{Skipped: "node-fwbundle.current unset — node is on the legacy alert path (bundle FLAG already raised)"}
	case info.GMAC == "":
		return goldenCheck{Skipped: "no gMAC — cannot select the node's live PCI series"}
	}
	token := grafanaToken()
	if token == "" {
		return goldenCheck{Skipped: "no Grafana token (GRAFANA_API_TOKEN, or the shared token-viewer item via op signin)"}
	}
	d, err := fetchGoldenData(token, info)
	if err != nil {
		return goldenCheck{Skipped: "query failed: " + err.Error()}
	}
	return classifyGolden(info, d, degraded, onNodeOK)
}

// classifyGolden is the pure comparison: golden table vs live series vs
// on-node link state, per alert BDF plus a node-wide layout sweep.
func classifyGolden(info *bmnInfo, d *goldenData, degraded map[string]bool, onNodeOK bool) goldenCheck {
	gc := goldenCheck{}
	line := func(format string, a ...interface{}) { gc.Lines = append(gc.Lines, fmt.Sprintf(format, a...)) }
	advise := func(s string) { gc.NextSteps = append(gc.NextSteps, "ADVISE: "+s) }

	line("golden rows for (cw_sku=%s, fwbundle=%s): %d   live PCI series on %s: %d",
		info.SKU, info.BundleCurrent, len(d.Expected), info.GMAC, len(d.Live))

	if len(d.Expected) == 0 {
		gc.Mismatch = true
		issue := fmt.Sprintf("golden-state: NO expected_pci_link_* rows published for (cw_sku=%s, fwbundle=%s) — NodeMissingPCIGoldenState class", info.SKU, info.BundleCurrent)
		gc.Issues = append(gc.Issues, issue)
		line("MISSING: %s", issue)
		advise(goldenFixAdvice)
		return gc
	}
	for _, bdf := range d.Conflicts {
		gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state: rows for %s CONFLICT across core-services clusters — golden table inconsistent, treat expected values as suspect", bdf))
	}

	for _, bdf := range info.PCIAlertBDFs {
		exp, live := d.Expected[bdf], d.Live[bdf]
		switch {
		case exp == nil && live == nil:
			line("%s  no golden row and no live series — device not tracked by the exporter", bdf)
			gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state: alert BDF %s absent from both the golden table and the live exporter series", bdf))
		case exp == nil:
			gc.Mismatch = true
			line("%s  live %s but NO golden row — BDF not in the golden table for this SKU+bundle", bdf, fmtGoldenDev(live))
			gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state: alert BDF %s (%s) has NO golden row for (cw_sku=%s, fwbundle=%s) — table changed since the alert fired or the BDF was never captured", bdf, live.DeviceName, info.SKU, info.BundleCurrent))
			advise(goldenFixAdvice)
		case live == nil:
			line("%s  golden %s but no live series — node-pci-exporter is not reporting this BDF", bdf, fmtGoldenDev(exp))
			gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state: alert BDF %s has a golden row but NO live node_pci_cur_* series — device absent from the bus or exporter stale", bdf))
		case !sameGoldenIdentity(exp, live):
			gc.Mismatch = true
			line("%s  IDENTITY MISMATCH — live %s  vs  golden %s", bdf, fmtGoldenDev(live), fmtGoldenDev(exp))
			gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state MISMATCH @ %s: node enumerates %s but the golden row expects %s — enumeration/layout differs from the golden table; the alert is comparing two different devices, NOT a degraded link", bdf, fmtGoldenDev(live), fmtGoldenDev(exp)))
			advise(goldenFixAdvice)
		case goldenValuesDiffer(exp, live):
			if degraded[bdf] {
				line("%s  golden AGREES with the alert — golden %s, live %s (link degraded on-node)", bdf, fmtGoldenVals(exp), fmtGoldenVals(live))
				gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state agrees with the alert @ %s: golden expects %s, live is %s — genuine degradation, follow the runbook path", bdf, fmtGoldenVals(exp), fmtGoldenVals(live)))
			} else if onNodeOK {
				gc.Mismatch = true
				line("%s  STALE golden values — same device, golden %s vs live %s at full capability", bdf, fmtGoldenVals(exp), fmtGoldenVals(live))
				gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state STALE @ %s (%s): live link runs %s at full capability on-node but the golden row says %s — wrong expected values for this SKU+bundle", bdf, live.DeviceName, fmtGoldenVals(live), fmtGoldenVals(exp)))
				advise(goldenFixAdvice)
			} else {
				line("%s  golden %s vs live %s — link state UNVERIFIED (no on-node verdict)", bdf, fmtGoldenVals(exp), fmtGoldenVals(live))
				gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state: values differ @ %s (golden %s, live %s) but the on-node link state is unverified — re-run the inspection before physical work", bdf, fmtGoldenVals(exp), fmtGoldenVals(live)))
			}
		default:
			line("%s  MATCH — %s (golden and live agree; alert should clear on the next evaluation)", bdf, fmtGoldenDev(live))
			gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state now MATCHES live @ %s (%s) — golden table updated since the alert fired; alert should clear on its own", bdf, fmtGoldenVals(live)))
			advise(fmt.Sprintf("golden and live agree at %s — re-check the alert; if cleared, proceed with the normal return-to-ready flow", bdf))
		}
	}

	// Node-wide layout sweep: identity mismatches beyond the alert BDFs are
	// the bundle-flip signature — one alert is usually the tip of a shifted
	// enumeration, and the count tells bulk-vs-single-BDF apart.
	var layout []string
	for bdf, exp := range d.Expected {
		if live := d.Live[bdf]; live != nil && !sameGoldenIdentity(exp, live) {
			layout = append(layout, bdf)
		}
	}
	sort.Strings(layout)
	if len(layout) > 0 {
		gc.Mismatch = true
		line("layout sweep: %d/%d BDF(s) enumerate a different device than their golden row: %s",
			len(layout), len(d.Expected), strings.Join(layout, " "))
		gc.Issues = append(gc.Issues, fmt.Sprintf("golden-state: %d BDF(s) node-wide enumerate a different device than the golden table (%s) — enumeration/layout shift, typically after a bundle change", len(layout), strings.Join(layout, " ")))
	}
	return gc
}

// sameGoldenIdentity compares what device sits at a BDF on each side —
// vendor:device IDs when both sides carry them, device name otherwise.
func sameGoldenIdentity(a, b *goldenDev) bool {
	if a.VendorID != "" && a.DeviceID != "" && b.VendorID != "" && b.DeviceID != "" {
		return a.VendorID == b.VendorID && a.DeviceID == b.DeviceID
	}
	return a.DeviceName == b.DeviceName
}

// goldenValuesDiffer compares expected vs live speed/width where both sides
// report. Idle-port raw width (63, METAL-4742) and an expected-idle 0 count
// as agreeing — the alert rules special-case them the same way.
func goldenValuesDiffer(exp, live *goldenDev) bool {
	if exp.HasSpeed && live.HasSpeed && exp.Speed != live.Speed {
		return true
	}
	if exp.HasWidth && live.HasWidth {
		idle := func(w float64) bool { return w == 0 || w == idleWidthRaw }
		if exp.Width != live.Width && !(idle(exp.Width) && idle(live.Width)) {
			return true
		}
	}
	return false
}

func fmtGT(v float64) string {
	return strconv.FormatFloat(v/1e9, 'g', -1, 64) + " GT/s"
}

func fmtGoldenVals(d *goldenDev) string {
	var parts []string
	if d.HasSpeed {
		parts = append(parts, fmtGT(d.Speed))
	}
	if d.HasWidth {
		w := fmt.Sprintf("x%s", strconv.FormatFloat(d.Width, 'g', -1, 64))
		if d.Width == idleWidthRaw {
			w += " (idle-port raw)"
		}
		parts = append(parts, w)
	}
	if len(parts) == 0 {
		return "<no values>"
	}
	return strings.Join(parts, " ")
}

func fmtGoldenDev(d *goldenDev) string {
	name := d.DeviceName
	if name == "" {
		name = "<unknown device>"
	}
	id := ""
	if d.VendorID != "" && d.DeviceID != "" {
		id = " [" + d.VendorID + ":" + d.DeviceID + "]"
	}
	return name + id + " " + fmtGoldenVals(d)
}

// ---------------------------------------------------------------------------
// GloQL fetch (Grafana datasource proxy, same path as the nodebot preflight)
// ---------------------------------------------------------------------------

type promResult struct {
	Metric map[string]string `json:"metric"`
	Value  []interface{}     `json:"value"` // [ts, "value"]
}

type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []promResult `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

func fetchGoldenData(token string, info *bmnInfo) (*goldenData, error) {
	d := &goldenData{Expected: map[string]*goldenDev{}, Live: map[string]*goldenDev{}}
	conflicts := map[string]bool{}
	expSel := fmt.Sprintf(`{cw_sku=%q,fwbundle=%q}`, info.SKU, info.BundleCurrent)
	liveSel := fmt.Sprintf(`{node=%q}`, info.GMAC)
	for _, q := range []struct {
		expr  string
		into  map[string]*goldenDev
		speed bool
	}{
		{"expected_pci_link_speed" + expSel, d.Expected, true},
		{"expected_pci_link_width" + expSel, d.Expected, false},
		{"node_pci_cur_speed" + liveSel, d.Live, true},
		{"node_pci_cur_width" + liveSel, d.Live, false},
	} {
		rows, err := promInstant(token, q.expr)
		if err != nil {
			return nil, err
		}
		mergeGoldenRows(q.into, rows, q.speed, conflicts)
	}
	for bdf := range conflicts {
		d.Conflicts = append(d.Conflicts, bdf)
	}
	sort.Strings(d.Conflicts)
	return d, nil
}

// mergeGoldenRows folds prom samples into the per-BDF map. Golden rows are
// served by trino-exporter from several core-services clusters at once —
// duplicates with identical values collapse; disagreeing values are recorded
// as conflicts.
func mergeGoldenRows(into map[string]*goldenDev, rows []promResult, speed bool, conflicts map[string]bool) {
	for _, r := range rows {
		bdf := r.Metric["device"]
		if bdf == "" || len(r.Value) < 2 {
			continue
		}
		s, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		dev := into[bdf]
		if dev == nil {
			dev = &goldenDev{
				DeviceName: r.Metric["device_name"],
				VendorID:   r.Metric["vendor_id"],
				DeviceID:   r.Metric["device_id"],
			}
			into[bdf] = dev
		}
		if speed {
			if dev.HasSpeed && dev.Speed != v {
				conflicts[bdf] = true
			}
			dev.Speed, dev.HasSpeed = v, true
		} else {
			if dev.HasWidth && dev.Width != v {
				conflicts[bdf] = true
			}
			dev.Width, dev.HasWidth = v, true
		}
	}
}

// promInstant runs one instant query against the golden datasource through
// the Grafana proxy. 401/403 drops the cached token, mirroring the nodebot
// preflight, so a rotated token is re-resolved on the next run.
func promInstant(token, expr string) ([]promResult, error) {
	base := strings.TrimRight(envOr("GRAFANA_URL", "https://grafana.int.coreweave.com"), "/")
	ds := envOr("GPUINSPECT_GOLDEN_DS_UID", goldenDSUID)
	u := base + "/api/datasources/proxy/uid/" + url.PathEscape(ds) + "/api/v1/query?query=" + url.QueryEscape(expr)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		keychainDelete(keychainSvcGrafana)
		return nil, fmt.Errorf("Grafana token INVALID/EXPIRED (HTTP %d) — rotate the token-viewer item or export GRAFANA_API_TOKEN", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GloQL query: unexpected HTTP %d", resp.StatusCode)
	}
	var out promResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("GloQL response decode: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("GloQL query status %q: %s", out.Status, out.Error)
	}
	return out.Data.Result, nil
}
