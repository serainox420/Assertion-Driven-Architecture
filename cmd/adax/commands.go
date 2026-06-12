package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	ada "github.com/serainox420/assertion-driven-architecture/ada"
)

// signalContext returns a context cancelled on Ctrl-C / SIGTERM, for a clean,
// observable shutdown mid-loop.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// addRunFlags binds override flags directly onto the live config: each flag's
// default is the current config value, so parsing leaves unspecified knobs alone
// and applies only what the operator typed. Flags > env > file > defaults.
func addRunFlags(fs *flag.FlagSet, cfg *Config, plan *bool) {
	fs.StringVar(&cfg.Model, "model", cfg.Model, "worker model tag")
	fs.StringVar(&cfg.OllamaHost, "ollama", cfg.OllamaHost, "Ollama base URL")
	fs.StringVar(&cfg.PlannerModel, "planner-model", cfg.PlannerModel, "planner model (blank ⇒ worker)")
	fs.IntVar(&cfg.MaxSteps, "max-steps", cfg.MaxSteps, "global step budget")
	fs.IntVar(&cfg.MaxRounds, "max-rounds", cfg.MaxRounds, "planning rounds")
	fs.IntVar(&cfg.GoalSteps, "goal-steps", cfg.GoalSteps, "steps per sub-goal (planning)")
	fs.IntVar(&cfg.MaxEntropy, "max-entropy", cfg.MaxEntropy, "entropy ceiling → fork")
	fs.IntVar(&cfg.MaxFacts, "max-facts", cfg.MaxFacts, "fact-folding cap")
	fs.IntVar(&cfg.MaxStuck, "max-stuck", cfg.MaxStuck, "abandon after N no-progress steps")
	fs.IntVar(&cfg.MaxAttempts, "max-attempts", cfg.MaxAttempts, "same-proposition tries per route")
	fs.IntVar(&cfg.MaxRoutes, "max-routes", cfg.MaxRoutes, "strategies before FAILED")
	fs.IntVar(&cfg.Stall, "stall", cfg.Stall, "no-progress successes before STABLE")
	fs.Float64Var(&cfg.Temperature, "temperature", cfg.Temperature, "normal decoder temperature")
	fs.BoolVar(&cfg.Meta, "meta", cfg.Meta, "enable the heuristic meta-controller")
	fs.BoolVar(&cfg.Memory, "memory", cfg.Memory, "persist & reuse re-validated facts")
	fs.StringVar(&cfg.Color, "color", cfg.Color, "colorize output: auto|always|never")
	fs.BoolVar(plan, "plan", *plan, "planning mode: decompose → execute → re-plan")
}

// parseInterspersed parses a flag set that may have positional arguments mixed in
// with flags (e.g. `run "objective" -model x -plan`). Go's flag package stops at
// the first positional; this loop resumes parsing after each one, so flags work
// before OR after the objective. Returns the collected positional tokens.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, args[0])
		args = args[1:]
	}
}

// cmdRun drives a single objective (flat loop, or planning when planMode/-plan).
func cmdRun(cfg *Config, args []string, planMode bool) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var objective string
	fs.StringVar(&objective, "objective", "", "the objective for the agent (or pass it positionally)")
	addRunFlags(fs, cfg, &planMode)
	positionals, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	cfg.normalize()
	initRender(cfg.Color, os.Stderr)

	if strings.TrimSpace(objective) == "" {
		objective = strings.TrimSpace(strings.Join(positionals, " "))
	}
	if objective == "" {
		errf("an objective is required, e.g. `ada run \"create /tmp/x and prove it exists\"`")
		return 2
	}

	ctx, stop := signalContext()
	defer stop()

	client := NewClient(cfg)
	mode := "flat loop"
	if planMode {
		mode = "planning mode"
	}
	infof("%s · model %s · %s", theme.Title.Render("run"), theme.Key.Render(cfg.Model), theme.Dim.Render(mode))
	infof("objective: %s", objective)

	logf := newLogRenderer(os.Stderr)
	res := executeRun(ctx, cfg, client, objective, planMode, logf)
	if res.Err != nil {
		errf("%v", res.Err)
		return 1
	}

	label := "RUN COMPLETE"
	if planMode {
		label = "PLAN RUN COMPLETE"
	}
	fmt.Println(renderSummary(label, res.Outcome, res.PlanReason, res.Snapshot, res.LastError))
	if hint := outcomeHint(res.Outcome); hint != "" {
		fmt.Fprintln(os.Stderr, "\n"+theme.Dim.Render(hint))
	}
	if res.Outcome == ada.OutcomeFinished {
		return 0
	}
	return 0 // a non-FINISHED outcome is still a clean, explained terminal — not an error
}

// cmdDemo runs the offline demonstration (flat or planning).
func cmdDemo(cfg *Config, args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	plan := fs.Bool("plan", false, "run the planning demo instead of the flat demo")
	fs.StringVar(&cfg.Color, "color", cfg.Color, "colorize output: auto|always|never")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	initRender(cfg.Color, os.Stderr)

	ctx, stop := signalContext()
	defer stop()

	fmt.Println(banner("offline demo — no model server needed"))
	logf := newLogRenderer(os.Stderr)
	var res RunResult
	label := "DEMO COMPLETE"
	if *plan {
		res = runPlanDemo(ctx, logf)
		label = "PLAN DEMO COMPLETE"
	} else {
		res = runDemo(ctx, logf)
	}
	if res.Err != nil {
		errf("%v", res.Err)
		return 1
	}
	fmt.Println(renderSummary(label, res.Outcome, res.PlanReason, res.Snapshot, res.LastError))
	return 0
}

// ── model management ──────────────────────────────────────────────────────────

func cmdModel(cfg *Config, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	ctx, stop := signalContext()
	defer stop()
	client := NewClient(cfg)

	switch sub {
	case "list", "ls":
		models, err := client.ListModels(ctx)
		if err != nil {
			errf("cannot reach Ollama at %s: %v", cfg.OllamaHost, err)
			return 1
		}
		if len(models) == 0 {
			warnf("no local models. pull one: ada model pull %s", cfg.Model)
			return 0
		}
		sort.Slice(models, func(i, j int) bool { return models[i].ModifiedAt.After(models[j].ModifiedAt) })
		fmt.Println(theme.Title.Render("Local models") + theme.Dim.Render("  ("+cfg.OllamaHost+")"))
		for _, m := range models {
			marker := "  "
			if m.Name == cfg.Model {
				marker = theme.OK.Render("▶ ")
			}
			size := humanBytes(m.Size)
			meta := strings.TrimSpace(m.Details.ParameterSize + " " + m.Details.QuantizationLevel)
			fmt.Printf("%s%s  %s  %s\n", marker, theme.Key.Render(padRight(m.Name, 34)),
				theme.Dim.Render(padRight(size, 9)), theme.Dim.Render(meta))
		}
		return 0

	case "pull", "get":
		if len(rest) == 0 {
			errf("usage: ada model pull <tag>")
			return 2
		}
		return pullModel(ctx, client, rest[0])

	case "show", "info":
		name := cfg.Model
		if len(rest) > 0 {
			name = rest[0]
		}
		info, err := client.ShowModel(ctx, name)
		if err != nil {
			errf("%v", err)
			return 1
		}
		fmt.Println(theme.Title.Render("model: ") + theme.Key.Render(name))
		for _, k := range []string{"parameters", "template", "system"} {
			if v, ok := info[k].(string); ok && strings.TrimSpace(v) != "" {
				fmt.Println("\n" + theme.Crumb.Render("── "+k+" ──"))
				fmt.Println(strings.TrimRight(v, "\n"))
			}
		}
		if det, ok := info["details"].(map[string]any); ok {
			b, _ := json.MarshalIndent(det, "", "  ")
			fmt.Println("\n" + theme.Crumb.Render("── details ──"))
			fmt.Println(highlightJSON(string(b)))
		}
		return 0

	case "rm", "delete", "remove":
		if len(rest) == 0 {
			errf("usage: ada model rm <tag>")
			return 2
		}
		if err := client.DeleteModel(ctx, rest[0]); err != nil {
			errf("%v", err)
			return 1
		}
		okf("removed %s", rest[0])
		return 0

	case "use", "set":
		if len(rest) == 0 {
			errf("usage: ada model use <tag>")
			return 2
		}
		cfg.Model = rest[0]
		if err := cfg.Save(); err != nil {
			errf("%v", err)
			return 1
		}
		okf("active model is now %s", cfg.Model)
		return 0

	default:
		errf("unknown model subcommand %q (list|pull|show|rm|use)", sub)
		return 2
	}
}

// pullModel downloads a model with a live, in-place progress line.
func pullModel(ctx context.Context, client *Client, name string) int {
	infof("pulling %s from %s", theme.Key.Render(name), client.cfg.OllamaHost)
	last := ""
	err := client.Pull(ctx, name, func(p PullProgress) {
		if p.Total > 0 && p.Completed >= 0 {
			pct := float64(p.Completed) / float64(p.Total) * 100
			fmt.Fprintf(os.Stderr, "\r\033[K%s %s %s",
				theme.Dim.Render(clip(p.Status, 24)), progressBar(pct, 28),
				theme.Dim.Render(fmt.Sprintf("%5.1f%% %s/%s", pct, humanBytes(p.Completed), humanBytes(p.Total))))
		} else if p.Status != last {
			fmt.Fprintf(os.Stderr, "\r\033[K%s %s\n", theme.Crumb.Render("·"), p.Status)
		}
		last = p.Status
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		errf("pull failed: %v", err)
		return 1
	}
	okf("pulled %s", name)
	return 0
}

// ── serve ─────────────────────────────────────────────────────────────────────

func cmdServe(cfg *Config, args []string) int {
	ctx, stop := signalContext()
	defer stop()
	client := NewClient(cfg)

	if err := client.Ping(ctx); err == nil {
		v, _ := client.Version(ctx)
		okf("Ollama is reachable at %s%s", cfg.OllamaHost, dimVersion(v))
		return 0
	}
	warnf("no Ollama server at %s — attempting to start it", cfg.OllamaHost)
	if _, err := exec.LookPath("ollama"); err != nil {
		errf("`ollama` is not installed (try: ada deps --with-ollama)")
		return 1
	}
	// Start `ollama serve` detached; poll for readiness.
	c := exec.Command("ollama", "serve")
	c.Stdout, c.Stderr = nil, nil
	if err := c.Start(); err != nil {
		errf("could not start ollama serve: %v", err)
		return 1
	}
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		if client.Ping(ctx) == nil {
			v, _ := client.Version(ctx)
			okf("ollama serve is up at %s%s", cfg.OllamaHost, dimVersion(v))
			return 0
		}
	}
	errf("ollama did not become ready in 30s")
	return 1
}

func dimVersion(v string) string {
	if v == "" {
		return ""
	}
	return theme.Dim.Render(" (v" + v + ")")
}

// ── config ────────────────────────────────────────────────────────────────────

func cmdConfig(cfg *Config, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls", "show":
		fmt.Println(theme.Title.Render("ada configuration") + theme.Dim.Render("  ("+cfg.Path()+")"))
		for _, f := range cfg.Fields() {
			fmt.Printf("  %s  %s  %s\n", theme.Key.Render(padRight(f.Key, 16)),
				theme.Val.Render(padRight(f.Value, 26)), theme.Dim.Render(f.Desc))
		}
		if len(cfg.Tools) > 0 {
			fmt.Printf("  %s  %s\n", theme.Key.Render(padRight("tools", 16)), theme.Dim.Render(fmt.Sprintf("%d registered (ada tools list)", len(cfg.Tools))))
		}
		return 0
	case "get":
		if len(rest) == 0 {
			errf("usage: ada config get <key>")
			return 2
		}
		for _, f := range cfg.Fields() {
			if f.Key == rest[0] {
				fmt.Println(f.Value)
				return 0
			}
		}
		errf("unknown key %q", rest[0])
		return 1
	case "set":
		if len(rest) < 2 {
			errf("usage: ada config set <key> <value>")
			return 2
		}
		if err := cfg.SetField(rest[0], strings.Join(rest[1:], " ")); err != nil {
			errf("%v", err)
			return 1
		}
		if err := cfg.Save(); err != nil {
			errf("%v", err)
			return 1
		}
		okf("%s = %s  (saved to %s)", rest[0], cfg.fieldValue(rest[0]), cfg.Path())
		return 0
	case "path":
		fmt.Println(cfg.Path())
		return 0
	case "edit":
		return editFile(cfg)
	case "reset":
		fresh := DefaultConfig()
		fresh.path = cfg.Path()
		if err := fresh.Save(); err != nil {
			errf("%v", err)
			return 1
		}
		okf("config reset to defaults at %s", fresh.Path())
		return 0
	default:
		errf("unknown config subcommand %q (list|get|set|path|edit|reset)", sub)
		return 2
	}
}

func (c *Config) fieldValue(key string) string {
	for _, f := range c.Fields() {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

func editFile(cfg *Config) int {
	if _, err := os.Stat(cfg.Path()); err != nil {
		if err := cfg.Save(); err != nil { // materialize defaults so there's something to edit
			errf("%v", err)
			return 1
		}
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command(editor, cfg.Path())
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		errf("%v", err)
		return 1
	}
	okf("saved — reload with any ada command")
	return 0
}

// ── env ───────────────────────────────────────────────────────────────────────

func cmdEnv(cfg *Config, args []string) int {
	export := len(args) > 0 && (args[0] == "export" || args[0] == "-x")
	pairs := [][2]string{
		{"ADA_OLLAMA", cfg.OllamaHost},
		{"ADA_MODEL", cfg.Model},
		{"ADA_PLANNER_MODEL", cfg.PlannerModel},
		{"ADA_TEMPERATURE", ftoa(cfg.Temperature)},
		{"ADA_FORK_TEMP", ftoa(cfg.ForkTemp)},
		{"ADA_PLAN_TEMP", ftoa(cfg.PlanTemp)},
		{"ADA_TOP_P", ftoa(cfg.TopP)},
		{"ADA_NUM_CTX", itoa(cfg.NumCtx)},
		{"ADA_NUM_PREDICT", itoa(cfg.NumPredict)},
		{"ADA_MAX_STEPS", itoa(cfg.MaxSteps)},
		{"ADA_MAX_ROUNDS", itoa(cfg.MaxRounds)},
		{"ADA_GOAL_STEPS", itoa(cfg.GoalSteps)},
		{"ADA_MAX_ENTROPY", itoa(cfg.MaxEntropy)},
		{"ADA_MAX_FACTS", itoa(cfg.MaxFacts)},
		{"ADA_MAX_STUCK", itoa(cfg.MaxStuck)},
		{"ADA_MAX_ATTEMPTS", itoa(cfg.MaxAttempts)},
		{"ADA_MAX_ROUTES", itoa(cfg.MaxRoutes)},
		{"ADA_STALL", itoa(cfg.Stall)},
		{"ADA_MEMORY_ENABLED", btoa(cfg.Memory)},
		{"ADA_MEMORY_FILE", cfg.MemoryPath()},
		{"ADA_META", btoa(cfg.Meta)},
		{"ADA_MARKDOWN", btoa(cfg.Markdown)},
		{"ADA_COLOR", cfg.Color},
		{"ADA_CONFIG", cfg.Path()},
	}
	if !export {
		fmt.Println(theme.Title.Render("ADA environment") + theme.Dim.Render("  (source <(ada env export) to apply)"))
	}
	for _, p := range pairs {
		if export {
			fmt.Printf("export %s=%q\n", p[0], p[1])
		} else {
			fmt.Printf("  %s=%s\n", theme.Key.Render(p[0]), theme.Val.Render(p[1]))
		}
	}
	return 0
}

// ── tools ─────────────────────────────────────────────────────────────────────

func cmdTools(cfg *Config, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		if len(cfg.Tools) == 0 {
			warnf("no tools registered. add one: ada tools add <name> -desc \"...\" -cmd \"...\"")
			return 0
		}
		fmt.Println(theme.Title.Render("Toolbox") + theme.Dim.Render("  (enabled tools are injected into the model's system prompt)"))
		for _, t := range cfg.Tools {
			state := theme.Dim.Render("○ off")
			if t.Enabled {
				state = theme.OK.Render("● on ")
			}
			fmt.Printf("  %s  %s  %s\n", state, theme.Key.Render(padRight(t.Name, 18)), theme.Dim.Render(clip(t.Description, 60)))
			if strings.TrimSpace(t.Command) != "" {
				fmt.Printf("            %s\n", theme.Dim.Render("$ "+clip(t.Command, 70)))
			}
		}
		return 0
	case "add":
		return toolsAdd(cfg, rest)
	case "rm", "remove", "delete":
		if len(rest) == 0 {
			errf("usage: ada tools rm <name>")
			return 2
		}
		return toolsMutate(cfg, rest[0], func(i int) { cfg.Tools = append(cfg.Tools[:i], cfg.Tools[i+1:]...) }, "removed")
	case "enable":
		if len(rest) == 0 {
			errf("usage: ada tools enable <name>")
			return 2
		}
		return toolsMutate(cfg, rest[0], func(i int) { cfg.Tools[i].Enabled = true }, "enabled")
	case "disable":
		if len(rest) == 0 {
			errf("usage: ada tools disable <name>")
			return 2
		}
		return toolsMutate(cfg, rest[0], func(i int) { cfg.Tools[i].Enabled = false }, "disabled")
	case "show":
		if len(rest) == 0 {
			errf("usage: ada tools show <name>")
			return 2
		}
		t := cfg.FindTool(rest[0])
		if t == nil {
			errf("no such tool %q", rest[0])
			return 1
		}
		b, _ := json.MarshalIndent(t, "", "  ")
		fmt.Println(highlightJSON(string(b)))
		return 0
	default:
		errf("unknown tools subcommand %q (list|add|rm|enable|disable|show)", sub)
		return 2
	}
}

func toolsAdd(cfg *Config, args []string) int {
	// The name is the first positional; Go's flag stops at it, so pull it out and
	// parse the flags that follow (`ada tools add <name> -desc ... -cmd ...`).
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("tools add", flag.ContinueOnError)
	desc := fs.String("desc", "", "what the tool does / when to use it")
	cmd := fs.String("cmd", "", "example or template command")
	channel := fs.String("channel", "", "suggested assertion channel (fs/process/service/exit_code)")
	assertion := fs.String("assert", "", "suggested assertion pattern")
	disabled := fs.Bool("disabled", false, "register but leave switched off")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if name == "" {
		errf("usage: ada tools add <name> -desc \"...\" -cmd \"...\" [-channel fs -assert \"path|file\"]")
		return 2
	}
	if cfg.FindTool(name) != nil {
		errf("a tool named %q already exists (rm it first)", name)
		return 1
	}
	cfg.Tools = append(cfg.Tools, Tool{
		Name: name, Description: *desc, Command: *cmd,
		Channel: *channel, Assertion: *assertion, Enabled: !*disabled,
	})
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	okf("added tool %s (%s)", name, map[bool]string{true: "disabled", false: "enabled"}[*disabled])
	return 0
}

func toolsMutate(cfg *Config, name string, mut func(i int), verb string) int {
	for i := range cfg.Tools {
		if strings.EqualFold(cfg.Tools[i].Name, name) {
			mut(i)
			if err := cfg.Save(); err != nil {
				errf("%v", err)
				return 1
			}
			okf("%s tool %s", verb, name)
			return 0
		}
	}
	errf("no such tool %q", name)
	return 1
}

// ── doctor ────────────────────────────────────────────────────────────────────

func cmdDoctor(cfg *Config, _ []string) int {
	ctx, stop := signalContext()
	defer stop()
	fmt.Println(theme.Title.Render("ada doctor") + theme.Dim.Render("  — environment & connectivity check"))

	line := func(ok bool, label, detail string) {
		mark := theme.OK.Render("✓")
		if !ok {
			mark = theme.Bad.Render("✗")
		}
		fmt.Printf("  %s %s  %s\n", mark, padRight(label, 20), theme.Dim.Render(detail))
	}

	// Go toolchain.
	if p, err := exec.LookPath("go"); err == nil {
		line(true, "go toolchain", p)
	} else {
		line(false, "go toolchain", "not found (needed for build/test)")
	}
	// Ollama CLI.
	if p, err := exec.LookPath("ollama"); err == nil {
		line(true, "ollama cli", p)
	} else {
		line(false, "ollama cli", "not found (ada deps --with-ollama)")
	}
	// Server reachability.
	client := NewClient(cfg)
	if err := client.Ping(ctx); err == nil {
		v, _ := client.Version(ctx)
		line(true, "ollama server", cfg.OllamaHost+strings.TrimSpace(" "+v))
		// Model present?
		if models, err := client.ListModels(ctx); err == nil {
			found := false
			for _, m := range models {
				if m.Name == cfg.Model {
					found = true
					break
				}
			}
			line(found, "worker model", cfg.Model+map[bool]string{true: " (present)", false: " (not pulled — ada model pull)"}[found])
		}
	} else {
		line(false, "ollama server", cfg.OllamaHost+" unreachable (ada serve)")
	}
	// Repo (for build/test/sandbox).
	if r := repoRoot(); r != "" {
		line(true, "project checkout", r)
	} else {
		line(false, "project checkout", "not found (build/test/sandbox unavailable here)")
	}
	// Config writable.
	line(cfg.Path() != "", "config file", cfg.Path())
	return 0
}

// ── script & sandbox wrappers ─────────────────────────────────────────────────

// cmdScript wraps the scripts/ workflow targets, running them from the repo root
// with output streamed through. `fmt` is handled natively (no script exists).
func cmdScript(cfg *Config, name string, args []string) int {
	root := repoRoot()
	if root == "" {
		errf("project checkout not found — run this from the repo (or set ADA_HOME=/path/to/checkout)")
		return 1
	}
	if name == "fmt" {
		return runProcess(root, "gofmt", append([]string{"-w", "ada", "cmd"}, args...)...)
	}
	script := map[string]string{
		"build":         "scripts/build.sh",
		"test":          "scripts/test.sh",
		"deps":          "scripts/install.sh",
		"package":       "scripts/package.sh",
		"sync":          "scripts/sync.sh",
		"bench":         "scripts/bench.sh",
		"create-models": "scripts/create-model.sh",
	}[name]
	if script == "" {
		errf("no script mapping for %q", name)
		return 1
	}
	infof("%s  %s", theme.Crumb.Render("▶"), theme.Dim.Render(script+" "+strings.Join(args, " ")))
	// Pass ADA_MODEL / OLLAMA_HOST_URL through so scripts honor the active config.
	env := append(os.Environ(),
		"ADA_MODEL="+cfg.Model,
		"OLLAMA_HOST_URL="+cfg.OllamaHost,
	)
	return runProcessEnv(root, env, filepath.Join(root, script), args...)
}

// cmdSandbox wraps the Docker sandbox lifecycle (docker compose) from the repo root.
func cmdSandbox(cfg *Config, cmd string, args []string) int {
	root := repoRoot()
	if root == "" {
		errf("project checkout not found — sandbox commands need the repo (set ADA_HOME=/path/to/checkout)")
		return 1
	}
	if cmd == "refresh" {
		return runProcess(root, filepath.Join(root, "scripts/sandbox-sync.sh"), args...)
	}
	if cmd == "clean-sandbox" {
		runProcess(root, "docker", "compose", "down", "-v")
		return runProcess(root, "docker", "rmi", "ada-sandbox:latest")
	}
	compose := map[string][]string{
		"up":      {"compose", "up", "-d"},
		"rebuild": {"compose", "up", "-d", "--build"},
		"shell":   {"compose", "exec", "arch", "bash"},
		"stop":    {"compose", "stop"},
		"start":   {"compose", "start"},
		"down":    {"compose", "down"},
	}[cmd]
	if compose == nil {
		errf("unknown sandbox command %q", cmd)
		return 2
	}
	infof("%s  %s", theme.Crumb.Render("▶"), theme.Dim.Render("docker "+strings.Join(append(compose, args...), " ")))
	return runProcess(root, "docker", append(compose, args...)...)
}

// runProcess execs name in dir with stdio attached, returning its exit code.
func runProcess(dir, name string, args ...string) int {
	return runProcessEnv(dir, os.Environ(), name, args...)
}

func runProcessEnv(dir string, env []string, name string, args ...string) int {
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		errf("%v", err)
		return 1
	}
	return 0
}

// ── small formatting helpers ──────────────────────────────────────────────────

func padRight(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(r))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func progressBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	return theme.Key.Render("[") + theme.OK.Render(strings.Repeat("█", filled)) +
		theme.Dim.Render(strings.Repeat("░", width-filled)) + theme.Key.Render("]")
}
