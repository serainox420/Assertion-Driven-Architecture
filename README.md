# Assertion-Driven Architecture (ADA) — Prototype

<img width="1280" height="589" alt="ADA20" src="https://github.com/user-attachments/assets/21be101d-5daf-4487-aeaa-d6dcc7539062" />

> ### A working Go prototype of the **Assertion-Driven Architecture**: a control pattern
> for LLM operations agents that removes self-evaluation from the model and relocates
it to a deterministic runtime.
>
> The model proposes a *hypothesis* (an action paired with a machine-checkable
> assertion). A non-LLM runtime executes the action, checks the assertion against
> **observable state**, and routes: assertion holds → record a fact, continue,
> never wake the model; assertion fails → hand the model a structured anomaly and
> force a course correction.

---

This repo implements the contract, runtime, orchestrator loop, and supporting
machinery described in the ADA design report. Section references below (e.g. §3)
point at that report.

```
THINK (LLM)  →  StateSnapshot → exactly one Task (command + assertion)
ACT (Runtime) →  execute; evaluate assertion against observable state (NO LLM)
   ├─ assertion TRUE  → record fact, continue, no LLM call
   └─ assertion FALSE → build AnomalyPayload, wake LLM, force correction
```

## The `ada` runtime — one binary for the whole loop

`ada` is the flagship runtime: a single static Go binary that does everything the
`Makefile` and `scripts/` do, but faster and friendlier — think *ollama, but for
ADA*. Run it with **no arguments** and it opens a full-screen, arrow-key-navigable
**TUI**; pass a subcommand and it behaves as a rich CLI. Every knob is also an
environment variable and a config-file key, so the same settings work in a shell,
a Dockerfile, CI, or the TUI.

```bash
make ada          # build just bin/ada   (make build builds it + ada_agent)
bin/ada           # no args → interactive TUI
bin/ada --help    # the full command reference
```

The TUI is self-sufficient — **everything** can be done from it: run an objective
and watch the loop stream live, drive a planning run, run a **benchmark suite** of
objectives back-to-back, switch/pull/remove Ollama models, edit every setting
(host, model, decoder params, budgets, debug logging, system & planner prompts),
and curate a **toolbox** of commands offered to the model. It
renders colorized logs, syntax-highlighted JSON, and styled markdown throughout.

```
  ADA  ▸ Home                                    qwen2.5-coder:14b  ● online
 ─────────────────────────────────────────────────────────────────────────
 │ Run an objective       Drive one goal through the flat ADA loop
   Planning run           Decompose an open-ended objective and execute
   Models                 List, pull, switch and remove Ollama models
   Settings               Host, model, decoder, budgets, prompts
   Toolbox                Curate the commands offered to the model
 ─────────────────────────────────────────────────────────────────────────
  ↑/↓ move · enter select · q quit
```

### CLI commands

| command | does |
|---------|------|
| `ada run "<objective>"` | drive one objective through the flat ADA loop (live, pretty) |
| `ada plan "<objective>"` | planning mode: decompose → execute → re-plan |
| `ada benchmark [<name>]` | list, or run, a suite of objectives from `benchmarks/` (see [Benchmark suites](#benchmark-suites)) |
| `ada demo [--plan]` | offline demo, no model server needed |
| `ada model list \| pull <tag> \| show <tag> \| rm <tag> \| use <tag>` | manage Ollama models |
| `ada serve` | check / start the Ollama server |
| `ada config list \| get <k> \| set <k> <v> \| edit \| reset` | manage every setting (persisted) |
| `ada tools list \| add \| rm \| enable \| disable` | curate the model's toolbox |
| `ada env [export]` | print (or `export`) the `ADA_*` knobs |
| `ada doctor` | check toolchain, server reachability, model, checkout |
| `ada build \| test \| fmt \| deps \| package \| bench \| sync` | the `scripts/` workflow |
| `ada up \| shell \| stop \| start \| down \| rebuild \| refresh` | the Docker sandbox |

Flags can appear before *or* after the objective; run `ada run -h` for the full
list (`-model`, `-ollama`, `-plan`, `-max-steps`, `-goal-steps`, `-temperature`,
`-meta`, `-memory`, `-color`, …). Precedence is **flags > env > config file >
defaults**.

### Config & environment

State lives in `$ADA_CONFIG` (default `~/.config/ada/config.json`). Any field is
overridable by an env var: `ADA_OLLAMA`/`OLLAMA_HOST`, `ADA_MODEL`,
`ADA_MAX_STEPS`, `ADA_TEMPERATURE`, `ADA_NUM_CTX`, `ADA_COLOR`, … — `ada env`
lists them all. **Tools** are operator-curated commands: enabled ones are injected
into the worker's system prompt as a `TOOLBOX`, steering the model toward
known-good commands and assertions without editing any code.

## Layout

| Path | What it owns |
|------|--------------|
| `ada/types.go` | The contract: `Task` (with pre/postconditions), `Assertion`, `AnomalyPayload`, `StateSnapshot`, `Fact` (§2) |
| `ada/runtime.go` | The deterministic muscle: prior→action→post lifecycle, enforce limits, adjudicate; blocking/daemon/job modes (§4) |
| `ada/assert.go` | Fact strength + independent-channel checks (fs/process/service), fs content matching, lazy-regex detection (§3, §8.4) |
| `ada/sanitize.go` | Output truncation + binary detection (§5) |
| `ada/classify.go` | Failure classification and entropy weighting (§8.2) |
| `ada/orchestrator.go` | The loop: entropy, fact folding, Hard Context Fork, weak-fact re-validation, bounded non-linear recovery (§7, §8) |
| `ada/memory.go` | Persistent, re-validated semantic memory: learn durable facts once, reuse them next run (§5.3) |
| `ada/llm.go` | `LLM` interface + Ollama client with grammar-constrained JSON decoding (§9) |
| `ada/mockllm.go` | Offline, deterministic stand-in for a real model |
| `ada/prompt.go` | The system prompt + JSON schema (§9, §16) |
| `ada/rl.go` | Meta-controller over a small discrete action space; reward shaping (§10) |
| `ada/engine.go` | Supervises a llama.cpp/Ollama subprocess; readiness polling (§14) |
| `cmd/ada/` | the raw `ada_agent` orchestrator CLI + a self-contained offline demo |
| `cmd/adax/` | the flagship `ada` binary: rich CLI + interactive TUI, config, tools, model management, workflow wrappers |

## What the prototype actually enforces

The load-bearing correctness claims from the report are implemented, not just described:

- **Assertion strength (§3).** Strength is derived from the *observation channel*, not the
  model's word. `fs`/`process`/`service`/`exit_code` → **strong** (read independently);
  `stdout`/`stderr` → **weak** (self-satisfiable). Weak facts are provisional everywhere downstream.
- **Independent-channel checks.** `fs` stats the changed file (optionally its mode via `path|0755`),
  `process` greps the kernel's `ps`/`ss` tables, `service` asks `systemctl is-active`.
- **Lazy-regex fail-secure (§8.4).** An unanchored `.*` / `[0-9]+` on an output channel is *rejected*,
  not passed — it becomes a `model_error` anomaly asking for an anchored pattern.
- **Fail-secure default.** An unknown assertion channel is never called success.
- **Output-as-injection defense (§6).** All environment-sourced output in the anomaly is Base64-encoded.
- **Hostile-output guards (§5).** Truncation (head+tail), binary detection, ulimit muzzle.
- **Background-process safety.** A blocking command that backgrounds a child inheriting the
  stdio pipe can't hang the runtime (a `WaitDelay` reclaims the pipes); a real timeout reaps the
  whole process group, not just the shell.
- **Failure classification + entropy (§8.2).** `connection refused`/`command not found` jump entropy
  fast; `model_error` increments gently; timeouts are transient.
- **Hard Context Fork (§8.3).** Kills daemons, **re-validates weak facts** (drops the unverifiable ones),
  resets entropy, and arms raised-temperature generation so the fork doesn't regenerate the dead strategy.
- **Strength-aware fact folding (§7.3).** Compaction never launders a weak fact into a strong conclusion.
- **Malformed JSON as a virtual anomaly (§9.2).** A parse failure bounces back as a `model_error`
  anomaly instead of crashing the loop.
- **In-band completion signal.** The model ends the run by setting `"final": true` on a Task —
  but the loop only terminates if that task's assertion *also passes*. Completion is proven, not
  declared. (`fs` patterns are also normalized, so an over-anchored `^/path$` still resolves.)
- **Deterministic stall guard.** Termination never depends on the model's goodwill. The runtime
  fingerprints each verified proposition; when the model re-proves ground it already established
  (a model that won't emit `final` will loop forever), the loop stops on its own with `STABLE`
  after `-stall` consecutive no-progress successes — and duplicate facts are never recorded.
- **Prior → action → post lifecycle (flexible assertions).** A `Task` carries optional
  **preconditions** (independent-state guards checked *before* the command — an unmet assumption
  skips the command entirely, so a wrong guess costs no action) and **postconditions** (extra
  independent checks *after*). "Verify before you act; verify the outcome in more than one direct
  way." Pre/postconditions accept fs/process/service only — they must be observable without
  trusting the command.
- **Corroboration upgrades strength (§3.2).** A result confirmed by an independent postcondition is
  recorded as a **strong** fact even when the primary assertion was weak — the state was genuinely
  observed, not merely narrated. A postcondition that *disagrees* fails the step: a command that
  claims success while an independent channel says otherwise did not achieve the goal.
- **`fs` content assertions.** `path|contains:^line$` reads the file back and matches an anchored
  regex — the bulletproof "read it to confirm the change really landed", as a *strong* independent
  observation rather than a trusted echo of stdout. Lazy unanchored content patterns are rejected.
- **Bounded non-linear recovery (§8).** Repeated failure of the *same* proposition is detected
  deterministically: each anomaly tells the model how many times this exact approach has failed and
  carries a **directive** to change method; after `-max-attempts` it forces a new strategy (Hard
  Context Fork), and after `-max-routes` strategies the goal is abandoned as `FAILED`. A finite,
  nested budget (default 3 × 3 = 9 tries at a stuck point) — improvise only when needed, only within
  a norm.
- **Re-validated persistent memory (§5.3).** Durable, independently-checkable strong facts persist
  across runs, so the agent learns a basic truth once instead of re-deriving it every task. Every
  persisted fact is **re-observed on load** and dropped if it no longer holds — memory saves the
  *steps* of rediscovery without ever letting a stale claim leak in as trusted truth.

## Setup scripts

> **Reference distro: Arch Linux.** The scripts probe `pacman` first and prefer
> native packages (`go`, `base-devel`, `ollama-rocm`/`ollama-cuda`). Debian/Ubuntu,
> Fedora, openSUSE, and macOS (Homebrew) are auto-detected and fully supported —
> the package manager and package names are mapped per distro, so the same
> `make deps` works everywhere.

A small toolkit under `scripts/` (also exposed via the `Makefile`) handles install,
build, test, model setup, running, and packaging:

| Command | Does |
|---------|------|
| `make deps` / `scripts/install.sh` | Install Go ≥1.22, git, jq, toolchain (multi-distro). `--with-ollama`, `--with-python` optional |
| `make build` / `scripts/build.sh` | Build the static `ada_agent` binary into `bin/` (§14.1) |
| `make test` / `scripts/test.sh` | gofmt check + `go vet` + `go test` (`--race` available) |
| `make demo` / `scripts/run.sh -demo` | Run the offline demo (no model needed) |
| `make model` / `scripts/model.sh` | Start Ollama if needed and pull the worker model |
| `make run ARGS="..."` / `scripts/run.sh ...` | Run the agent (builds first if needed) |
| `make package` / `scripts/package.sh` | Assemble the portable `ada_toolkit` tarball (§14) |
| `make sync` / `scripts/sync.sh` | Update local files from a fresh clone — shows diffs, asks before overwriting |
| `make bench` / `scripts/bench.sh` | Run the graded benchmark objectives (see `benchmark/`) |
| `scripts/ollama-env.sh` | Canonical Ollama env — `source` it before `ollama serve` |

### Updating an existing checkout

```bash
make sync                 # clone fresh, list what differs, prompt before replacing
make sync FLAGS=--dry-run # just show the differences
```

### Benchmark

`benchmark/` holds eight increasingly hard objectives (`objectives.tsv`) with an
expected-output reference and a manual scoring sheet (`expected.md`):

```bash
make bench LEVEL=L4       # run one level
make bench                # run all; transcripts land in benchmark/results/
```

Typical first run:

```bash
make deps WITH=--with-ollama   # or: scripts/install.sh --with-ollama
make build && make test
make demo                       # offline, instant
make model                      # pull qwen2.5-coder:14b
make run ARGS='-objective "create /tmp/app/ready and prove the file exists"'
```

### Sandbox (isolated Docker container)

Because ADA's whole job is to run real, state-changing commands, it is safest to
exercise it inside a throwaway container that **cannot touch the host**. The
`Dockerfile` / `docker-compose.yml` give you exactly that:

- **No host filesystem access.** `/ada` is *copied into the image at build time*,
  not bind-mounted — anything the sandbox does to `/ada` stays in the sandbox.
- **No host networking.** It runs on an isolated bridge; the host's Ollama is
  still reachable over the Docker gateway (address: `http://172.17.0.1:11434`,
  exported as `$OLLAMA_HOST_URL` inside the container).
- **Privilege/limits hardening.** `no-new-privileges`, a `pids_limit`, and
  optional `mem_limit`/`cpus` caps so a runaway can't exhaust the host.
- **Persistent state.** The container plus a docker-managed `ada-home` volume
  (`/root`) survive `stop`/`start`/`rebuild`.

**⚠️ Important:** Ollama must listen on `0.0.0.0:11434` (all interfaces), not
just `127.0.0.1`. Start the server with:

```bash
OLLAMA_HOST=0.0.0.0:11434 ollama serve
```

Or set it permanently in `~/.config/ollama/ollama.env` (path varies by OS).

| Command | Does |
|---------|------|
| `make up` | Start the sandbox (builds the image once if missing; preserves state) |
| `make shell` | Open a bash shell inside the running sandbox |
| `make stop` / `make start` | Pause / resume with all state intact |
| `make down` | Remove the container (keeps the `ada-home` volume) |
| `make rebuild` | Rebuild the image from scratch and re-populate `/ada` |
| `make refresh` | Re-populate `/ada` from the host working dir **without** a rebuild |
| `make clean-sandbox` | Completely remove the container, image, and volume |

```bash
make up                 # build + start once
make shell              # hack inside; nothing leaks to the host
# edit files on the host, then push them into the live sandbox:
make refresh            # /ada now matches your working tree (no rebuild)
make stop               # later: make start — state is exactly as you left it
make clean-sandbox      # nuke everything if needed
```

Inside the sandbox, point the agent at the host's model server:

```bash
make run ARGS="-objective '...' -ollama $OLLAMA_HOST_URL"
```

**Optional shared folder.** A single host↔sandbox bridge directory is wired up
but **commented out** in `docker-compose.yml` (host `./.shared` ↔ `/shared`).
Uncomment it and `make up` to turn it on, re-comment and `make up` to turn it
off — it is the only host path the sandbox can ever touch.

## Run it

Requires Go 1.24+ (the flagship `ada` TUI pulls in newer libraries; the `go` /
`toolchain` directives fetch the right compiler automatically on Go 1.21+).

```bash
# Offline demo — no model server needed. Walks the full failure/recovery narrative.
go run ./cmd/ada -demo

# Against a local Ollama / llama.cpp server:
ollama pull qwen2.5-coder:14b
go run ./cmd/ada \
  -objective "create /tmp/app/ready and prove the file exists" \
  -model qwen2.5-coder:14b \
  -ollama http://localhost:11434 \
  -meta          # optional heuristic meta-controller
```

The demo walks the full flexible-assertion lifecycle: it rejects a lazy assertion; tries a
write whose **precondition** (the app dir exists) is unmet, so the command is *skipped*; makes
the dir, proven via `fs`; writes the config and **corroborates** it with a postcondition that
reads the contents back, so an `exit_code` success is recorded **strong**; then a doomed
readiness probe fails twice, the runtime's **directive** forces a change of method, and it
proves readiness an independent way:

```
step=1 THINK id=lazy_probe    channel=stdout
step=1 ANOMALY id=lazy_probe    class=model_error  expected="LAZY_ASSERTION: anchor your pattern (^...$)"
step=2 THINK id=write_config  channel=exit_code
step=2 ANOMALY id=write_config  class=precondition expected="PRECONDITION_UNMET: fs …/app|dir"   (command skipped)
step=3 THINK id=make_appdir   channel=fs
step=3 ACK   id=make_appdir   strength=strong
step=4 THINK id=write_config  channel=exit_code
step=4 ACK   id=write_config  strength=strong       (exit_code 0 corroborated by an fs content read-back)
step=5 THINK id=prove_ready   channel=process
step=5 ANOMALY id=prove_ready   class=transient attempts=1
step=6 ANOMALY id=prove_ready   class=transient attempts=2   (directive: change the METHOD)
step=7 THINK id=prove_ready   channel=fs
step=7 ACK   id=prove_ready   strength=strong
step=7 OBJECTIVE_COMPLETE
```

## Flexible assertions & bounded recovery

The model still proposes and the runtime still disposes — but a `Task` is now a richer
hypothesis with a **prior → action → post** shape, and recovery is a finite, deterministic
search rather than an open-ended retry. A single emission can read like this:

```json
{
  "id": "set_prod_mode",
  "command": "sed -i 's/^mode:.*/mode: prod/' /etc/app/config.yaml",
  "mode": "blocking",
  "timeout_sec": 10,
  "preconditions":  [{ "type": "file", "pattern": "/etc/app/config.yaml|file", "channel": "fs" }],
  "assertion":      { "type": "exit", "pattern": "0", "channel": "exit_code" },
  "postconditions": [{ "type": "content", "pattern": "/etc/app/config.yaml|contains:^mode: prod$", "channel": "fs" }]
}
```

- The **precondition** is checked first; if the config file isn't there, the `sed` never runs and
  the model is told which assumption was wrong — a bad guess costs no action.
- The **assertion** is the primary success check (here, the editor exited cleanly).
- The **postcondition** reads the file back and confirms the new line is actually present. Because
  an independent channel corroborated the change, the fact is recorded **strong** even though
  `exit_code` alone is only moderately trustworthy. If the postcondition disagreed, the step would
  fail — "it exited 0" is not "it worked".

When an approach fails, the runtime counts how many times that *same* proposition has failed and
hands the model an escalating **directive** to change method. The budget is bounded and nested:

| Flag | Default | Bounds |
|------|---------|--------|
| `-max-attempts` | 3 | tries at one proposition within a strategy before a forced new strategy |
| `-max-routes` | 3 | alternative strategies (Hard Context Forks) before the goal is abandoned `FAILED` |
| `-memory` / `-memory-file` | on | persist & reuse re-validated durable facts across runs |

So a genuinely stuck point gets at most `3 × 3 = 9` tries before ADA stops cleanly — it improvises
only when a failure demands it, and never past the norm.

## Open-ended / complex tasks (planning mode)

The flat loop handles a single concrete objective. For a larger or open-ended task —
one without an obvious end — pass `-plan`. A **planner** breaks the objective into a
flat, ordered list of sub-goals, the flat loop completes each one (carrying verified
facts forward), and the planner **re-plans from the accumulated facts every round**,
continuing until it judges the objective satisfied.

```bash
# Offline planning demo — decomposes, completes sub-goals, declares done.
go run ./cmd/ada -demo -plan

# A real open-ended objective:
go run ./cmd/ada -plan \
  -objective "provision this host as a working web server and prove it serves traffic" \
  -model qwen2.5-coder:14b \
  -max-rounds 8 -goal-steps 40
```

```
round=1 PLAN done=false subgoals=2 reason="provision the workspace files"
round=1 goal=1/2 START "create the config file at …/config.yaml"
round=1 goal=1 FINISHED …
round=1 goal=2 FINISHED …
round=2 PLAN done=true subgoals=0 reason="both workspace files exist and are verified"
=== PLAN RUN COMPLETE: FINISHED ===
```

Design choices (flat, re-plannable — not a rigid tree, per §0):

- **Completion is the planner's call**, judged against verified facts — never the
  executor's say-so. The objective ends when the planner returns `done: true`.
- **Re-plannable list, not a tree.** Each round the next sub-goals are re-derived from
  the current facts, so actions that change the environment don't shatter a pre-committed plan.
- **Always terminates.** `-max-rounds` bounds the run; a round that produces no new
  verified facts ends as `STABLE` (stuck) rather than looping forever.
- Same model can plan and execute, or split a large "driver" planner from a fast
  "worker" executor (§1.5).

## Debug logs

Turn on **debug mode** to capture the full anatomy of a run — model params and
limits, every step's task JSON, the raw LLM response, decoded stdout/stderr,
anomalies, established facts, planner calls, and a final summary. Enable it with
`ada config set debug_enabled true` (or `ADA_DEBUG=1`, or `ada_agent -debug`);
the per-section toggles live under **Settings ▸ Debug** in the TUI.

By default debug mode writes a **single combined report** per run — one
self-contained `run-<id>.md` file under `$XDG_STATE_HOME/ada/debug` with every
enabled section. It's the easiest thing to read, attach to an issue, or diff.

```bash
ada config set debug_enabled true     # combined report is the default
ada run "create /tmp/app/ready and prove the file exists"
# → debug: report ~/.local/state/ada/debug/run-20260623T100527-67d053.md
```

Prefer the legacy layout — a per-run **folder** with one file per channel
(`tasks.jsonl`, `anomalies.jsonl`, `facts.jsonl`, `planner.jsonl`,
`meta.json`, `summary.json`)? Turn the single file off:

```bash
ada config set debug_combined false    # or: ada_agent -debug -debug-combined=false
```

## Benchmark suites

A **benchmark** is a list of objectives, defined as a JSON config in the
`benchmarks/` folder, that ADA runs one after another — the fast, repeatable way
to gauge how well it handles a spread of common situations. Pick one in the TUI
under **Run ▸ Benchmark**, or from the CLI:

```bash
ada benchmark                 # list available suites
ada benchmark planner-basics  # run every task in order; prints an N/M scorecard
```

Five suites ship in [`benchmarks/`](benchmarks/), roughly easy → hard:
`planner-basics` (files, dirs, content, modes), `file-operations` (copying,
permission bits, a project tree), `text-processing` (transforms proven by
reading the result back), `process-and-ports` (background processes & listening
sockets via `ps`/`ss`), and `provisioning` (a full mini-service capstone). Each
has five progressively harder tasks that genuinely need decomposition. A task
passes when it reaches `FINISHED` or `STABLE`.

When debug logging is on, a suite run creates a single directory prefixed
`benchmark-` (instead of the per-run `run-`) and drops **one consolidated report
per task** inside it (`01-<task>.md`, `02-<task>.md`, …), each with all enabled
debug logging in one file. See [`benchmarks/README.md`](benchmarks/README.md)
for the config format.

> The graded `benchmark/` (singular) `objectives.tsv` + `make bench` workflow is
> unchanged; the suites under `benchmarks/` are the runtime-native counterpart.

## Test

```bash
go test ./...
```

Coverage includes: channel→strength mapping, exit/stdout/fs assertions, lazy-regex rejection,
fail-secure on unknown channels, Base64 anomaly encoding, failure classification, timeout
handling, anomaly→recovery, parse-error-as-anomaly, hard-fork weak-fact dropping, and
strength-aware folding.

## Scope

This prototype targets benign **operational** automation (provisioning, remediation,
verification). Offensive-security application is out of scope, per the design report's §0.

## Wiring your own model

Implement the one-method `LLM` interface and pass it to the orchestrator:

```go
type LLM interface {
    GenerateTask(ctx context.Context, snapshot StateSnapshot, temperature float64) (Task, error)
}
```

`OllamaLLM` is provided; `MockLLM`/`ScriptedLLM` make the loop testable without a server.
