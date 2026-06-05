# ADA Benchmark — objectives & expected outputs

A graded set of benign operational objectives, increasing in difficulty. Use it to
eyeball how well a given model drives the loop: does it write **strong** assertions,
pick the **right channel**, **recover** from anomalies, and **signal completion**
with `"final": true`?

Run one level or all of them:

```bash
make bench LEVEL=L3          # one level
make bench                   # all levels
scripts/bench.sh L1 L5 L8    # a custom subset
```

Each run writes a transcript to `benchmark/results/<level>.log` and prints the
final outcome + established facts. Compare against the **Expected** sections below.
The objective strings live in `benchmark/objectives.tsv` (single source of truth).

A run is a **pass** if: outcome is `FINISHED` **or `STABLE`**, the listed observable
state exists, and the facts that carry it are **strong** (not `stdout`-asserted).
`FINISHED` means the model signaled completion with `"final": true`; `STABLE` means
the runtime detected a fixed point (the objective was met but the model didn't say
so) — both are successful terminals. `EXHAUSTED`, weak facts standing in for state
changes, or a premature `final` are **fails**.

> Scratch dir for every level is `/tmp/ada-bench`. Reset between runs with:
> `rm -rf /tmp/ada-bench`.

---

## L1 — file exists (warm-up)

**Objective:** create the file `/tmp/ada-bench/ready` and prove it exists.

- **Channels:** `fs`.
- **Expected final state:** `/tmp/ada-bench/ready` exists.
- **Verify manually:** `test -f /tmp/ada-bench/ready && echo OK`
- **Good final Task:** `mkdir -p /tmp/ada-bench && touch /tmp/ada-bench/ready` with
  `{"channel":"fs","pattern":"/tmp/ada-bench/ready"}`, `"final": true`.
- **Watch for:** asserting on `stdout` (weak) instead of `fs`; anchoring the path
  (`^/tmp/...$`) — the runtime tolerates it now, but a literal path is correct.

## L2 — capture a value to a file

**Objective:** write the kernel release (`uname -r`) into `/tmp/ada-bench/kernel.txt`
and prove the file exists and is non-empty.

- **Channels:** `fs` (existence + non-empty).
- **Expected final state:** `kernel.txt` exists and contains the kernel version.
- **Verify manually:** `test -s /tmp/ada-bench/kernel.txt && cat /tmp/ada-bench/kernel.txt`
- **Good final assertion:** `{"channel":"fs","pattern":"/tmp/ada-bench/kernel.txt|nonempty"}` —
  the `fs` channel proves both existence and size>0 in one strong check. (The §3.4 shell-sentinel
  form — `[ -s file ] && echo OK` asserted on `stdout ^OK$` — is also valid.)
- **Watch for:** asserting that the *command printed* the version (weak) rather than that
  the *file* holds it.

## L3 — small report, free-form command

**Objective:** identify the current user and OS and save a short report to
`/tmp/ada-bench/env.txt`, then prove the file exists.

- **Channels:** `fs`.
- **Expected final state:** `env.txt` exists, containing e.g. `whoami`, `uname -a`,
  and/or `/etc/os-release` excerpts.
- **Verify manually:** `cat /tmp/ada-bench/env.txt`
- **Watch for:** interactive commands; multiple steps without folding into a single
  report file; declaring `final` before the file exists.

## L4 — file content **and** permission bits

**Objective:** create `/tmp/ada-bench/app/config.yaml` containing the line `mode: prod`
with file mode `0644`, and prove it exists with that mode.

- **Channels:** `fs` with the `path|mode` form.
- **Expected final state:** the file exists, contains `mode: prod`, mode is `0644`.
- **Verify manually:** `stat -c '%a' /tmp/ada-bench/app/config.yaml; cat /tmp/ada-bench/app/config.yaml`
- **Good final assertion:** `{"channel":"fs","pattern":"/tmp/ada-bench/app/config.yaml|0644"}`.
- **Watch for:** creating the dir is implied (`mkdir -p`); forgetting `chmod 644`; asserting
  existence only and ignoring the mode.

## L5 — daemon + listening socket

**Objective:** start a background HTTP server on port 8085 serving `/tmp/ada-bench` and
prove the port is listening. *(needs `python3`)*

- **Channels:** `daemon` mode + `process` (socket table).
- **Expected final state:** a `python3 -m http.server 8085` process is up; port 8085 is
  `LISTEN`.
- **Verify manually:** `ss -tlnp | grep ':8085'` (or `curl -sS localhost:8085 >/dev/null && echo OK`)
- **Good shape:** `mode: "daemon"`, command `cd /tmp/ada-bench && python3 -m http.server 8085`,
  assertion `{"channel":"process","pattern":":8085"}`.
- **Watch for:** `blocking` mode (would hang until timeout); asserting on the server's
  startup banner (weak) instead of the kernel socket table (strong). Stop it after:
  `pkill -f 'http.server 8085'`.

## L6 — tool presence + version capture

**Objective:** verify that `jq` is installed, capture `jq --version` into
`/tmp/ada-bench/jq.txt`, and prove that file exists.

- **Channels:** `fs` (and optionally `exit_code` for the presence probe).
- **Expected final state:** `jq.txt` exists, containing e.g. `jq-1.7`.
- **Verify manually:** `cat /tmp/ada-bench/jq.txt`
- **Watch for:** if `jq` is absent the agent should report the anomaly, not fabricate a
  version. (Install it first with `scripts/install.sh` if you want a clean pass.)

## L7 — compound condition via a shell sentinel (§3.4)

**Objective:** using a single sentinel check, validate that
`/tmp/ada-bench/app/config.yaml` exists and is non-empty, and prove success.

- **Channels:** `stdout` — *legitimately*, because the sentinel is emitted only after a real
  multi-condition check of independent state.
- **Pre-req:** run L4 first (creates the config).
- **Expected final state:** no new state; the agent proves a property.
- **Good final Task:**
  `if [ -f /tmp/ada-bench/app/config.yaml ] && [ -s /tmp/ada-bench/app/config.yaml ]; then echo CONFIG_OK; else exit 1; fi`
  asserted on `stdout ^CONFIG_OK$`, `"final": true`.
- **Watch for:** growing an `AND`/`OR` grammar in the assertion instead of pushing the
  boolean logic into the shell; an unanchored sentinel pattern.

## L8 — idempotent self-heal (daemon + pidfile)

**Objective:** ensure `/tmp/ada-bench/daemon.pid` points to a live process; if it does not,
start a `sleep 600` background daemon, record its PID to that file, then prove the process
is running.

- **Channels:** `fs` (pidfile) + `process` (liveness).
- **Expected final state:** `daemon.pid` exists; the PID it names is alive (`sleep 600`).
- **Verify manually:** `kill -0 "$(cat /tmp/ada-bench/daemon.pid)" && echo ALIVE`
- **Good shape:** a check-then-act: probe `kill -0 $(cat pidfile)`, and on failure start
  the daemon (`mode: daemon`, `sleep 600 & echo $! > pidfile`), final assertion on the
  process being live. This is the §15 idempotency pattern in miniature.
- **Watch for:** asserting the pidfile *exists* but not that the PID is *alive* (a stale
  pidfile passes a weak check); not handling the already-running case idempotently.
  Stop it after: `kill "$(cat /tmp/ada-bench/daemon.pid)"`.

---

### Scoring sheet (manual)

| Level | FINISHED? | State present? | Facts strong? | Recovered from an anomaly? | Notes |
|-------|-----------|----------------|---------------|----------------------------|-------|
| L1 | | | | | |
| L2 | | | | | |
| L3 | | | | | |
| L4 | | | | | |
| L5 | | | | | |
| L6 | | | | | |
| L7 | | | | | |
| L8 | | | | | |
