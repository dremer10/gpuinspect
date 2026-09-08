package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// BareMetalNode fetch + parse (read-only kubectl; mgmt-cluster context)
// ---------------------------------------------------------------------------

const (
	bmnLabelZone          = "ds.coreweave.com/physical-topology.zone"
	bmnLabelFabric        = "ib.coreweave.cloud/fabric"
	bmnLabelBundleCurrent = "node.coreweave.cloud/node-fwbundle.current"
	bmnLabelBundleTarget  = "node.coreweave.cloud/node-fwbundle.target"
	bmnLabelDPUBundle     = "node.coreweave.cloud/dpu-fwbundle.current"

	bmnAlertPrefix = "Alert triggered "
)

var (
	// PCI address embedded in alert messages, e.g.
	// "PCI link width for ... at 0000:5c:00.0 is unexpected."
	bmnAlertBDFRe = regexp.MustCompile(`(?i)at ([0-9a-fx:.]+) is`)
	// Alert names that map to the PCI link triage workflow. The bare
	// NodePCIWidth/NodePCISpeed forms are the legacy hardcoded alerts.
	bmnPCIAlertRe = regexp.MustCompile(`NodePCILink(Speed|Width)Unexpected|NodePCIWidth|NodePCISpeed`)
	// Zone with a trailing zone letter, e.g. RNO2A -> region RNO2.
	bmnZoneLetterRe = regexp.MustCompile(`^(.+[0-9])[A-Za-z]$`)
)

// bmnDoc mirrors just the slice of the flcc.coreweave.com BareMetalNode CRD
// that gpuinspect reads (same shape as linkfail's bareMetalNode).
type bmnDoc struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Firmware struct {
			NodeBundle string `json:"nodeBundle"`
		} `json:"firmware"`
	} `json:"spec"`
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
			State        string `json:"state"`
			Workflow     string `json:"workflow"`
			WorkflowStep string `json:"workflowStep"`
		} `json:"flcc"`
		ReportedNodeInfo struct {
			NodeName string `json:"nodeName"`
			Status   struct {
				Allocatable map[string]string `json:"allocatable"`
				Conditions  []bmnCondition    `json:"conditions"`
			} `json:"status"`
		} `json:"reportedNodeInfo"`
	} `json:"status"`
}

type bmnCondition struct {
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

func fetchBMN(name, kubeContext string) (*bmnInfo, error) {
	if !haveCmd("kubectl") {
		return nil, fmt.Errorf("kubectl not found in PATH")
	}
	args := []string{"get", "baremetalnodes.flcc.coreweave.com", name, "-o", "json"}
	if kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		switch {
		case strings.Contains(msg, "the server doesn't have a resource type") ||
			strings.Contains(msg, "error: the server doesn't have"):
			return nil, fmt.Errorf("current kubectl context has no BareMetalNode CRD — switch to a *-mgmt cluster (e.g. run: tl <region>), or pass --context")
		case strings.Contains(msg, "NotFound"):
			return nil, fmt.Errorf("BMN %q not found in this mgmt cluster", name)
		default:
			return nil, fmt.Errorf("kubectl get bmn %s: %v: %s", name, err, strings.TrimSpace(msg))
		}
	}
	return parseBMN(stdout.Bytes())
}

func parseBMN(raw []byte) (*bmnInfo, error) {
	var doc bmnDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing BMN JSON: %w", err)
	}
	labels := doc.Metadata.Labels
	zone := labels[bmnLabelZone]

	online := "-"
	if doc.Status.Health.Online != nil {
		online = fmt.Sprintf("%t", *doc.Status.Health.Online)
	}
	expected := 0
	if v, ok := doc.Status.ReportedNodeInfo.Status.Allocatable["nvidia.com/gpu"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			expected = n
		}
	}

	info := &bmnInfo{
		Name:             doc.Metadata.Name,
		GMAC:             doc.Status.ReportedNodeInfo.NodeName,
		Serial:           doc.Status.Serial,
		DeviceSlot:       doc.Status.DeviceSlot,
		SKU:              doc.Status.Sku.CwSku,
		State:            doc.Status.Flcc.State,
		Workflow:         doc.Status.Flcc.Workflow,
		WorkflowStep:     doc.Status.Flcc.WorkflowStep,
		Zone:             zone,
		Region:           bmnRegionFromZone(zone),
		Fabric:           labels[bmnLabelFabric],
		Online:           online,
		ExpectedGPUs:     expected,
		BundleCurrent:    labels[bmnLabelBundleCurrent],
		BundleTarget:     labels[bmnLabelBundleTarget],
		DPUBundleCurrent: labels[bmnLabelDPUBundle],
		BundleSpec:       doc.Spec.Firmware.NodeBundle,
	}

	// Nodes often carry several near-identical conditions for the same alert
	// (one per firing instance); dedup on name+BDF keeping the latest
	// transition so the identity block and the AI prompt stay readable.
	alertIdx := map[string]int{}
	for _, c := range doc.Status.ReportedNodeInfo.Status.Conditions {
		if c.Status != "True" || !strings.HasPrefix(c.Reason, bmnAlertPrefix) {
			continue
		}
		a := bmnAlert{
			Name:           strings.TrimSpace(strings.TrimPrefix(c.Reason, bmnAlertPrefix)),
			Message:        c.Message,
			LastTransition: c.LastTransitionTime,
		}
		if m := bmnAlertBDFRe.FindStringSubmatch(c.Message); m != nil {
			a.BDF = bmnNormBDF(m[1])
		}
		key := a.Name + "|" + a.BDF
		if i, ok := alertIdx[key]; ok {
			if a.LastTransition > info.Alerts[i].LastTransition {
				info.Alerts[i].LastTransition = a.LastTransition
				info.Alerts[i].Message = a.Message
			}
			continue
		}
		alertIdx[key] = len(info.Alerts)
		info.Alerts = append(info.Alerts, a)
	}
	sort.Slice(info.Alerts, func(i, j int) bool {
		return info.Alerts[i].LastTransition < info.Alerts[j].LastTransition
	})

	seen := map[string]bool{}
	for _, a := range info.Alerts {
		if a.BDF == "" || !bmnPCIAlertRe.MatchString(a.Name) || seen[a.BDF] {
			continue
		}
		seen[a.BDF] = true
		info.PCIAlertBDFs = append(info.PCIAlertBDFs, a.BDF)
	}

	bundleCheck(info)
	return info, nil
}

// bmnRegionFromZone trims the trailing zone letter (RNO2A -> RNO2); zones
// without one yield "".
func bmnRegionFromZone(zone string) string {
	if m := bmnZoneLetterRe.FindStringSubmatch(zone); m != nil {
		return m[1]
	}
	return ""
}

// bmnNormBDF lowercases a PCI address from an alert message and expands the
// lspci short form (bb:dd.f) to the domain-qualified form used everywhere
// else in gpuinspect.
func bmnNormBDF(s string) string {
	s = strings.ToLower(s)
	if bdfRe.MatchString(s) && len(s) == len("bb:dd.f") {
		s = "0000:" + s
	}
	return s
}

// fieldDiagActive reports whether the node is in a fielddiag workflow step
// (fielddiag, fielddiag-reboot, ...). While NVIDIA fieldiag owns the GPUs,
// every nvidia-smi call blocks in the driver — the on-node inspection would
// hang until the diag completes, not fail.
func (i *bmnInfo) fieldDiagActive() bool {
	return strings.HasPrefix(strings.ToLower(i.WorkflowStep), "fielddiag")
}

// fieldDiagWarning is the human-readable version of fieldDiagActive, shown
// in the identity block, the live phase line and the issues list ("" when
// the node is not in fielddiag).
func (i *bmnInfo) fieldDiagWarning() string {
	if !i.fieldDiagActive() {
		return ""
	}
	return fmt.Sprintf("node is in the %q workflow step — fieldiag owns the GPUs, so nvidia-smi calls WILL BLOCK and the on-node inspection is expected to hang until the diag finishes; re-run after the fielddiag workflow completes", i.WorkflowStep)
}

// ---------------------------------------------------------------------------
// Bundle / golden-state sanity check (known false-positive class)
// ---------------------------------------------------------------------------

func bundleCheck(info *bmnInfo) {
	if info.BundleCurrent == "" {
		info.BundleFlags = append(info.BundleFlags,
			"node-fwbundle.current is UNSET — node is on the legacy/fallback (non-golden-state) alert path; NodePCIWidth-class alerts on this node are likely FALSE POSITIVES (fwbundle/golden-state mismatch)")
	}
	if info.BundleCurrent != "" && info.BundleTarget != "" && info.BundleCurrent != info.BundleTarget {
		info.BundleFlags = append(info.BundleFlags,
			fmt.Sprintf("fwbundle mid-update: current=%s target=%s — golden-state expectations may not match this node yet",
				info.BundleCurrent, info.BundleTarget))
	}
	for _, a := range info.Alerts {
		if a.Name == "NodePCIWidth" || a.Name == "NodePCISpeed" {
			info.BundleFlags = append(info.BundleFlags,
				"legacy NodePCIWidth/Speed alert firing — only legacy nodes should use this; check fwbundle/golden state first (known false-positive class)")
			break
		}
	}
}

func printBundleSection(log *logger, c palette, info *bmnInfo) {
	log.p("%sBundle check (k describe bmn | grep -i bundle equivalent)%s", c.B, c.X)
	val := func(s string) string {
		if s == "" {
			return c.Y + "<unset>" + c.X
		}
		return s
	}
	curColor := ""
	if info.BundleCurrent != "" && info.BundleTarget != "" {
		if info.BundleCurrent == info.BundleTarget {
			curColor = c.G
		} else {
			curColor = c.R
		}
	}
	log.p("  %snode-fwbundle.current    :%s %s%s%s", c.D, c.X, curColor, val(info.BundleCurrent), c.X)
	log.p("  %snode-fwbundle.target     :%s %s%s%s", c.D, c.X, curColor, val(info.BundleTarget), c.X)
	log.p("  %sspec.firmware.nodeBundle :%s %s", c.D, c.X, val(info.BundleSpec))
	log.p("  %sdpu-fwbundle.current     :%s %s", c.D, c.X, val(info.DPUBundleCurrent))
	for _, f := range info.BundleFlags {
		log.p("  %sFLAG:%s %s", c.Y, c.X, f)
	}
}
