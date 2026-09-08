package main

// Shared contracts for the BMN-first gpuinspect workflow. bmn.go, scan.go,
// gpuanalysis.go, ai.go, nodebot.go and orchestrate.go implement against
// these; main.go wires them together.

type bmnAlert struct {
	Name           string // e.g. NodePCILinkWidthUnexpected
	BDF            string // PCI address parsed from the alert message ("" if none)
	Message        string
	LastTransition string
}

type bmnInfo struct {
	Name, GMAC, Serial, DeviceSlot, SKU string
	State, Zone, Region, Fabric         string
	Online                              string // "true" / "false" / "-"
	Workflow, WorkflowStep              string // status.flcc.{workflow,workflowStep}
	ExpectedGPUs                        int    // allocatable nvidia.com/gpu; 0 = unknown
	Alerts                              []bmnAlert
	PCIAlertBDFs                        []string // dedup'd BDFs from NodePCILink* alerts
	BundleCurrent, BundleTarget         string   // node.coreweave.cloud/node-fwbundle.{current,target} labels
	BundleSpec                          string   // spec.firmware.nodeBundle
	DPUBundleCurrent                    string   // node.coreweave.cloud/dpu-fwbundle.current
	BundleFlags                         []string // human-readable anomalies (false-positive signals)
}

// gpuFindings is produced on-node by gpuanalysis.go, printed in the report,
// and serialized into the findings JSON handed to the AI layer.
type gpuFindings struct {
	Manufacturer string   `json:"manufacturer"`
	GPUModel     string   `json:"gpu_model"`
	CountSMI     int      `json:"count_nvidia_smi"`
	CountLspci   int      `json:"count_lspci"`
	Expected     int      `json:"count_expected"`
	ReportedGPUs int      `json:"count_allocatable"` // node-reported allocatable; shrinks with the fault
	MissingBDFs  []string `json:"missing_bdfs"`
	RevFF        []string `json:"rev_ff_devices"` // lspci "(rev ff)" = fell off the bus
	Inventory    []string `json:"inventory"`      // index, pci.bus_id, serial, uuid, name
}

// inspectResult is one BMN's outcome, consumed by the matrix renderer in
// orchestrate.go. NextSteps are ADVISORY ONLY — gpuinspect never executes
// cwctl or any state-changing command.
type inspectResult struct {
	BMN         string
	Info        *bmnInfo
	VerdictCode int // 0 healthy | 1 degraded | 2 present-for-RMA | 3 error
	// FalsePositive marks the PEX890xx benign class (idle-port x0 /
	// self-cleared transient) — on-node flag confirmed against the mgmt
	// alert feed (withdrawn when any non-PCI-link alert is present).
	FalsePositive bool
	// GoldenMismatch marks a golden-state error (goldenstate.go): the alert
	// compares against a golden row that mismatches the node's enumeration or
	// carries stale values — fixed on the golden side, never by drain/RMA.
	GoldenMismatch bool
	Issues        []string
	NextSteps     []string
	OutDir        string
	AISummary     string // first line of the AI analysis, for the matrix
	Err           error
	Output        string // buffered colored terminal section, flushed atomically
}
