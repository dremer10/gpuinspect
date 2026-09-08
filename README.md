# gpuinspect

BMN-first GPU/PCIe inspection and analysis tool (evolution of gpu-node-helper).
Follows the FRO "NodePCILinkSpeedUnexpected" runbook (Confluence 649756759) and
the METAL "Triaging NodePCILinkWidthUnexpected: mapping a PCIe bridge BDF to a
NIC, GPU or NVMe drive" runbook (Confluence 1616642163).

**gpuinspect inspects and advises only.** It never executes `cwctl` or any
state-changing command — mgmt-cluster access is `kubectl get` only, on-node
commands are read-only diagnostics, and every remediation is emitted as
copy-paste advice for the operator.

## Build

Prerequisites: Go 1.23+ (`golangci-lint` for `make lint`; Docker only for
`make docker-build`). From this directory (`team/amerten/projects/gpuinspect`):

    make build          # three binaries into team/amerten/bin/:
                        #   gpuinspect              native binary (on $PATH)
                        #   gpuinspect-linux-amd64  pushed to x86 nodes
                        #   gpuinspect-linux-arm64  pushed to aarch64 nodes (GH200/GB200)
    make test           # go test ./...
    make lint           # golangci-lint run (mirrors repo pre-commit CI)
    make release        # adds linux-arm64 + darwin-amd64/arm64
    make docker-build   # same builds inside Docker (pins the Go toolchain)

`team/amerten/bin/` is on your `$PATH` and is **gitignored** — binaries are
build artifacts, never committed; rebuild after pulling changes. Always use
`make build` (it produces both outputs; remote mode finds the linux binary
next to the running one). A stray `go build ./...` here drops a `gpuinspect`
binary in this source directory — harmless and gitignored, just `rm` it.

## Usage

    gpuinspect                        # fleet scan: nodes firing PCI link alerts
    gpuinspect <BMN> [<BMN>...]       # inspect BMNs (everything derived from the BMN)
    gpuinspect <BMN> --temps          # quick live thermal readout, all GPUs
    frop dissect -c <BMN> | gpuinspect <BMN>          # history + inspection, concurrently
    gpuinspect --remote <gMAC> --bmn <BMN> [BDF...]   # direct remote (legacy)

Run from a laptop with a **mgmt-cluster** kubectl context active (`tl <region>`).
For each BMN the tool:

1. `kubectl get bmn <BMN> -o json` — pulls gMAC, SKU, serial, deviceslot,
   state, expected GPU count, the firing alert conditions (PCI BDFs are parsed
   straight out of the alert messages — you don't pass BDFs), and the
   fwbundle labels.
2. **Bundle check** (the `k describe bmn | grep -i bundle` fields):
   `node-fwbundle.current` unset → legacy/fallback alert path (known
   false-positive class); `current != target` → mid-update, golden-state rows
   may not match.
3. Pushes the linux binary over `tsh ssh acc@<gMAC>` — the node's architecture
   is probed first (`>>> Node arch` line) so aarch64 nodes (GH200/GB200) get
   the arm64 binary — and runs on-node:
   PCIe link triage on the alert BDFs (bridge→endpoint mapping, AER/DPC
   history) plus the full **GPU analysis suite** (nvidia-smi inventory / ECC /
   remapped rows / topo / NVLink, missing-GPU detection via per-platform lspci
   BDF sets — e.g. Dell H100 {19:00, 3b:00, 4c:00, 5d:00, 9b:00, bb:00,
   cb:00, db:00} — and `(rev ff)` fell-off-the-bus scan). A golden-set
   mismatch only raises GPU MISSING alerts when lspci actually counts fewer
   GPUs than expected — a full complement at unexpected addresses prints a
   dim NOTE instead of eight false alerts. The pushed binary is **cached on
   the node** at `/var/tmp/gpuinspect_cache_<sha256>`, so re-runs skip the
   9MB transfer (the `Pushing bin` preamble line says which path was taken);
   the cache file is safe to delete — the next run just pushes again.
4. Retrieves everything to `~/Downloads/tmp/<BMN>_<ts>/` (ticket/RMA
   evidence): ANSI-free full report log, `collect_bundle_*.tar.gz`,
   `gpu_full.txt`, `verdict.json`, `analysis.md`.
5. **AI analysis** of the findings (Anthropic API; key from
   `ANTHROPIC_API_KEY` or 1Password via `op read` — same vault frop uses),
   then **nodebot diagnose** with the findings as extra context. Nodebot runs
   by default per BMN; it is auto-skipped when frop dissect context is piped
   in (frop dissect *is* a nodebot diagnose), `--no-nodebot` skips it always,
   `--nodebot` forces it back on.

Multiple BMNs are inspected **in parallel** (default 4, `--parallel N`); each
node's colored section prints atomically as it completes, then a summary
**matrix** (BMN | gMAC | SKU | state | verdict | issues | next steps) prints
and is written to `~/Downloads/tmp/summary_<ts>.{txt,md}`. The matrix and
summary files are guaranteed on **every** orchestrated run — single or multi
BMN, piped or TTY — and legacy `--remote` runs emit a single-row matrix +
summary files too. After the colored matrix, an ANSI-free markdown copy
(fleet advisory included) prints between
`--- matrix (markdown — copy for evidence) ---` and `--- end matrix ---`.

`summary_<ts>.md` additionally carries one `## Full report — <BMN>` fenced
section per node with the complete ANSI-free run output — the whole
inspection in a single ticket-attachable file. The terminal markdown block
and `--copy` stay compact (matrix + per-BMN findings only).

**Evidence:** select that fenced markdown block straight from the terminal,
or `pbcopy < ~/Downloads/tmp/summary_<ts>.md` (or run with `--copy`).

## Recipes — which invocation when

**Full investigation of a node you don't understand yet** (the default move —
history + PCIe triage + GPU suite + AI, one nodebot pass total):

    frop dissect -c <BMN> | gpuinspect <BMN>

frop and the on-node inspection run **concurrently** (stdin is read in the
background), so this costs ≈ max(frop, inspection) — about frop's own runtime.
frop's history vetoes the false-positive verdict where it applies, feeds the
AI, and lands in the summary md.

**Quick inspection without history** (no frop available, or the node is fresh
fallout you just want classified):

    gpuinspect <BMN>

**Quick thermal question** — "what are the temps on every GPU right now"
(~1 min; skips PCIe triage, AI and nodebot):

    gpuinspect <BMN> --temps

Use it when a runbook/nodebot advisory says "capture live thermals, don't
trust the idle snapshot", or as the cheap re-check before/after a cw-thermal
AWX run. Remember idle temps clearing ≠ fault cleared — a heat-sink/TIM
defect only shows under load; a GPU hot *at idle* is a strong signal.

**Thermal readout + history in one evidence file**:

    frop dissect -c <BMN> | gpuinspect <BMN> --temps

Temps sample while frop is still running; frop's genuine-fault signals join
the ISSUES column and the full frop text goes into `summary_<ts>.md`. Note
`--temps` skips the AI pass, so the context is captured and signal-checked
but not AI-narrated.

**Thermal sweep across several nodes** (one matrix, ~the time of one node):

    gpuinspect <BMN1> <BMN2> <BMN3> --temps --parallel 8

**Re-check after a DCT action** (single BMN; tells the verdict logic what was
just tried so the advice escalates correctly):

    gpuinspect <BMN> --post-drain      # after the AC power drain
    gpuinspect <BMN> --post-swap       # after an NVMe drive swap
    gpuinspect <BMN> --post-reseat     # after a GPU/NIC endpoint reseat

**Deep GPU evaluation** (extended inventory, retired pages, dmon/pmon; add
intrusive stress tests only with explicit confirmation):

    gpuinspect <BMN> --eval
    gpuinspect <BMN> --eval-dcgm --yes-intrusive
    gpuinspect <BMN> --eval-nvbandwidth --yes-intrusive

**Fleet fallout triage** — scan then inspect every hit (see the linkfail
section below):

    gpuinspect $(linkfail | awk '{print $1}' | grep -E '^ss' | sort -u) --parallel 8

**Non-GPU PCIe chase** (PEX/NIC/NVMe sweep; discovery is GPU-only by default):

    gpuinspect <BMN> --all-pci

## Flags

    --context <ctx>   kubectl context for the mgmt cluster (default: current)
    --out-dir <dir>   evidence dir (default ~/Downloads/tmp; /tmp/gpuinspect if no home dir)
    --parallel <n>    concurrent BMN inspections (default 4)
    --no-ai           skip AI analysis
    --no-golden       skip the golden-state check (runs by default per BMN)
    --no-nodebot      skip nodebot diagnose (runs by default per BMN)
    --nodebot         force nodebot even when piped frop context auto-skips it
    --no-color        disable ANSI colors
    --copy            also copy the markdown matrix to the clipboard (pbcopy/xclip/xsel)
    --all-pci         widen auto-discovery beyond GPU-related devices (PEX/NIC/NVMe
                      sweep); default is GPU-focused — alert BDFs are always checked
    --remote <gMAC>   direct remote mode; --bmn required
    --bmn <BMN>       on-node / direct-remote modes
    --user <user>     remote ssh user (default acc)
    --linux-bin <p>   linux binary to push (default: auto)
    --post-drain / --post-swap / --post-reseat / --triaged
                      re-check runs after DCT action (single BMN)
    --temps           live thermal readout for ALL GPUs — core+HBM temps sampled
                      over a time-bounded GPUINSPECT_TEMP_SECONDS window (default
                      10s, up to 1 Hz), driver thermal thresholds, throttle-reason
                      bits, dmesg thermal history; quick mode, skips PCIe
                      triage/AI/nodebot
    --eval            full GPU evaluation on the node — extended nvidia-smi inventory,
                      retired pages, per-GPU PCIe/AER detail, dmon/pmon samples;
                      results land in eval/ inside the collect bundle
    --eval-dcgm       + dcgmi diag (implies --eval; requires --yes-intrusive)
    --dcgm-level <n>  DCGM diag level 1-4 (default 2)
    --eval-nvbandwidth
                      + nvbandwidth (implies --eval; requires --yes-intrusive)
    --eval-bug-report + nvidia-bug-report.sh (implies --eval; slow but safe)
    --yes-intrusive   confirm GPU-stressing diagnostics (DCGM diag / nvbandwidth)

`--eval` works in every mode — `gpuinspect --eval <BMN>` runs the full
evaluation on the node over Teleport and pulls the bundle back. Real eval
faults (GPU below max PCIe width, DCGM FAIL, nvbandwidth failure) gate the
verdict; heuristics (hot-idle clocks) are informational notes. Tuning env:
`GPUINSPECT_DMON_SECONDS`/`GPUINSPECT_PMON_SECONDS` (20/10),
`GPUINSPECT_DCGM_TIMEOUT`/`GPUINSPECT_NVBW_TIMEOUT`/`GPUINSPECT_BUGREPORT_TIMEOUT`
(600/300/3600).

`--temps` is the quick thermal question — `gpuinspect <BMN> --temps` answers
"what are the temps on every GPU right now" in about a minute: snapshot +
`nvidia-smi -q -d TEMPERATURE` thresholds + a live sampling window (the worst
value seen in the window gates the verdict, not the idle snapshot) + throttle
bits + dmesg thermal history. The window is bounded by TIME, not sample count
— on big HGX nodes a single nvidia-smi query can take several seconds. Both
driver threshold families are handled: absolute (`GPU Slowdown Temp: 89 C` —
fault when the core temp rises to it) and **T.Limit headroom** on H100/newer
drivers (`GPU Slowdown T.Limit Temp: -2 C` — fault when the live
`temperature.gpu.tlimit` headroom falls to it; the summary shows `tlim min`
and `TL <slow>/<maxop>`). Exit 1 on any thermal finding: throttle reason
active, core ≥ slowdown/max-operating, HBM ≥ memory max, headroom down at its
T.Limit, or fewer GPUs visible than expected (never healthy-by-silence). Raw
captures land in `temps/` inside the bundle and a `verdict.json` feeds the
normal matrix/summary. Cannot be combined with `--eval`.

Every GPU-runbook / nvidia-smi command is echoed (`$ cmd`) with its output in
the live report AND the triage log — the eval per-GPU table, retired pages,
dmon/pmon/DCGM/nvbandwidth tails included. Full untruncated copies always land
in the bundle. Per-command report cap: `GPUINSPECT_SHOW_MAX_LINES` (default 200).

## Piping frop dissect (external diagnosis context)

    frop dissect -c <BMN> | gpuinspect <BMN>

gpuinspect's on-node view is a point-in-time snapshot; `frop dissect` sees the
history (alert persistence, prior DO/HO tickets, failed power drains, an RMA
already in flight). Laptop modes read piped stdin as advisory context:

- hard signals (RMA in progress, failed power drain / AC cycle, persistent
  alert) **veto the FALSE-POS verdict** — the node is never advised back to
  ready against known history, and the matrix shows at least DEGRADED;
- the full text is handed to the AI analysis (and to nodebot when forced with
  `--nodebot`) and lands in `summary_<ts>.md`.

Piping composes with `--temps` too (`frop dissect -c <BMN> | gpuinspect <BMN>
--temps`): the live readout runs while frop is still working, frop's signals
join the ISSUES column, and the full frop text lands in the summary md — the
AI pass stays skipped in temps mode.

Stdin is read in the **background**: inspection starts immediately and runs
concurrently with frop, so pipeline wall-clock is ≈ max(frop, inspection)
plus the AI pass — not the sum. The context is only awaited at verdict/AI
time (progress shows `📥 waiting for piped context` if frop is still
running); if it never arrives the run continues without it after
`GPUINSPECT_CONTEXT_TIMEOUT_MINUTES` (default 30).

Piped context also **auto-skips gpuinspect's own nodebot pass** — frop
dissect *is* a nodebot diagnose, so running it again duplicates minutes of
work. `--nodebot` forces it back on.

Independently, a **self-cleared** false-positive claim is withdrawn when the
PCI link alert has been firing continuously for longer than
`GPUINSPECT_FP_MAX_ALERT_AGE_HOURS` (default 72) — a transient does not fire
for days; that is a flapping-link suspect (runbook recurrence caveat).
Idle-port (METAL-4742) classifications are exempt: idle ports persist by design.

## Exit codes

    0 healthy — including the verified FALSE-POS verdict | 1 degraded, advice issued | 2 present for RMA | 3 error

## PEX890xx false-positive verdict (return-to-ready)

RNO2 runbook (Morgan Thompson, morning standup 2026-07-14 — same class as
XID 109): `NodePCILinkSpeed/WidthUnexpected` on a **PEX890xx** PCIe Gen 5
switch port is almost always the health sweep sampling the port while its
link partner is idle or trained down. The on-node triage recognizes the two
observable shapes:

- **idle-port x0** — the bridge has NO device behind it (METAL-4742), and
- **self-cleared** — the alert BDF re-checks at full speed/width.

When *every* PCIe finding is one of those, the node gets the **FALSE
POSITIVE** verdict (exit 0, matrix label `FALSE-POS`) — a definite,
tool-verified green light. The runbook's "no accompanying XID, IB/fabric, or
thermal fault" gate is checked on-node, not left to the operator:

- no XID in dmesg, no fatal AER,
- every InfiniBand-layer fabric port ACTIVE (sysfs; dumped to
  `ib_ports.txt`),
- no GPU HW/SW Thermal Slowdown active (`nvidia_smi_q.txt`),
- GPU suite clean — ECC, remap, NVLink, expected count, no missing or
  `(rev ff)` GPU,
- AER/DPC history sampled on every alert BDF (including self-cleared ones) —
  no retrain noise, no DPC on a recovered port.

The advice is return-to-ready — no RMA, no hardware ticket:

    cwctl flcc node -w return-to-ready -m "sending to ready" <bmn>

The orchestrator additionally enforces the runbook's "only fallout reason"
gate against the mgmt alert feed: any non-PCI-link alert on the BMN withdraws
the classification. Multi-BMN runs roll all `FALSE-POS` nodes into the fleet
advisory with per-node return-to-ready commands **plus a single-paste BULK
loop** (`for n in <all FP BMNs>; do cwctl flcc node -w return-to-ready ...;
done`) to send every false positive back to production in one go. **Caveat (per runbook):**
don't bulk-clear blindly — a BDF that re-alerts across reboots, or appears
alongside real PCIe/GPU errors, is a genuine link fault (reseat via OFR/DCT
or open a hardware ticket). Self-cleared ports get their AER/DPC error
history sampled too: retrain noise (BadTLP/Rollover) or a triggered DPC on a
recovered port is a flapping link and disqualifies the class. As always, gpuinspect never executes cwctl —
the commands are printed advice only.

## Golden-state check (GOLDEN-MIS verdict)

`NodePCILinkSpeed/WidthUnexpected` doesn't measure the link against its own
capability — it compares `node_pci_cur_*` against `expected_pci_link_*`, the
per-`(cw_sku, fwbundle, device)` **golden table**
(`fleetops-common-ansible` `get_pci_golden_state`), joined purely on the BDF
string. Two whole alert classes are therefore **golden-state errors**, not
hardware faults:

- **enumeration/layout mismatch** — after a bundle change or a BIOS
  enumeration shift the node enumerates a *different device* at the alert BDF
  than the golden row expects (e.g. a PEX switch running a perfect
  32 GT/s x16 where golden expects an X550 NIC at 8 GT/s x4). The link never
  degraded; the alert compares two different devices.
- **stale golden values** — same device, wrong expected numbers.

Every orchestrated run reproduces the alert's comparison per alert BDF —
**device identity included**, which the alert rule itself never checks —
querying GloQL through the same Grafana token machinery as the nodebot
preflight, plus a node-wide layout sweep that counts every BDF whose live
device differs from its golden row (the bundle-flip signature). A missing
golden table for the node's `(SKU, fwbundle)` is flagged too
(`NodeMissingPCIGoldenState` class).

Any golden-state error gives the node the **`GOLDEN-MIS`** matrix verdict
(exit ≥ 1), withdraws a PEX890xx false-positive claim (these alerts carry
`failure_cause`, so return-to-ready won't stick), lands in the ISSUES /
NEXT STEPS columns, the summary files, the fleet advisory (`Golden-state
errors: N/M node(s)`) and the AI/nodebot context. The advice is always the
same: **fix enumeration (BIOS settings/NVRAM reset + reapply profile) or
refresh the golden state — a power drain or RMA will never clear it.**

When golden and live *agree* on an alert BDF the check says so explicitly
("alert should clear on the next evaluation" — the table was fixed after the
alert fired), and when they disagree while the on-node triage confirmed real
degradation, the golden table *corroborates* the alert and the normal
runbook path stands.

Skipped (with a dim note, never fatal) when the BMN has no `cwSku`, when
`node-fwbundle.current` is unset (legacy alert path — separate FLAG), or when
no Grafana token resolves. Env: `GRAFANA_API_TOKEN` / `GRAFANA_URL`
(default `grafana.int.coreweave.com`), `GPUINSPECT_GOLDEN_DS_UID`
(default: the GloQL datasource). Disable with `--no-golden`.

## linkfail — fleet scan + gpuinspect pipeline

`linkfail` (source: `team/amerten/projects/linkfail/`) lists the BMNs whose
most recent alert condition is `NodePCILinkSpeedUnexpected` or
`NodePCILinkWidthUnexpected` as a table (name, error, deviceslot, fabric,
zone, SKU, online, state, serial, last transition, PCI addr).

Build it the same way as gpuinspect — the output lands on your `$PATH`:

    cd team/amerten/projects/linkfail
    make build          # -> team/amerten/bin/linkfail

Defaults scan fabrics RNO2-FAB7/RNO2-FAB13, zone RNO2A, state triage, owner
unassigned. `-fabric/-zone/-state/-owner` narrow the scan; `-selector`
overrides all of them:

    linkfail                                                # default fallout scan
    linkfail -fabric RNO2-FAB3 -zone RNO2B
    linkfail -selector 'flcc.coreweave.com/state=production'

The pipeline — scan the fleet, then inspect every hit:

    tl rno2                                                 # mgmt kubectl context (interactive login)
    linkfail                                                # eyeball the fallout table first
    gpuinspect $(linkfail | awk '{print $1}' | grep -E '^ss' | sort -u) --parallel 8

gpuinspect does the rest: per-node PCIe triage + GPU suite + AI + nodebot,
the summary matrix, and — when two or more nodes match only the PEX890xx
idle-port pattern — the fleet advisory with per-node return-to-ready
commands. (Validated 2026-07-27: a 56-node FAB7 sweep through this exact
pipeline classified 55/56 as the false-positive class.)

## Credentials

AI uses `ANTHROPIC_API_KEY` (or `anthropic_api_key`) when set; otherwise
`op read op://eng-fleeteng-frops/anthropic_apikey/password` (run `op signin`
first). **1Password is consulted at most once per run** (multi-BMN runs share
one resolution), and the key is cached in the macOS keychain for **1 hour**,
so back-to-back runs don't re-prompt. Keys are only ever passed via process
environment or keychain — never argv or plaintext files. `--no-ai` needs no
credentials.

`--nodebot` additionally resolves a Grafana Bearer token (frop's
token-viewer item in the same vault, same once-per-run + 1h keychain
caching) and validates it with a preflight query before launching nodebot —
an expired shared token is reported plainly ("needs rotation") instead of
surfacing as nodebot's misleading "Node not found in GloQL" traceback. A
personal `GRAFANA_API_TOKEN` env var takes precedence;
`GPUINSPECT_GRAFANA_OP_REF=op://<vault>/<item>/<field>` points the 1Password
lookup at a personal item instead; `GRAFANA_URL` retargets both the preflight
and nodebot; `GPUINSPECT_SKIP_GRAFANA_PREFLIGHT=1` bypasses the check.
Whatever the source, the token must be valid for the Grafana instance nodebot
queries (fleet GloQL lives on grafana.int.coreweave.com — a per-cluster
sandbox Grafana has no fleet datasources).

## nodebot bootstrap

`--nodebot` finds the CLI via `GPUINSPECT_NODEBOT`, `$PATH`, a
gpuinspect-managed venv, or frop's venv — and when none exist it bootstraps
one exactly like frop does: discovers the source (`GPUINSPECT_NODEBOT_DIR`,
the frop submodule in any `~/coreweave/*/team/*/gox/frop/nodebot`, or a
`git clone` of coreweave/nodebot into the user cache), then
`python3 -m venv` + `pip install -e src[mcp]` under
`~/Library/Caches/gpuinspect/nodebot-venv-1` (one-time, a minute or two).

## Per-user config

`~/.config/gpuinspect/config` (override path with `GPUINSPECT_CONFIG`) holds
`KEY=VALUE` lines applied as environment *defaults* at startup — real env
vars always win. Works in any shell with no profile edits. Store op://
references or plain settings only, never secrets. Example:

    GPUINSPECT_GRAFANA_OP_REF=op://Employee/sa-1-amerten-gpuinspect/password

## Env

    TRIAGE_SSH / TRIAGE_SCP     override transport (default tsh ssh/scp)
    GPUINSPECT_SCAN_SELECTOR    label selector for scan mode
    GPUINSPECT_NODEBOT          path to the nodebot executable
    NVLINK_EXPECTED             healthy NVLink line rate (default 26.562 GB/s)
    ANTHROPIC_MODEL             AI model override (default claude-opus-4-8)
    MODEL                       nodebot model override (default claude-opus-4-8)
    GPUINSPECT_SHOW_MAX_LINES   per-command report line cap (default 200)
    GPUINSPECT_TEMP_SECONDS     --temps sampling window in seconds (default 10)
    GPUINSPECT_DMON_SECONDS / GPUINSPECT_PMON_SECONDS
                                eval sampling windows (default 20/10)
    GPUINSPECT_DCGM_TIMEOUT / GPUINSPECT_NVBW_TIMEOUT / GPUINSPECT_BUGREPORT_TIMEOUT
                                eval tool timeouts in seconds (600/300/3600)
    GPUINSPECT_FP_MAX_ALERT_AGE_HOURS
                                self-cleared FP age gate (default 72)
    GPUINSPECT_CONTEXT_TIMEOUT_MINUTES
                                max wait for piped frop context (default 30)
    LOGDIR, STATEDIR, FORCE_COLOR   as in gpu-node-helper
