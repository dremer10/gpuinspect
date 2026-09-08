package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Width-runbook triage (METAL runbook "Triaging NodePCILinkWidthUnexpected:
// mapping a PCIe bridge BDF to a NIC, GPU or NVMe drive", Confluence page
// 1616642163): the alert payload names a PCIe bridge (downstream port) BDF,
// not the end device. Map bridge -> child, confirm the link from the endpoint
// side, identify the device so a DCT reseat or RMA ticket can proceed.
// ---------------------------------------------------------------------------

var (
	childBDFRe = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	slotNumRe  = regexp.MustCompile(`Slot #(\d+)`)
	// Runbook grep patterns: step 1 on the bridge, step 4 on the endpoint.
	// Trailing colons keep LnkCap2:/LnkSta2: out, matching the runbook greps.
	bridgeLinkRe   = regexp.MustCompile(`LnkCap:|LnkSta:|Slot #`)
	endpointLinkRe = regexp.MustCompile(`LnkCap:|LnkSta:|LnkCtl2:`)
)

// grepLines returns the trimmed lines of s matching re — the tee'd equivalent
// of the runbook's `lspci ... | grep -E '...'` steps.
func grepLines(s string, re *regexp.Regexp) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if re.MatchString(l) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// nonEmptyLines returns the trimmed non-blank lines of s.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// childBuses returns the deduped secondary-bus numbers of the child BDFs
// (e.g. 0000:c1:00.0 -> c1), preserving order.
func childBuses(children []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, ch := range children {
		parts := strings.Split(ch, ":")
		if len(parts) != 3 || seen[parts[1]] {
			continue
		}
		seen[parts[1]] = true
		out = append(out, parts[1])
	}
	return out
}

// treeLinesFor returns the lspci -vt lines belonging to the given secondary
// buses — the runbook step 2 option A subtree for one bridge. A bus appears
// in the tree as `[c1]` (leaf) or `[c1-c4]` (range start).
func treeLinesFor(tree string, buses []string) []string {
	var out []string
	for _, line := range strings.Split(tree, "\n") {
		for _, b := range buses {
			if strings.Contains(line, "["+b+"]") || strings.Contains(line, "["+b+"-") {
				out = append(out, strings.TrimRight(line, " "))
				break
			}
		}
	}
	return out
}

// cheatSheet maps the degradation pattern to the width-runbook interpretation
// cheat-sheet row ("" when not degraded). Advice text only — never executed.
func cheatSheet(speedDeg, widthDeg, down bool) string {
	switch {
	case down:
		return "link down (x0) — no active lanes; if a real endpoint sits behind this port, treat as a width failure"
	case speedDeg && widthDeg:
		return "speed AND width below LnkCap — reseat the endpoint if GPU/NIC/NVMe, otherwise RMA"
	case widthDeg:
		return "width down, speed nominal — lane failure: reseat the endpoint first"
	case speedDeg:
		return "speed down, width nominal — signal integrity: reseat once; if it recurs, RMA"
	}
	return ""
}

// boxCmd / boxOut are echoCmd/echoOutput for runbook commands run inside the
// per-BDF box — the │ prefix keeps the box drawing intact.
func (t *triage) boxCmd(cmdline string) {
	t.log.p("%s│ $ %s%s", t.c.B, cmdline, t.c.X)
}

func (t *triage) boxOut(lines []string) {
	if len(lines) == 0 {
		t.log.p("%s│%s   %s<no output>%s", t.c.B, t.c.X, t.c.D, t.c.X)
		return
	}
	for _, l := range lines {
		t.log.p("%s│%s   %s", t.c.B, t.c.X, l)
	}
}

// pciTree returns `lspci -vt` (runbook step 2 option A), run once per triage
// and captured in full to the bundle; callers echo per-bridge subtrees.
func (t *triage) pciTree() string {
	if !t.pciTreeDone {
		t.pciTreeDone = true
		t.pciTreeCache = cmdOut("lspci", "-vt")
		if t.bundle != "" {
			_ = os.WriteFile(filepath.Join(t.bundle, "pcie_tree.txt"), []byte(t.pciTreeCache), 0o644)
		}
	}
	return t.pciTreeCache
}

func isBridge(bdf string) bool {
	return strings.HasPrefix(readSysfs("/sys/bus/pci/devices/"+bdf+"/class"), "0x0604")
}

// pciChildren returns the BDFs on the bridge's secondary bus (runbook step 2,
// option B: children appear as directory entries under the bridge's sysfs node).
func pciChildren(bdf string) []string {
	entries, _ := osReadDirNames("/sys/bus/pci/devices/" + bdf)
	var out []string
	for _, name := range entries {
		if childBDFRe.MatchString(name) {
			out = append(out, name)
		}
	}
	return out
}

// routeFor maps a device class to its remediation path: PEX switches follow
// the speed-runbook power-drain path, NVMe the drive-swap path, and GPU/NIC
// endpoints the width-runbook single-DCT-reseat path.
func routeFor(class string) string {
	if class == "gpu" || class == "nic" {
		return "reseat"
	}
	return class // switch | nvme | other
}

// inspectBridge walks a degraded bridge BDF (width runbook steps 2-4): find
// the child device, confirm the link from the endpoint side, flag an endpoint
// that advertises a lower ceiling than the bridge, and identify the part.
// Returns the remediation route derived from the child ("reseat"/"nvme"), or
// "" when the bridge's own class should decide.
func (t *triage) inspectBridge(r *devResult) string {
	c := t.c
	children := pciChildren(r.bdf)
	// Step 2 option B — sysfs one-liner: children on the secondary bus.
	t.boxCmd("ls /sys/bus/pci/devices/" + r.bdf + "/ | grep '^0000:'")
	t.boxOut(children)
	if len(children) == 0 {
		r.noChild = true
		if r.down {
			t.log.p("%s│%s %sNOTE    : no device behind this bridge — x0 on an unused PEX downstream%s", c.B, c.X, c.Y, c.X)
			t.log.p("%s│%s %s          port is its expected idle state. Known false-positive pattern%s", c.B, c.X, c.Y, c.X)
			t.log.p("%s│%s %s          (METAL-4742: kernel passes through the raw saturated width, 63,%s", c.B, c.X, c.Y, c.X)
			t.log.p("%s│%s %s          on idle ports). Verify against golden state / vendor topology%s", c.B, c.X, c.Y, c.X)
			t.log.p("%s│%s %s          doc BEFORE requesting any physical action.%s", c.B, c.X, c.Y, c.X)
		}
		return ""
	}

	// Step 2 option A — lspci -vt subtree (the runbook's most-readable
	// bridge -> child mapping); full tree lands in the bundle (pcie_tree.txt).
	buses := childBuses(children)
	if tree := t.pciTree(); tree != "" {
		t.boxCmd("lspci -vt   # subtree for " + r.bdf)
		t.boxOut(treeLinesFor(tree, buses))
	}
	// Step 2 option C — list everything on the secondary bus.
	for _, bus := range buses {
		t.boxCmd("lspci -s " + bus + ":")
		t.boxOut(nonEmptyLines(cmdOut("lspci", "-s", bus+":")))
	}

	route := ""
	for _, ch := range children {
		desc := strings.TrimSpace(cmdOut("lspci", "-s", ch))
		if desc == "" {
			desc = ch + " <lspci returned nothing>"
		}
		cls := classify(desc)
		t.log.p("%s│%s Child   : %s  %s[%s]%s", c.B, c.X, desc, c.D, cls, c.X)

		// Step 4 — confirm the link from the endpoint side.
		t.boxCmd("lspci -vvv -s " + ch + " | grep -E 'LnkCap:|LnkSta:|LnkCtl2:'")
		t.boxOut(grepLines(cmdOut("lspci", "-vvv", "-s", ch), endpointLinkRe))
		chPath := "/sys/bus/pci/devices/" + ch
		eMaxS := readSysfs(chPath + "/max_link_speed")
		eMaxW := readSysfs(chPath + "/max_link_width")
		eCurS := readSysfs(chPath + "/current_link_speed")
		eCurW := readSysfs(chPath + "/current_link_width")
		if eMaxS != "" && eCurS != "" {
			t.log.p("%s│%s           endpoint side: cap %s x%s / current %s x%s", c.B, c.X, eMaxS, eMaxW, eCurS, eMaxW2(eCurW))
			eMaxWi, _ := strconv.Atoi(eMaxW)
			bMaxWi, _ := strconv.Atoi(r.maxWidth)
			eCurWi, errECW := strconv.Atoi(eCurW)
			switch {
			case numLT(eMaxS, r.maxSpeed) || (eMaxWi > 0 && bMaxWi > 0 && eMaxWi < bMaxWi):
				r.childCapBelow = true
				t.log.p("%s│%s %sNOTE    : endpoint LnkCap is BELOW the bridge's — the endpoint itself is%s", c.B, c.X, c.Y, c.X)
				t.log.p("%s│%s %s          the ceiling. Wrong part or FW: verify against BOM. NO DCT action.%s", c.B, c.X, c.Y, c.X)
			case numLT(eCurS, eMaxS) || (errECW == nil && eMaxWi > 0 && eCurWi < eMaxWi):
				r.childLinkDeg = true
				t.log.p("%s│%s %sNOTE    : endpoint sees the degraded link too — degraded on BOTH sides:%s", c.B, c.X, c.Y, c.X)
				t.log.p("%s│%s %s          physical / signal-integrity issue (reseat path per cheat sheet).%s", c.B, c.X, c.Y, c.X)
			default:
				t.log.p("%s│%s %s          endpoint reports a full link from its side — bridge/endpoint%s", c.B, c.X, c.D, c.X)
				t.log.p("%s│%s %s          disagree; the link may have retrained since the alert fired.%s", c.B, c.X, c.D, c.X)
			}
		}

		// Step 3 — vendor serial / interface name for the ticket.
		t.identifyEndpoint(ch, cls)

		if r.childBDF == "" || routeRank(cls) > routeRank(r.childClass) {
			r.childBDF, r.childDesc, r.childClass = ch, descOnly(desc), cls
		}
	}

	switch r.childClass {
	case "gpu", "nic":
		route = "reseat"
	case "nvme":
		route = "nvme"
	}
	return route
}

func routeRank(class string) int {
	switch class {
	case "gpu":
		return 3
	case "nic":
		return 2
	case "nvme":
		return 1
	}
	return 0
}

// eMaxW2 guards against empty sysfs width reads in the display line.
func eMaxW2(w string) string {
	if w == "" {
		return "?"
	}
	return w
}

// identifyEndpoint logs the vendor serial / interface identity needed for a
// DCT or RMA ticket (width runbook step 3), best-effort per device class.
func (t *triage) identifyEndpoint(bdf, class string) {
	c := t.c
	switch class {
	case "gpu":
		if t.haveNvsmi {
			short := strings.ToLower(strings.TrimPrefix(bdf, "0000:"))
			t.boxCmd("nvidia-smi --query-gpu=serial,pci.bus_id,uuid --format=csv | grep -i " + short)
			for _, line := range strings.Split(cmdOut("nvidia-smi",
				"--query-gpu=serial,pci.bus_id,uuid", "--format=csv"), "\n") {
				if strings.Contains(strings.ToLower(line), short) {
					t.log.p("%s│%s GPU     : %s", c.B, c.X, strings.TrimSpace(line))
				}
			}
		}
	case "nic":
		t.boxCmd("ls /sys/bus/pci/devices/" + bdf + "/net/")
		ifaces, _ := osReadDirNames("/sys/bus/pci/devices/" + bdf + "/net")
		for _, ifc := range ifaces {
			t.log.p("%s│%s Iface   : %s", c.B, c.X, ifc)
			if haveCmd("ethtool") {
				t.boxCmd("ethtool -i " + ifc)
				for _, line := range strings.Split(cmdOut("ethtool", "-i", ifc), "\n") {
					l := strings.TrimSpace(line)
					if strings.HasPrefix(l, "firmware-version:") || strings.HasPrefix(l, "bus-info:") {
						t.log.p("%s│%s           %s", c.B, c.X, l)
					}
				}
			}
		}
		// Mellanox vendor PN / SN as printed on the card.
		if haveCmd("mstvpd") {
			t.boxCmd("mstvpd -d " + bdf + " | grep -E 'PN|SN'")
			for _, line := range strings.Split(cmdOut("mstvpd", "-d", bdf), "\n") {
				l := strings.TrimSpace(line)
				if strings.HasPrefix(l, "PN:") || strings.HasPrefix(l, "SN:") {
					t.log.p("%s│%s           %s", c.B, c.X, l)
				}
			}
		} else if haveCmd("mlxfwmanager") {
			t.boxCmd("mlxfwmanager --query -d " + bdf + " | grep -E 'PSID|FW Version|Base MAC|Part Number'")
			for _, line := range strings.Split(cmdOut("mlxfwmanager", "--query", "-d", bdf), "\n") {
				l := strings.TrimSpace(line)
				if strings.Contains(l, "PSID") || strings.Contains(l, "FW Version") ||
					strings.Contains(l, "Base MAC") || strings.Contains(l, "Part Number") {
					t.log.p("%s│%s           %s", c.B, c.X, l)
				}
			}
		}
	case "nvme":
		t.nvmeSlotMap(bdf)
	}
}
