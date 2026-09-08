package main

// Fleet scan mode (gpuinspect with no args) — a direct port of linkfail:
// lists baremetal nodes whose conditions include a firing
// NodePCILinkSpeedUnexpected / NodePCILinkWidthUnexpected (or legacy
// NodePCIWidth / NodePCISpeed) alert, printed as a colored table. One row
// per matching condition, so a node firing both alerts appears twice.
//
// Read-only: the only external command is `kubectl get`.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// kubectl JSON shapes (private to scan mode; bmn.go owns bmnInfo)
// ---------------------------------------------------------------------------

type scanCondition struct {
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

type scanBMN struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		DeviceSlot string `json:"deviceSlot"`
		Serial     string `json:"serial"`
		Sku        struct {
			CwSku string `json:"cwSku"`
		} `json:"sku"`
		Health struct {
			Online *bool `json:"online"`
		} `json:"health"`
		Flcc struct {
			State string `json:"state"`
		} `json:"flcc"`
		ReportedNodeInfo struct {
			Status struct {
				Conditions []scanCondition `json:"conditions"`
			} `json:"status"`
		} `json:"reportedNodeInfo"`
	} `json:"status"`
}

type scanNodeList struct {
	Items []scanBMN `json:"items"`
}

type scanRow struct {
	Name, Error, DeviceSlot, Zone, Sku, Online, State, Serial, LastTransition, PCIAddr string
}

const scanLabelZone = "ds.coreweave.com/physical-topology.zone"

var (
	scanErrorRe   = regexp.MustCompile(`NodePCILink(Speed|Width)Unexpected|NodePCIWidth|NodePCISpeed`)
	scanPCIAddrRe = regexp.MustCompile(`(?i)at ([0-9a-fx:.]+) is`)
	// "rma" as a whole dash-separated token: matches "rma", "prepare-for-rma",
	// "rma-hold" — but not incidental substrings like "format".
	scanRMAStateRe = regexp.MustCompile(`(?i)(^|-)rma($|-)`)
)

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func runScan(o *options) int {
	nodes, code := scanFetchNodes(os.Getenv("GPUINSPECT_SCAN_SELECTOR"), o.kubeContext)
	if code != 0 {
		return code
	}

	rows := scanBuildRows(nodes)

	var hiddenRMA int
	if o.ignoreRMA {
		rows, hiddenRMA = scanFilterRMA(rows)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Error != rows[j].Error {
			return rows[i].Error < rows[j].Error
		}
		return rows[i].Name < rows[j].Name
	})

	useColor := !o.noColor && os.Getenv("NO_COLOR") == "" && isTTY(os.Stdout)

	if len(rows) == 0 {
		if hiddenRMA > 0 {
			fmt.Printf("no nodes with PCI link alerts found (%d in-RMA node(s) hidden by --ignore-rma)\n", hiddenRMA)
		} else {
			fmt.Println("no nodes with PCI link alerts found")
		}
		return 0
	}

	scanPrintTable(rows, useColor)

	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Name] = true
	}
	footer := fmt.Sprintf("\n%d node(s) with PCI link alerts — inspect with: gpuinspect <BMN> [<BMN>...]", len(seen))
	if hiddenRMA > 0 {
		footer += fmt.Sprintf(" (%d in-RMA node(s) hidden by --ignore-rma)", hiddenRMA)
	}
	if useColor {
		footer = scanAnsiDim + footer + scanAnsiReset
	}
	fmt.Println(footer)
	return 0
}

// ---------------------------------------------------------------------------
// kubectl fetch
// ---------------------------------------------------------------------------

// scanFetchNodes runs the (read-only) kubectl get and returns the node list.
// On failure it prints to stderr and returns a non-zero exit code.
func scanFetchNodes(selector, kubeContext string) ([]scanBMN, int) {
	if !haveCmd("kubectl") {
		fmt.Fprintln(os.Stderr, "gpuinspect: kubectl not found in PATH")
		return nil, 3
	}
	args := []string{"get", "baremetalnodes.flcc.coreweave.com", "-o", "json"}
	if selector != "" {
		args = append(args, "-l", selector)
	}
	if kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	cmd := exec.Command("kubectl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		errText := stderr.String()
		fmt.Fprint(os.Stderr, errText)
		if strings.Contains(errText, "doesn't have a resource type") {
			fmt.Fprintln(os.Stderr, "gpuinspect: current kubectl context has no BareMetalNode CRD — switch to a *-mgmt cluster (tl <region>)")
			return nil, 3
		}
		fmt.Fprintf(os.Stderr, "gpuinspect: kubectl get baremetalnodes failed: %v\n", err)
		return nil, 3
	}
	var list scanNodeList
	if err := json.Unmarshal(out, &list); err != nil {
		fmt.Fprintf(os.Stderr, "gpuinspect: parsing kubectl output: %v\n", err)
		return nil, 3
	}
	return list.Items, 0
}

// ---------------------------------------------------------------------------
// Row building
// ---------------------------------------------------------------------------

// scanBuildRows emits one row per currently-true "Alert triggered" condition
// whose reason matches a PCI link alert (current or legacy naming). Nodes
// often carry several near-identical conditions for the same alert, so rows
// are deduplicated on (node, error, PCI addr), keeping the latest transition.
func scanBuildRows(nodes []scanBMN) []scanRow {
	var rows []scanRow
	seen := map[string]int{} // dedup key -> index into rows
	for _, n := range nodes {
		online := "-"
		if n.Status.Health.Online != nil {
			online = fmt.Sprintf("%t", *n.Status.Health.Online)
		}
		for _, c := range n.Status.ReportedNodeInfo.Status.Conditions {
			if c.Status != "True" || !strings.HasPrefix(c.Reason, "Alert triggered") {
				continue
			}
			errName := scanErrorRe.FindString(c.Reason)
			if errName == "" {
				continue
			}
			pciAddr := "-"
			if m := scanPCIAddrRe.FindStringSubmatch(c.Message); m != nil {
				pciAddr = m[1]
			}
			key := n.Metadata.Name + "|" + errName + "|" + pciAddr
			if i, ok := seen[key]; ok {
				if c.LastTransitionTime > rows[i].LastTransition {
					rows[i].LastTransition = scanOrDash(c.LastTransitionTime)
				}
				continue
			}
			seen[key] = len(rows)
			rows = append(rows, scanRow{
				Name:           n.Metadata.Name,
				Error:          errName,
				DeviceSlot:     scanOrDash(n.Status.DeviceSlot),
				Zone:           scanOrDash(n.Metadata.Labels[scanLabelZone]),
				Sku:            scanOrDash(n.Status.Sku.CwSku),
				Online:         online,
				State:          scanOrDash(n.Status.Flcc.State),
				Serial:         scanOrDash(n.Status.Serial),
				LastTransition: scanOrDash(c.LastTransitionTime),
				PCIAddr:        pciAddr,
			})
		}
	}
	return rows
}

// scanFilterRMA drops rows whose FLCC state is RMA-related (--ignore-rma),
// returning the kept rows and the count of distinct hidden nodes.
func scanFilterRMA(rows []scanRow) ([]scanRow, int) {
	kept := rows[:0]
	hidden := map[string]bool{}
	for _, r := range rows {
		if scanRMAStateRe.MatchString(r.State) {
			hidden[r.Name] = true
			continue
		}
		kept = append(kept, r)
	}
	return kept, len(hidden)
}

func scanOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---------------------------------------------------------------------------
// Table rendering (linkfail's width computation / paint, scan-prefixed ANSI)
// ---------------------------------------------------------------------------

const (
	scanAnsiReset   = "\033[0m"
	scanAnsiBold    = "\033[1m"
	scanAnsiDim     = "\033[2m"
	scanAnsiGreen   = "\033[32m"
	scanAnsiYellow  = "\033[33m"
	scanAnsiRed     = "\033[31m"
	scanAnsiCyan    = "\033[36m"
	scanAnsiMagenta = "\033[35m"
	scanAnsiBlue    = "\033[34m"
	scanAnsiGrey    = "\033[90m"
)

func scanPrintTable(rows []scanRow, useColor bool) {
	headers := []string{"NAME", "ERROR", "DEVICESLOT", "ZONE", "SKU", "ONLINE", "STATE", "SERIAL", "LAST-TRANSITION", "PCI-ADDR"}
	cells := func(r scanRow) []string {
		return []string{r.Name, r.Error, r.DeviceSlot, r.Zone, r.Sku, r.Online, r.State, r.Serial, r.LastTransition, r.PCIAddr}
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h) + 2
	}
	for _, r := range rows {
		for i, v := range cells(r) {
			if len(v)+2 > widths[i] {
				widths[i] = len(v) + 2
			}
		}
	}

	paint := func(color, s string, width int) string {
		padded := fmt.Sprintf("%-*s", width, s)
		if !useColor || color == "" {
			return padded
		}
		return color + padded + scanAnsiReset
	}

	for i, h := range headers {
		fmt.Print(paint(scanAnsiBold+scanAnsiCyan, h, widths[i]))
	}
	fmt.Println()

	for _, r := range rows {
		errColor := scanAnsiMagenta
		if strings.Contains(r.Error, "Speed") {
			errColor = scanAnsiRed
		}
		onlineColor := scanAnsiRed
		if r.Online == "true" {
			onlineColor = scanAnsiGreen
		}
		stateColor := scanAnsiRed
		switch r.State {
		case "triage":
			stateColor = scanAnsiYellow
		case "production":
			stateColor = scanAnsiGreen
		}
		rowColors := []string{scanAnsiGreen, errColor, "", scanAnsiYellow, scanAnsiBlue, onlineColor, stateColor, scanAnsiGrey, scanAnsiMagenta, scanAnsiRed}
		for i, v := range cells(r) {
			fmt.Print(paint(rowColors[i], v, widths[i]))
		}
		fmt.Println()
	}
}
