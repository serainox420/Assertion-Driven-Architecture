# Assertion-Driven Architecture (ADA) — Prototype

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

## Layout

| Path | What it owns |
|------|--------------|
| `ada/types.go` | The contract: `Task`, `Assertion`, `AnomalyPayload`, `StateSnapshot`, `Fact` (§2) |
| `ada/runtime.go` | The deterministic muscle: execute, enforce limits, adjudicate; blocking/daemon/job modes (§4) |
| `ada/assert.go` | Fact strength + independent-channel checks (fs/process/service) and lazy-regex detection (§3, §8.4) |
| `ada/sanitize.go` | Output truncation + binary detection (§5) |
| `ada/classify.go` | Failure classification and entropy weighting (§8.2) |
| `ada/orchestrator.go` | The loop: entropy, fact folding, Hard Context Fork, weak-fact re-validation (§7, §8) |
| `ada/llm.go` | `LLM` interface + Ollama client with grammar-constrained JSON decoding (§9) |
| `ada/mockllm.go` | Offline, deterministic stand-in for a real model |
| `ada/prompt.go` | The system prompt + JSON schema (§9, §16) |
| `ada/rl.go` | Meta-controller over a small discrete action space; reward shaping (§10) |
| `ada/engine.go` | Supervises a llama.cpp/Ollama subprocess; readiness polling (§14) |
| `cmd/ada/` | CLI + a self-contained offline demo |

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
  still reachable at `$OLLAMA_HOST_URL` (`http://host.docker.internal:11434`).
- **Privilege/limits hardening.** `no-new-privileges`, a `pids_limit`, and
  optional `mem_limit`/`cpus` caps so a runaway can't exhaust the host.
- **Persistent state.** The container plus a docker-managed `ada-home` volume
  (`/root`) survive `stop`/`start`/`rebuild`.

| Command | Does |
|---------|------|
| `make up` | Start the sandbox (builds the image once if missing; preserves state) |
| `make shell` | Open a bash shell inside the running sandbox |
| `make stop` / `make start` | Pause / resume with all state intact |
| `make down` | Remove the container (keeps the `ada-home` volume) |
| `make rebuild` | Rebuild the image from scratch and re-populate `/ada` |
| `make refresh` | Re-populate `/ada` from the host working dir **without** a rebuild |

```bash
make up                 # build + start once
make shell              # hack inside; nothing leaks to the host
# edit files on the host, then push them into the live sandbox:
make refresh            # /ada now matches your working tree (no rebuild)
make stop               # later: make start — state is exactly as you left it
```

Inside the sandbox, point the agent at the host's model server explicitly:

```bash
make run ARGS="-objective '...' -ollama $OLLAMA_HOST_URL"
```

**Optional shared folder.** A single host↔sandbox bridge directory is wired up
but **commented out** in `docker-compose.yml` (host `./.shared` ↔ `/shared`).
Uncomment it and `make up` to turn it on, re-comment and `make up` to turn it
off — it is the only host path the sandbox can ever touch.

## Run it

Requires Go 1.22+.

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

The demo output shows the loop rejecting a lazy assertion, proving real work via an
independent `fs` check (a strong fact), hitting an `env_deterministic` failure, and
adapting to completion:

```
step=1 THINK id=lazy_probe   channel=stdout
step=1 ANOMALY id=lazy_probe class=model_error expected="LAZY_ASSERTION: anchor your pattern (^...$)"
step=2 THINK id=write_config channel=fs
step=2 ACK   id=write_config strength=strong
step=3 THINK id=read_marker  channel=exit_code
step=3 ANOMALY id=read_marker class=env_deterministic expected="0" exit=1
step=4 THINK id=create_marker channel=fs
step=4 ACK   id=create_marker strength=strong
step=4 OBJECTIVE_COMPLETE
```

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
