# gpuinspect — agent memory (gitignored; auto-loaded when Claude runs from this dir)

Keep this file current: when you change architecture, flags, flows, or gotchas, update the
relevant line here in the same session. Stay lean — facts not derivable from a quick glance.

## What it is
BMN-first GPU/PCIe inspection tool (Go, single module, evolution of gpu-node-helper).
**Advise-only invariant:** never executes cwctl or state-changing commands; mgmt access is
`kubectl get` only; on-node cmds are read-only; remediations are printed advice.
Runbooks: FRO NodePCILinkSpeedUnexpected (Confluence 649756759), METAL bridge-BDF mapping (1616642163).

## Modes (main.go: parseArgs → loadConfigDefaults → dispatch)
- no args → `runScan` (scan.go): fleet scan of BMNs firing PCI link alerts (kubectl get bmn, label selector `GPUINSPECT_SCAN_SELECTOR`).
- `gpuinspect <BMN>...` → `orchestrateBMNs` (orchestrate.go): the main path.
- `--remote <gMAC> --bmn <BMN>` → `runRemote` (remote.go): legacy direct mode.
- on-node (pushed linux binary re-invokes itself) → `runLocal` (triage.go).

## Per-BMN pipeline (orchestrate.go inspectOneBMN, parallel, default 4)
1. `fetchBMN` (bmn.go): kubectl get bmn JSON → gMAC, SKU, serial, deviceslot, state, expected
   GPU count, alert conditions (BDFs parsed FROM alert messages), fwbundle labels.
2. `bundleCheck`: node-fwbundle current unset → legacy alert path (false-positive class);
   current != target → mid-update.
3. Push `gpuinspect-linux-amd64` over `tsh scp` to acc@<gMAC>, run on-node (remote.go
   runRemoteTo): PCIe triage on alert BDFs (triage.go checkBDF → endpoint.go bridge/endpoint
   mapping, AER/DPC) + GPU suite (gpuanalysis.go: smi inventory/ECC/remapped rows/topo/NVLink,
   missing-GPU via per-platform lspci BDF sets in `gpuPlatforms`, `(rev ff)` scan; collect.go
   bundle+err history).
4. Retrieve to `~/Downloads/tmp/<BMN>_<ts>/` (defaultOutDir() in main.go; falls back /tmp/gpuinspect
   if no home; changed from /tmp 2026-07-27): report log, collect_bundle tar.gz, gpu_full.txt,
   verdict.json (verdictjson.go), analysis.md.
5. `aiAnalyze` (ai.go): Anthropic API on verdict JSON (skip: --no-ai).
6. `runNodebot` (nodebot.go): nodebot diagnose <gMAC> with findings as context, DEFAULT ON
   (skip: --no-nodebot). AUTO-SKIPPED when frop context was piped in (frop dissect IS a nodebot
   diagnose — dim "nodebot skipped" line in buf); --nodebot now FORCES it anyway (o.nodebotForce,
   2026-08-04 — was a compat no-op). Failures degrade to one-line WARN, never fatal.

Exit codes: 0 healthy / 1 degraded / 2 RMA / 3 error (verdict.go). Emojis: ✅⚠️🔴 (progress.go verdictEmoji).

## PEX890xx false-positive verdict (added 2026-07-28, RNO2 runbook Jul-14 standup / Morgan Thompson)
On-node classifier `pexFalsePositive()` (verdict.go): node is the benign PEX890xx class when EVERY
degraded device is idle-port x0 (noChild+down, METAL-4742) and/or alert BDFs self-cleared (PEX switch
healthy at re-check — tracked in triage.alertCleared, set in checkBDF when len(o.bdfs)>0 and
classify=="switch"), AND GPU suite present+clean (no xid/aer/ecc/remap/nvlink/missing/revff/short
count), no pcieErrors/retrainNoise/childCapBelow, no dpcTrig on a self-cleared BDF (collectErrHistory
samples AER/DPC for alertCleared BDFs too — flapping-link guard), no ibFault/thermalFault
(checkFabricThermal in collect.go: IB-layer sysfs ports must be ACTIVE — Ethernet-layer ports report
but don't gate; nvidia-smi -q HW/SW Thermal Slowdown Active), not a --post-* run. ibFault/thermalFault
alone also force DEGRADED(1) so HEALTHY stays a verified all-clear. FP banner prints the verified-gates
checklist + "OK TO RETURN" with the cwctl return-to-ready line. Result: exit 0, green
FALSE-POSITIVE banner with `cwctl flcc node -w return-to-ready -m "sending to ready" <bmn>` advice +
recurrence caveat; verdict.json flag `pex_false_positive` + self-cleared issue lines. Orchestrator
(inspectOneBMN) withdraws the flag if the BMN carries any non-PCI-link alert ("only fallout reason"
gate, bmnPCIAlertRe); matrix VERDICT cell shows FALSE-POS (verdictLabel). NOTE: idle-port-only nodes
used to verdict 1/DEGRADED — now 0/FALSE-POS.
LIVE-VALIDATED 2026-07-29 against the full FAB7 fallout: 66/67 FALSE-POS, 0 real faults
(batch30 30/30; batch2 29 FP + ss892297x4307762 unreachable/power-cycle; batch3 7/7, first live
BULK advisory). Ex-RMA node ss892297x4307765 now clean (likely DCT-serviced — verify history before
clearing). Evidence: ~/Downloads/tmp/evidence_fab7_20260727.md, fleetrun_batch30_20260729_0740.log,
summary_20260729_074816.{txt,md}; return-to-ready scripts ~/Downloads/return_to_ready_batch*.sh.
The 2026-07-28 remote-exec bug ("wait: remote command exited without exit status") did NOT
reproduce anywhere in the 67-node runs, incl. the two nodes where it was first seen.

## Fielddiag warning (2026-08-13)
Nodes in the fielddiag workflow step hang gpuinspect: NVIDIA fieldiag owns the GPUs, so every
on-node nvidia-smi blocks in the driver (found on ss892297x4304555 — 4 stale gpuinspect+nvidia-smi
pairs stuck on the node from hung runs). parseBMN now reads status.flcc.{workflow,workflowStep}
(bmnInfo.Workflow/WorkflowStep); fieldDiagActive() (bmn.go, prefix-match "fielddiag" — covers
fielddiag-reboot) drives fieldDiagWarning(). Warning surfaces in THREE places (inform-only, no
bail): red WARN in the identity block, the LIVE phase line ("⚠️ fielddiag holds the GPUs...expect
a hang" — buffered report never prints on a hang, phase line is what the user actually sees), and
the issues list/matrix. Identity block also gained a "Workflow" row. Tests:
TestParseBMNFieldDiagWorkflow, TestFieldDiagActive. Live-verified against ss892297x4304555.

## Full GPU evaluation (--eval, eval.go, added 2026-08-03)
Port of team/amerten/scripts/gpu_testing_scripts/gpu_eval.sh into the on-node binary (the bash
script's plain ssh/scp can't reach BMNs — gpuinspect's tsh transport can; script is now superseded).
Flags: `--eval` (safe: extended smi query CSV + gpu_inventory.json, retired pages, per-GPU
lspci -vvv -xxxx + sysfs link/AER, dmon/pmon windows), `--eval-dcgm` + `--dcgm-level 1-4` (default 2)
and `--eval-nvbandwidth` (both REQUIRE `--yes-intrusive` — GPU-stressing, the one carve-out from the
read-only invariant, operator-opted), `--eval-bug-report` (slow, safe). All imply `--eval`.
Works in every mode: flags pass through remote.go passthru; orchestrated BMN mode copies opts.
Output: <bundle>/eval/ (inside the tar, auto-retrieved). eval_summary.json = findings + notes.
Verdict wiring: evalFindings (GPU width below max from smi endpoint view, DCGM FAIL/timeout,
nvbandwidth error) block pexFalsePositive and force ≥DEGRADED; evalNotes (hot-idle >500MHz@0%,
missing tools) are informational only. verdict.json: issues get "eval: " prefix, flags eval_ran /
eval_findings. Gen-current < gen-max deliberately NOT flagged (idle ASPM). Env tuning (numeric-
validated, forwarded over ssh): GPUINSPECT_{DMON,PMON}_SECONDS (20/10),
GPUINSPECT_{DCGM,NVBW,BUGREPORT}_TIMEOUT (600/300/3600). Tests: eval_test.go, main_test.go gating.

## XID Bible in AI diagnosis (added 2026-08-03, user request)
aiSystemPrompt (ai.go) embeds a condensed XID Bible (FMAA Confluence 335773794) — app-level/
reset-reboot/triage/ignore classes + sequencing rules (45/62/154 follow a root cause; recurrence
escalates to RMA). Deliberately embedded, NOT fetched at runtime (no Confluence auth in the tool);
re-sync the prompt if the page changes materially. Feeding it: collect.go xidLinesFromDmesg (pure,
last 20 "NVRM: Xid" dmesg lines) → triage.xidLines → verdict.json xid_lines → AI + nodebot context.
Before this the AI only saw boolean xid_in_dmesg, never WHICH Xid. Evidence dir also changed
2026-08-03: ONE dir + ONE sibling <BMN>_<ts>.tar.gz per node (archiveEvidence in remote.go, runs
LAST so analysis.md is included; on-node bundle tar skipped when LOCAL_HINT set; pushed binary
removed remotely pre-scp). HO ticket = attach the tar.gz.

## GH200/GB200 (Grace superchip) false-positive fixes (2026-08-07)
- expectedGPUCount: "GH200" contains "H200" and "GB200" contains "B200", so Grace superchips
  hit the 8-GPU HGX floor → false "only 1/8 GPUs visible" + MISSING GPU PATH on healthy 1-GPU
  GH200s (found on ss900770x4200951). Now: GH200/GB200 trust allocatable, floored at 1.
- nvlinkAnomalous (collect.go, pure fn): NVML "all links are inActive" on a SINGLE-GPU node =
  no NVLink fabric (nothing to link to) → dim note, not an anomaly. Multi-GPU all-inactive and
  any per-link inactive/down line still flag. Tests: TestNvlinkAnomalous, TestExpectedGPUCount.
- Live-verified on ss900770x4200951: issues 4→2 (width degraded + IB port DOWN remain — real).

## GPU-focused scope + tee'd runbook commands (2026-08-04)
- Auto-discovery is NVIDIA-only by default (`discoverVendorsGPU` in triage.go); `--all-pci`
  restores the wide PEX/Mellanox sweep (passes through remote mode). Alert BDFs are ALWAYS
  triaged regardless — they're the fallout reason.
- show.go: echoCmd/echoOutput/runShow/bashShow/runQuiet/bashQuiet/showFileTail — every
  GPU-runbook + nvidia-smi command echoes `$ cmd` and its output through the logger
  (terminal AND triage log), full copy still in the bundle. Cap per command:
  GPUINSPECT_SHOW_MAX_LINES (default 200). Bulky whole-system captures (lspci -vv, dmesg,
  nvidia-smi -q) echo command + "captured to <file>" only.
- collect.go runGPUSuite() replaced the single gpu_full.txt bash blob: same
  `===== section =====` layout in gpu_full.txt, substantive sections shown inline
  (nvidia-smi, list-gpus, ecc nonzero, inventory, remapped rows, topo, nvlink status/anomalies);
  pure-duplicate sections (q grep state, full ecc counters, p2p, nvlink -R) file-only.
- eval.go now prints a per-GPU table (printEvalTable — width-below-max rows in red), echoes
  every eval command, shows dmon/pmon/dcgm/nvbandwidth tails inline (showFileTail).

## Width-runbook full parity (2026-08-28, Confluence 1616642163)
Every check/command from the METAL bridge-BDF mapping runbook now runs AND is echoed tee'd when
`gpuinspect <BMN>` triages a degraded/alert BDF. New: step-1 raw `lspci -vvv | grep LnkCap/LnkSta/
Slot #` evidence (degraded + self-cleared alert BDFs, boxCmd/boxOut in endpoint.go keep the │ box);
step-2 option A `lspci -vt` subtree (pciTree() cached once/run, full copy bundle pcie_tree.txt;
treeLinesFor matches `[bus]`/`[bus-`) + option B sysfs ls echo + option C `lspci -s <bus>:`; step-4
endpoint-side `lspci -vvv | grep LnkCap/LnkSta/LnkCtl2` + three-way call: capBelow (existed) /
childLinkDeg NEW (endpoint cur < endpoint cap ⇒ "degraded on BOTH sides — physical/signal-integrity,
reseat confirmed"; verdict.json issue) / dim "endpoint reports full link — may have retrained";
step-3 identification cmds echoed (nvidia-smi query grep, ls net, ethtool -i, mstvpd/mlxfwmanager,
nvme list -v, dmidecode -t slot); step-5 errhist gained DpcCtl/DpcSta/HeaderLog/CESta/UESta grep
section; interpretation cheat sheet (cheatSheet() pure fn, endpoint.go) printed per degraded BDF
("Runbook :" box line, skipped for noChild idle-port) + verdict reseat block + verdict.json
NextSteps. checkBDF lspci now -vvv (was -vv). ALL inform-only — verdict codes and the live-validated
pexFalsePositive gates untouched (childLinkDeg nodes already fail FP via non-idle degraded child).
Tests: endpoint_test.go (grepLines, nonEmptyLines, childBuses, treeLinesFor, cheatSheet).

## Live thermal readout `--temps` (temps.go, 2026-08-04)
Quick mode: runLocal branches to runTemps() BEFORE the lspci check (needs nvidia-smi only);
skips PCIe triage, and parseArgs forces noAI + nodebot=false (rejects `--temps --eval`).
Snapshot query → `nvidia-smi -q -d TEMPERATURE` thresholds (parseTempLimits, keyed by BDF) →
1 Hz live sampling window (GPUINSPECT_TEMP_SECONDS, default 10; forwarded to the node via the
evalEnv list in remote.go) → per-GPU summary + dmesg thermal grep. Worst temp seen in the
window gates the verdict (tempsFaults, pure): throttle-reason Active, core ≥ slowdown or
max-operating, HBM ≥ memory max, or fewer GPUs than GPUINSPECT_EXPECTED_GPUS → exit 1;
0 GPUs from nvidia-smi → exit 1 (never healthy-by-silence). writeTempsVerdict emits a minimal
verdict.json (flags temps_only + thermal_fault) so the orchestrator matrix renders normally.
Captures land in <bundle>/temps/ — normal tar/retrieval path.

## External context / frop dissect integration (context.go, 2026-08-04)
`frop dissect -c <BMN> | gpuinspect <BMN>` — laptop modes read piped stdin in the BACKGROUND
(main sets o.extCtx = startContextReader(os.Stdin) when !isTTY; NEVER on-node — the push streams
the binary over stdin). Inspection starts immediately; consumers block at gate time via
o.extCtx.wait() (nil-safe → ""; timeout GPUINSPECT_CONTEXT_TIMEOUT_MINUTES default 30 → "").
inspectOneBMN resolves it ONCE right after runRemoteTo (phase "📥 waiting for piped context"
when not ready()); runRemote (legacy) wait()s in the verdict-gating block. Piped context also
AUTO-SKIPS nodebot (see pipeline step 6). Sanitized (ANSI + box-drawing
stripped, 24KB tail cap). externalContextSignals() extracts hard
genuine-fault signals: RMA in progress (+HO/DO ticket ids), failed power drain/AC cycle,
persistence wording. Any signal ⇒ FP verdict WITHDRAWN (orchestrate + legacy --remote), issues
lines added, vcode 0→1. Full context also appended to the AI user msg (ai.go aiAnalyze 3rd arg,
system-prompt section "EXTERNAL DIAGNOSIS CONTEXT") and nodebot context (nodebotContextAppend
2nd arg). Tests: context_test.go (real ss892297x4318514 frop excerpt).

## Self-cleared age gate (orchestrate.go, 2026-08-04)
verdict.json flag `self_cleared` (alertCleared non-empty). If FP && self_cleared && oldest PCI
alert LastTransition older than GPUINSPECT_FP_MAX_ALERT_AGE_HOURS (default 72h) ⇒ FP withdrawn
(flapping-link suspicion, runbook recurrence caveat), vcode 0→1. Idle-port-only FPs are exempt
(they persist by design, METAL-4742). Motivation: ss892297x4318514 — alerts firing since
2026-07-30, two failed drains, RMA HO-187120 in flight, but a port that re-checked healthy
would previously have verdicted FALSE-POS. Any FP withdrawal (incl. the non-PCI-alert gate)
now bumps exit 0→1 so the matrix never shows HEALTHY on a withdrawn claim.

## Golden-state check (goldenstate.go, added 2026-08-18)
Laptop-side, per-BMN, in inspectOneBMN right after verdict.json parse (phase "📐"). Reproduces the
NodePCILink*Unexpected comparison via GloQL (Grafana proxy uid benz42hlglhj4a, same token machinery
as nodebot preflight — grafanaToken(); GRAFANA_URL / GPUINSPECT_GOLDEN_DS_UID override):
expected_pci_link_{speed,width}{cw_sku,fwbundle} (golden table, trino-exporter, duplicated across
core-services clusters — mergeGoldenRows dedups, disagreeing dups → Conflicts) vs
node_pci_cur_{speed,width}{node=<gMAC>} (live). Classification (classifyGolden, PURE) per alert BDF
+ node-wide layout sweep, identity = vendor_id:device_id (fallback device_name):
- IDENTITY MISMATCH (live device ≠ golden row, the ge71c24 2026-08-17 class) / STALE values (same
  device, link at full cap on-node — needs degradedBDFSet(vf)) / NO golden rows
  (NodeMissingPCIGoldenState) / alert BDF absent from table → gc.Mismatch: verdict GOLDEN-MIS
  (verdictLabel precedence ABOVE FALSE-POS), withdraws pex FP (return-to-ready won't stick,
  failure_cause), vcode 0→1, fleet advisory "Golden-state errors: N/M" block, goldenFixAdvice const
  (BIOS/NVRAM reset + reapply profile OR get_pci_golden_state refresh — drain/RMA will NOT clear).
- golden AGREES + on-node degraded → corroboration issue only; values differ + no on-node verdict →
  "unverified" (no mismatch claim); both agree → "now MATCHES live, alert should clear" (golden
  refreshed after alert fired). Width 63/0 idle-port pair counts as agreeing (METAL-4742).
Skips (never fatal, dim line): no cwSku / fwbundle.current unset (legacy path) / no gMAC / no
Grafana token / query error; --no-golden disables. Report section via writeGoldenSection; findings
appended to findingsJSON as "Golden-state check" text section → AI + nodebot see it (aiSystemPrompt
gained a GOLDEN-STATE CHECK doctrine block). Issues/NextSteps flow into matrix + summaries as usual.
Tests: goldenstate_test.go (classify classes, merge/conflict, idle width, label, fleet advisory,
degradedBDFSet, skip gates). Live-verified metric shapes via GloQL 2026-08-18 (ge71c24, b1:00.0).

## Fleet advisory rollup (orchestrate.go, added 2026-07-27, reworked 2026-07-28)
Multi-BMN runs (≥2) roll up isPexFalsePositiveOnly nodes (= FalsePositive flag OR legacy
idle-port-triplet issue pattern). Prints after the matrix + lands in summary txt/md: "N/M match ONLY
the PEX890xx false-positive pattern" → golden-state check a sample, then bulk return-to-ready with
one cwctl line PER node, PLUS a single-paste BULK line (`for n in <all FP bmns>; do cwctl flcc node
-w return-to-ready ...; done`, added 2026-07-29), don't-bulk-clear caveat + "Needs individual
attention: <bmns>".
Tests: orchestrate_test.go, triage_test.go (TestPexFalsePositive).

## Markdown summary format (reworked 2026-08-03, 3-agent judge panel → composite)
summaryMd (orchestrate.go) no longer dumps full "; "-joined issues into table cells. Now: table
ISSUES/NEXT STEPS cells = short category rollups via summarizeFindings/findingCategory ("66 issues:
LINK DOWN ×10, …", ≤4 categories then "…"; short lists pass through verbatim); below the table one
"### <BMN> — <VERDICT>" section per node with findings, bullets deduped by rollupFindings (pure fn:
findings identical modulo ONE BDF collapse to "<pattern with <bdf>> — N BDFs: bdf1 …"; singleton/
BDF-less/multi-BDF verbatim — zero data loss). ss892297x3a23503's 66 issues → 8 bullets, live-
verified. summaryTxt + colored printMatrix untouched. Tests: TestRollupFindings,
TestSummarizeFindings, TestSummaryMdCompactTableAndDetailSections.
2026-08-04: summary_<ts>.md file now = summaryMdFull(results) — summaryMd + per-node
"## Full report — <BMN>" fenced blocks (inspectResult.Output, ANSI stripped, ``` escaped as
"`` `"). Terminal markdown block + --copy clipboard STAY compact (summaryMd). Test: TestSummaryMdFull.

## Matrix / evidence output (added 2026-07-29)
Matrix + summary_<ts>.{txt,md} guaranteed on EVERY orchestrated run (single or multi BMN, piped or
TTY); legacy --remote emits a single-row matrix + summaries too. ANSI-free markdown matrix block
(incl. fleet advisory) prints after the colored matrix between `--- matrix (markdown — copy for
evidence) ---` / `--- end matrix ---`; `--copy` flag (default off) pipes it to the clipboard via
pbcopy/xclip/xsel — "matrix copied to clipboard", or one-line WARN if no clipboard tool (never fatal).

## Progress display (progress.go)
Claude-CLI-style: one animated line per BMN (spinner ~8fps, per-phase elapsed), phases
⏳ queued → 🔎 BMN lookup → 🔬 on-node via <gMAC> → 🤖 AI → 🛰️ nodebot. Node's full buffered
report prints atomically on finish above the progress area; TTY+color only — piped/`--no-color`
falls back to plain timestamped phase lines. Wired via `phase func(string)` callback into inspectOneBMN.
Smoke-test trick: `script -q /tmp/x.log ../../bin/gpuinspect <BMN> ...` (pseudo-TTY exercises ANSI path).

## Credentials (ai.go + keycache.go)
- op:// refs are base64-embedded (`decodeEmbeddedOPRef`) to dodge secret scanners:
  anthropic = op://eng-fleeteng-frops/anthropic_apikey/password; grafana = frop's token-viewer item.
- Precedence: env (`ANTHROPIC_API_KEY`/`anthropic_api_key`; `GRAFANA_API_TOKEN`) → macOS keychain
  cache (1h TTL, `security` CLI, value = "<unix-expiry>:<key>") → `op read` (ONCE per run,
  shared across BMNs). `GPUINSPECT_GRAFANA_OP_REF` overrides grafana op ref.
- Keys travel via process env/keychain only — never argv/files. `--no-ai` needs no creds.
- Grafana preflight (nodebot.go grafanaPreflight) validates token before nodebot launch so an
  expired shared token says "needs rotation" instead of nodebot's bogus "Node not found in GloQL".
  Fleet GloQL = grafana.int.coreweave.com. Skip: `GPUINSPECT_SKIP_GRAFANA_PREFLIGHT=1`.

## nodebot discovery/bootstrap (nodebot.go findNodebot, nodebotsetup.go)
`GPUINSPECT_NODEBOT` → $PATH → gpuinspect venv → frop venv → bootstrap: source from
`GPUINSPECT_NODEBOT_DIR` | `~/coreweave/*/team/*/gox/frop/nodebot` | git clone coreweave/nodebot;
`python3 -m venv` + `pip install -e src[mcp]` at `~/Library/Caches/gpuinspect/nodebot-venv-1`
(version const in nodebotsetup.go). Context passed via FROP_NODEBOT_DIAGNOSE_CONTEXT_APPEND (8KB cap, 12h lookback).

## Config & env
`~/.config/gpuinspect/config` (or `GPUINSPECT_CONFIG`) KEY=VALUE lines → env *defaults* (real env wins).
Other env: TRIAGE_SSH/TRIAGE_SCP (transport override; default tsh ssh/scp), NVLINK_EXPECTED
(26.562 GB/s), ANTHROPIC_MODEL (default claude-opus-4-8), MODEL (nodebot), LOGDIR/STATEDIR/FORCE_COLOR,
RERUN_CMD/LOCAL_HINT (on-node), NO_COLOR.

## Build / test / repo gotchas
- `make build` → native + linux-amd64 into `team/amerten/bin/` (on $PATH). `make test`, `make lint`.
- Binaries no longer git-tracked (as of 2026-07-28/29): consolidated `team/amerten/.gitignore`
  covers `bin/` and stray project binaries; the once-committed 9MB `gpuinspect` binary was removed
  (history stripped via filter-branch before the amerten-ng push). Stray `go build ./...` here is
  now harmless to git, but still prefer `make build`.
- go.mod module name = `gpuinspect` (must match binary). Tests: `go test ./...` all in package root.
- Branch state (2026-07-29): `amerten-ng` MERGED to main via PR #209 (FP verdict + npool-check +
  h100-eval + model default claude-opus-4-8). Now on `amerten-dev`, 3 commits ahead of main
  (0653c45 README, 5a6b0f6 `--copy` flag, 5dab24e markdown matrix + BULK line + `--remote`
  matrix/summary parity), no upstream, NOT pushed. Working tree clean.
  Open items: push amerten-dev + PR; return-to-ready execution for the 66 FAB7 false positives
  (scripts in ~/Downloads) — user-run, tool is advise-only.
- Older session transcripts live under the `gpu-node-helper` project dir (old cwd) — `/resume`
  here won't list them.

## Ecosystem
- `linkfail` (team/amerten/projects/linkfail, ~300-line main.go, built to bin/): kubectl scan of
  mgmt BMNs whose latest alert is NodePCILinkSpeed/WidthUnexpected. Defaults: -fabric
  RNO2-FAB7,RNO2-FAB13 -zone RNO2A -state triage -owner unassigned; -selector overrides all.
  Pipeline: `tl rno2` → `linkfail` → `gpuinspect <hits>`.
- Width-63 idle-port pattern (METAL-4742): bridge with NO device behind it reporting raw width 63
  → known false positive; verdict flags it and advises golden-state check before physical work.
- Requires laptop mgmt kubectl context (`tl <region>`); tsh login is interactive — user runs it.
