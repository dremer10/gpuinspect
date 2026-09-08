# gpuinspect

BMN-first GPU/PCIe inspection and analysis tool for CoreWeave bare-metal nodes.
Give it a BareMetalNode name and it pulls the node's identity and firing alerts
from the mgmt cluster, pushes itself onto the node over Teleport, runs read-only
PCIe link triage plus a full GPU health suite, retrieves the evidence to your
laptop, and prints a verdict with copy-paste remediation advice.

**gpuinspect inspects and advises only.** It never executes `cwctl` or any
state-changing command. Mgmt-cluster access is `kubectl get` only, on-node
commands are read-only diagnostics, and every remediation is printed as advice
for the operator to run.

Runbooks it implements: FRO "NodePCILinkSpeedUnexpected" (Confluence 649756759)
and METAL "Triaging NodePCILinkWidthUnexpected: mapping a PCIe bridge BDF to a
NIC, GPU or NVMe drive" (Confluence 1616642163).

## Install

### Prerequisites

| Tool | Why |
|---|---|
| Go 1.23+ | build |
| `kubectl` with a mgmt-cluster context | BMN lookup (`tl <region>` sets it up) |
| `tsh` (Teleport), logged in | ssh/scp to the node as `acc@<gMAC>` |
| `op` (1Password CLI), signed in | API keys for the AI pass and nodebot (optional, see Credentials) |
| `golangci-lint` | `make lint` only |
| Docker | `make docker-build` only |

### Build

From this directory:

```sh
make build
```

This writes three binaries into `../../bin/` (the `bin/` folder that sits next
to the `projects/` folder this source lives in):

| Binary | Purpose |
|---|---|
| `gpuinspect` | native binary you run on your laptop |
| `gpuinspect-linux-amd64` | pushed to x86 nodes |
| `gpuinspect-linux-arm64` | pushed to aarch64 nodes (GH200/GB200) |

Always build with `make build` rather than `go build`. The laptop binary
locates the linux binaries *next to itself*, so all three must land in the same
`bin/` directory. A stray `go build ./...` here drops a `gpuinspect` binary into
this source directory; it is gitignored and harmless, just `rm` it.

`bin/` is gitignored. Binaries are build artifacts, never committed. Rebuild
after pulling changes.

Other targets:

```sh
make test           # go test ./...
make lint           # golangci-lint run (mirrors repo pre-commit CI)
make release        # adds darwin-amd64/arm64 builds
make docker-build   # same builds inside Docker (pins the Go toolchain)
make clean          # remove the built binaries
```

### Add the binary to your PATH

If you use the fleet-ops shell setup (`cwft`, or `setup/source` from
cw-fleet-tools), your workspace `bin/` is already on `$PATH` and nothing more is
needed. Verify with:

```sh
which gpuinspect
gpuinspect --version
```

Otherwise add the `bin/` directory once to your shell profile. From this
directory:

```sh
echo "export PATH=\"$(cd ../../bin && pwd):\$PATH\"" >> ~/.zshrc
source ~/.zshrc
```

Do not copy the `gpuinspect` binary elsewhere on its own. Remote mode needs the
linux binaries beside it. If you must relocate, move all three together, or
point at the linux binary explicitly with `--linux-bin <path>`.

## Invoke

Run from a laptop with a mgmt-cluster kubectl context active and a Teleport
login in place:

```sh
tl rno2                          # mgmt kubectl context (interactive login)
tsh status                       # confirm the Teleport session is valid
gpuinspect <BMN>
```

Modes:

```sh
gpuinspect                                        # fleet scan: BMNs firing PCI link alerts
gpuinspect <BMN> [<BMN>...]                       # inspect one or more BMNs (main path)
gpuinspect <BMN> --temps                          # quick live thermal readout, all GPUs
frop dissect -c <BMN> | gpuinspect <BMN>          # history + inspection, concurrently
gpuinspect --remote <gMAC> --bmn <BMN> [BDF...]   # direct remote by gMAC (legacy)
gpuinspect --help
```

You never pass PCI BDFs in BMN mode. They are parsed out of the alert messages
on the BMN object.

### What one inspection does

1. **BMN lookup.** `kubectl get bmn <BMN> -o json` for gMAC, SKU, serial,
   deviceslot, FLCC state and workflow, expected GPU count, firing alert
   conditions, and the fwbundle labels. A node sitting in the `fielddiag`
   workflow step is flagged up front: fieldiag owns the GPUs and every
   on-node `nvidia-smi` will hang.
2. **Bundle check.** `node-fwbundle.current` unset means the legacy alert
   path (a known false-positive class); `current != target` means a firmware
   update is in flight.
3. **On-node triage.** The matching linux binary is pushed over `tsh scp`
   (cached on the node at `/var/tmp/gpuinspect_cache_<sha256>`, so re-runs skip
   the transfer) and runs: PCIe link triage on each alert BDF (bridge to
   endpoint mapping, both-sides LnkCap/LnkSta, AER/DPC history, the full
   width-runbook command set echoed as `$ cmd` with output) plus the GPU suite
   (nvidia-smi inventory, ECC, remapped rows, topology, NVLink, missing-GPU and
   `(rev ff)` detection, XID lines from dmesg, IB port state, thermal slowdown
   bits).
4. **Golden-state check.** Reproduces the alert's own comparison via GloQL:
   the golden `expected_pci_link_*` row for the node's SKU and fwbundle against
   the live `node_pci_cur_*` values, device identity included. Catches alerts
   caused by enumeration shifts or stale golden tables that no drain or RMA
   will ever clear (verdict `GOLDEN-MIS`).
5. **Evidence retrieval** to `~/Downloads/tmp/<BMN>_<ts>/` plus a sibling
   `<BMN>_<ts>.tar.gz`: ANSI-free report log, collect bundle, `gpu_full.txt`,
   `verdict.json`, `analysis.md`. Attach the tar.gz to the HO ticket.
6. **AI analysis** of the findings, then **nodebot diagnose** with the findings
   as context. Both are on by default and both degrade to a one-line warning
   when credentials are missing. Piped frop context auto-skips nodebot, since
   frop dissect already is one.

Multiple BMNs run in parallel (default 4, `--parallel N`). Each node's report
prints atomically as it finishes, then a summary matrix (BMN, gMAC, SKU, state,
verdict, issues, next steps) prints and is written to
`~/Downloads/tmp/summary_<ts>.{txt,md}`. An ANSI-free markdown copy of the
matrix prints between `--- matrix (markdown — copy for evidence) ---` markers;
`--copy` also puts it on the clipboard. The `.md` file carries the full
per-node report as fenced sections, so one file holds the whole run.

### Exit codes and verdicts

| Exit | Matrix label | Meaning |
|---|---|---|
| 0 | `HEALTHY` | verified all-clear |
| 0 | `FALSE-POS` | PEX890xx false-positive class, safe to return to ready |
| 1 | `DEGRADED` / `GOLDEN-MIS` | advice issued, operator action needed |
| 2 | `RMA` | present for RMA |
| 3 | `ERROR` | tool or transport failure |

`FALSE-POS` is the runbook's PEX890xx sweep artifact: every PCIe finding is
either an idle x0 port with nothing behind it (METAL-4742) or an alert BDF that
re-checks healthy, AND the GPU suite is clean, no XID, no fatal AER, no
retrain noise or DPC on the recovered port, every IB port ACTIVE, no thermal
slowdown, no non-PCI-link alert on the BMN, and the alert is younger than
`GPUINSPECT_FP_MAX_ALERT_AGE_HOURS` (default 72). The advice printed is:

```sh
cwctl flcc node -w return-to-ready -m "sending to ready" <bmn>
```

Multi-BMN runs roll every `FALSE-POS` node into a fleet advisory with one
return-to-ready line per node plus a single-paste `for n in ...; do ...; done`
bulk line. Do not bulk-clear blindly: a BDF that re-alerts across reboots or
appears with real PCIe/GPU errors is a genuine link fault.

## Use

### Recipes

**Full investigation of a node you don't understand yet.** The default move:
history, PCIe triage, GPU suite, AI, one nodebot pass total. frop and the
on-node inspection run concurrently, so wall-clock is about frop's own runtime.

```sh
frop dissect -c <BMN> | gpuinspect <BMN>
```

**Quick classification without history.**

```sh
gpuinspect <BMN>
```

**What are the temps on every GPU right now.** About a minute. Skips PCIe
triage, AI and nodebot. The worst value seen in the sampling window gates the
verdict, not the idle snapshot. Idle temps clearing does not mean the fault
cleared; a GPU hot *at idle* is a strong signal.

```sh
gpuinspect <BMN> --temps
gpuinspect <BMN1> <BMN2> <BMN3> --temps --parallel 8     # thermal sweep, one matrix
frop dissect -c <BMN> | gpuinspect <BMN> --temps          # thermals + history in one file
```

**Re-check after a DCT action.** Single BMN. Tells the verdict logic what was
just tried so the advice escalates correctly.

```sh
gpuinspect <BMN> --post-drain      # after the AC power drain
gpuinspect <BMN> --post-swap       # after an NVMe drive swap
gpuinspect <BMN> --post-reseat     # after a GPU/NIC endpoint reseat
```

**Deep GPU evaluation.** Extended inventory, retired pages, per-GPU PCIe/AER
detail, dmon/pmon windows. Results land in `eval/` inside the bundle. The
stress tests are the one carve-out from the read-only rule and require an
explicit `--yes-intrusive`.

```sh
gpuinspect <BMN> --eval
gpuinspect <BMN> --eval-dcgm --dcgm-level 3 --yes-intrusive
gpuinspect <BMN> --eval-nvbandwidth --yes-intrusive
gpuinspect <BMN> --eval-bug-report                       # slow but safe
```

**Fleet fallout triage.** Scan, eyeball the table, then inspect every hit.
`linkfail` is a sibling project in the same workspace (`make build` there puts
it in the same `bin/`).

```sh
tl rno2
linkfail
gpuinspect $(linkfail | awk '{print $1}' | grep -E '^ss' | sort -u) --parallel 8
```

Or use the built-in scan, which lists BMNs whose latest alert is a PCI link
alert (`--ignore-rma` hides nodes already in an RMA state):

```sh
gpuinspect
gpuinspect --ignore-rma
```

**Non-GPU PCIe chase.** Auto-discovery is GPU-only by default; this widens it
to the PEX/NIC/NVMe sweep. Alert BDFs are always triaged regardless.

```sh
gpuinspect <BMN> --all-pci
```

**No credentials handy.**

```sh
gpuinspect <BMN> --no-ai --no-nodebot --no-golden
```

### Flags

```text
--context <ctx>      kubectl context for the mgmt cluster (default: current)
--out-dir <dir>      evidence dir (default ~/Downloads/tmp; /tmp/gpuinspect if no home)
--parallel <n>       concurrent BMN inspections (default 4)
--ignore-rma         fleet scan: hide nodes whose FLCC state is RMA-related
--no-ai              skip the AI analysis
--no-golden          skip the golden-state check
--no-nodebot         skip nodebot diagnose
--nodebot            force nodebot even when piped frop context would auto-skip it
--no-color           disable ANSI colors
--copy               also copy the markdown matrix to the clipboard (pbcopy/xclip/xsel)
--all-pci            widen auto-discovery beyond GPU devices (PEX/NIC/NVMe sweep)
--temps              live thermal readout for all GPUs; quick mode
--eval               full GPU evaluation (safe)
--eval-dcgm          + dcgmi diag (implies --eval; requires --yes-intrusive)
--dcgm-level <n>     DCGM diag level 1-4 (default 2)
--eval-nvbandwidth   + nvbandwidth (implies --eval; requires --yes-intrusive)
--eval-bug-report    + nvidia-bug-report.sh (implies --eval; slow but safe)
--yes-intrusive      confirm GPU-stressing diagnostics
--post-drain / --post-swap / --post-reseat / --triaged
                     re-check runs after a DCT action (single BMN)
--remote <gMAC>      direct remote mode; --bmn required
--bmn <BMN>          node BMN (on-node / direct-remote modes)
--user <user>        remote ssh user (default acc)
--linux-bin <path>   linux binary to push (default: auto, next to this binary)
--version            print version
-h, --help           usage
```

`--temps` cannot be combined with `--eval`.

### Piping frop dissect

```sh
frop dissect -c <BMN> | gpuinspect <BMN>
```

gpuinspect's on-node view is a point-in-time snapshot. `frop dissect` sees the
history: alert persistence, prior DO/HO tickets, failed power drains, an RMA
already in flight. When stdin is a pipe, gpuinspect reads it in the background
and only waits for it at verdict time (progress shows
`📥 waiting for piped context`). Hard signals in that text veto the `FALSE-POS`
verdict so a node is never advised back to ready against known history. The
full text goes to the AI pass and into `summary_<ts>.md`. If frop never
finishes, the run continues without it after
`GPUINSPECT_CONTEXT_TIMEOUT_MINUTES` (default 30).

### Credentials

Everything except the AI pass, nodebot, and the golden-state check works with
no credentials at all. Those three degrade to a one-line warning when a key is
missing; `--no-ai --no-nodebot --no-golden` silences them.

- **Anthropic API key** (AI pass): `ANTHROPIC_API_KEY` if set, otherwise
  `op read` of the shared frops vault item. Run `op signin` first.
- **Grafana token** (golden-state check, nodebot preflight): `GRAFANA_API_TOKEN`
  if set, otherwise `op read` of frop's token-viewer item.
  `GPUINSPECT_GRAFANA_OP_REF=op://<vault>/<item>/<field>` points the lookup at a
  personal item. `GRAFANA_URL` retargets (default `grafana.int.coreweave.com`).

1Password is consulted at most once per run and the value is cached in the
macOS keychain for one hour, so back-to-back runs do not re-prompt. Keys travel
via process environment or keychain only, never argv or files.

nodebot is discovered via `GPUINSPECT_NODEBOT`, then `$PATH`, then a
gpuinspect-managed venv, then frop's venv. If none exist it bootstraps one under
`~/Library/Caches/gpuinspect/` (one-time, a minute or two).

### Per-user config

`~/.config/gpuinspect/config` (override with `GPUINSPECT_CONFIG`) holds
`KEY=VALUE` lines applied as environment *defaults* at startup. Real env vars
always win. Store `op://` references or plain settings only, never secrets.

```text
GPUINSPECT_GRAFANA_OP_REF=op://Employee/<my-item>/password
GPUINSPECT_TEMP_SECONDS=20
```

### Environment variables

```text
TRIAGE_SSH / TRIAGE_SCP               transport override (default tsh ssh / tsh scp)
GPUINSPECT_SCAN_SELECTOR              label selector for fleet-scan mode
GPUINSPECT_NODEBOT                    path to the nodebot executable
GPUINSPECT_NODEBOT_DIR                nodebot source dir for the venv bootstrap
GPUINSPECT_SKIP_GRAFANA_PREFLIGHT=1   skip the Grafana token preflight
GPUINSPECT_GOLDEN_DS_UID              Grafana datasource uid for the golden-state check
GPUINSPECT_SHOW_MAX_LINES             per-command report line cap (default 200)
GPUINSPECT_TEMP_SECONDS               --temps sampling window in seconds (default 10)
GPUINSPECT_EXPECTED_GPUS              --temps: fault if fewer GPUs are visible
GPUINSPECT_DMON_SECONDS / GPUINSPECT_PMON_SECONDS
                                      eval sampling windows (default 20/10)
GPUINSPECT_DCGM_TIMEOUT / GPUINSPECT_NVBW_TIMEOUT / GPUINSPECT_BUGREPORT_TIMEOUT
                                      eval tool timeouts in seconds (600/300/3600)
GPUINSPECT_FP_MAX_ALERT_AGE_HOURS     self-cleared false-positive age gate (default 72)
GPUINSPECT_CONTEXT_TIMEOUT_MINUTES    max wait for piped frop context (default 30)
NVLINK_EXPECTED                       healthy NVLink line rate (default 26.562 GB/s)
ANTHROPIC_MODEL                       AI model override (default claude-opus-4-8)
MODEL                                 nodebot model override
NO_COLOR / FORCE_COLOR                color control
```

## Troubleshooting

- **`gpuinspect: command not found`** after `make build`: `bin/` is not on your
  `$PATH`. See "Add the binary to your PATH".
- **"no linux/amd64 binary found to push"** (or `linux/arm64`): the laptop
  binary was moved without its `gpuinspect-linux-*` siblings, or only one arch
  was built. Rebuild with `make build` or pass `--linux-bin`.
- **BMN lookup fails**: no mgmt kubectl context. Run `tl <region>` first, or
  pass `--context`.
- **ssh/scp to the node fails**: Teleport session expired. `tsh login`, then
  retry. `TRIAGE_SSH`/`TRIAGE_SCP` override the transport if you are not on
  Teleport.
- **The run hangs at the on-node phase**: check the identity block for a
  `fielddiag` workflow warning. fieldiag owns the GPUs and `nvidia-smi` blocks
  in the driver until it finishes. Wait, or skip the node.
- **"token-viewer item ... needs rotation"**: the shared Grafana token expired.
  Ask in #fro-internal-help, set a personal `GRAFANA_API_TOKEN`, or run with
  `--no-golden --no-nodebot`.
- **Node was `FALSE-POS` yesterday and alerts again today**: that is the
  runbook's recurrence caveat. A BDF that re-alerts across reboots is a
  genuine flapping link. Follow the reseat path, do not return-to-ready again.
