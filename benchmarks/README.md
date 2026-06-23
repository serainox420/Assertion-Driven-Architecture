# Benchmark suites

A *benchmark* is a JSON config file in this folder describing a list of
objectives to run one after another. It is the easy, repeatable way to see how
well ADA handles a spread of common situations — pick a suite in the TUI
(**Run ▸ Benchmark**) or on the CLI (`ada benchmark <name>`), and every task is
driven through the same loop a normal run uses.

This is distinct from the older graded `benchmark/objectives.tsv` (singular,
driven by `make bench` / `scripts/bench.sh`); the suites here are first-class to
the `ada` runtime and produce per-task debug reports.

## Format

```json
{
  "name": "planner-basics",
  "description": "what this suite measures",
  "mode": "plan",                 // "plan" (default) or "flat" — applies to every task
  "tasks": [
    {
      "name": "create-ready-file",          // short label → report filename slug
      "objective": "create /tmp/x and prove it exists",
      "plan": true,                          // optional: override the suite mode for this task
      "max_steps": 8,                        // optional per-task budget overrides
      "max_rounds": 3,
      "goal_steps": 8
    }
  ]
}
```

- **`mode`** sets the default execution mode for the suite. `plan` decomposes
  each objective into verified sub-goals; `flat` drives a single objective
  through the deterministic THINK→ACT→ASSERT loop. A task may override it with
  its own `plan` field.
- **Budgets** (`max_steps`, `max_rounds`, `goal_steps`) are optional. When zero
  or omitted, the active config / `ADA_*` values apply.
- Everything else (model, host, temperature, recovery budgets, memory, …) comes
  from the normal config, so a suite stays portable across machines.

## Scoring

A task counts as a **pass** when it reaches `FINISHED` (the model proved
completion) or `STABLE` (a verified fixed point). `EXHAUSTED` and `FAILED` are
misses. The runner prints an aggregate scorecard (`N/M passed`).

## Debug reports

When debug logging is on (`ada config set debug_enabled true`, or `ADA_DEBUG=1`),
running a suite creates a single directory under the debug folder prefixed
`benchmark-` (instead of the per-run `run-`), and **each task drops its own
consolidated report** inside it — one Markdown file (`01-<task>.md`,
`02-<task>.md`, …) containing all enabled debug logging (meta, steps, raw LLM
responses, anomalies, facts, planner calls, summary).

## Where suites live

Resolved in order: `$ADA_BENCH_DIR`, else `<repo>/benchmarks`, else
`./benchmarks`.
