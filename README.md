# Assertion-Driven Architecture (ADA) — Prototype

> ### A working Go prototype of the **Assertion-Driven Architecture**: a control pattern
> for LLM operations agents that removes self-evaluation from the model and relocates
it to a deterministic runtime.
>
> <img width="900" height="600" alt="image" src="https://github.com/user-attachments/assets/082896d2-f803-42f0-8016-5fca21c52c42" />
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
- **Failure classification + entropy (§8.2).** `connection refused`/`command not found` jump entropy
  fast; `model_error` increments gently; timeouts are transient.
- **Hard Context Fork (§8.3).** Kills daemons, **re-validates weak facts** (drops the unverifiable ones),
  resets entropy, and arms raised-temperature generation so the fork doesn't regenerate the dead strategy.
- **Strength-aware fact folding (§7.3).** Compaction never launders a weak fact into a strong conclusion.
- **Malformed JSON as a virtual anomaly (§9.2).** A parse failure bounces back as a `model_error`
  anomaly instead of crashing the loop.

## Setup scripts

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
| `scripts/ollama-env.sh` | Canonical Ollama env — `source` it before `ollama serve` |

Typical first run:

```bash
make deps WITH=--with-ollama   # or: scripts/install.sh --with-ollama
make build && make test
make demo                       # offline, instant
make model                      # pull qwen2.5-coder:14b
make run ARGS='-objective "create /tmp/app/ready and prove the file exists"'
```

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
