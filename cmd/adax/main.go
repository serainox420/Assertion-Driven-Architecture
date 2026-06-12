// Command ada is the flagship Assertion-Driven Architecture runtime: one binary
// that runs objectives, drives planning, manages Ollama models, edits its own
// config and tools, wraps the build/test/sandbox workflow, and — when invoked with
// no arguments — opens a full-screen interactive TUI. Think of it as ADA's `ollama`.
//
//	ada                      # interactive TUI
//	ada run "make /tmp/x and prove it exists"
//	ada plan "provision this host as a web server"
//	ada demo [--plan]        # offline, no model needed
//	ada model pull qwen2.5-coder:14b
//	ada config set model qwen3-coder:30b
//	ada tools add ...        # curate the model's toolbox
//	ada --help
package main

import (
	"fmt"
	"os"
	"strings"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg := LoadConfig()
	initRender(cfg.Color, os.Stderr)

	args := os.Args[1:]

	// No arguments → the interactive TUI (the headline experience).
	if len(args) == 0 {
		if err := runTUI(cfg); err != nil {
			errf("%v", err)
			os.Exit(1)
		}
		return
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "-h", "--help", "help":
		printHelp()
	case "-v", "--version", "version":
		printVersion(cfg)
	case "tui":
		if err := runTUI(cfg); err != nil {
			errf("%v", err)
			os.Exit(1)
		}
	case "run":
		os.Exit(cmdRun(cfg, rest, false))
	case "plan":
		os.Exit(cmdRun(cfg, rest, true))
	case "demo":
		os.Exit(cmdDemo(cfg, rest))
	case "model", "models":
		os.Exit(cmdModel(cfg, rest))
	case "serve":
		os.Exit(cmdServe(cfg, rest))
	case "config", "cfg":
		os.Exit(cmdConfig(cfg, rest))
	case "env":
		os.Exit(cmdEnv(cfg, rest))
	case "tools", "tool":
		os.Exit(cmdTools(cfg, rest))
	case "doctor":
		os.Exit(cmdDoctor(cfg, rest))
	case "bench":
		os.Exit(cmdScript(cfg, "bench", rest))
	case "build", "test", "fmt", "deps", "package", "sync", "create-models":
		os.Exit(cmdScript(cfg, cmd, rest))
	case "up", "shell", "stop", "start", "down", "rebuild", "refresh", "clean-sandbox":
		os.Exit(cmdSandbox(cfg, cmd, rest))
	default:
		// A bare objective with no subcommand is treated as `run`, so
		// `ada "make /tmp/x"` just works.
		if !strings.HasPrefix(cmd, "-") {
			os.Exit(cmdRun(cfg, args, false))
		}
		errf("unknown command %q — try `ada help`", cmd)
		os.Exit(2)
	}
}

func printVersion(cfg *Config) {
	fmt.Printf("ada %s\n", version)
	fmt.Printf("config: %s\n", cfg.Path())
	fmt.Printf("ollama: %s\n", cfg.OllamaHost)
	fmt.Printf("model:  %s\n", cfg.Model)
}

const helpMarkdown = "" +
	"# ada — Assertion-Driven Architecture runtime\n\n" +
	"Run `ada` with **no arguments** for the interactive TUI.\n\n" +
	"## Core\n" +
	"| command | what it does |\n" +
	"|---|---|\n" +
	"| `ada run \"<objective>\"` | drive one objective through the flat ADA loop |\n" +
	"| `ada plan \"<objective>\"` | planning mode: decompose → execute → re-plan |\n" +
	"| `ada demo [--plan]` | offline demo, no model server needed |\n" +
	"| `ada tui` | open the full-screen interactive interface |\n\n" +
	"## Models (Ollama)\n" +
	"| command | what it does |\n" +
	"|---|---|\n" +
	"| `ada model list` | list locally available models |\n" +
	"| `ada model pull <tag>` | download a model (live progress) |\n" +
	"| `ada model show <tag>` | show a model's template & parameters |\n" +
	"| `ada model rm <tag>` | delete a local model |\n" +
	"| `ada serve` | check / start the Ollama server |\n\n" +
	"## Configuration & tools\n" +
	"| command | what it does |\n" +
	"|---|---|\n" +
	"| `ada config list` | show every setting and its value |\n" +
	"| `ada config set <key> <value>` | change a setting (persisted) |\n" +
	"| `ada config edit` | open the config file in $EDITOR |\n" +
	"| `ada env` | print the ADA_* environment knobs |\n" +
	"| `ada tools list` / `add` / `rm` / `enable` | curate the model's toolbox |\n\n" +
	"## Workflow (wraps scripts/ & docker)\n" +
	"`ada build` · `ada test` · `ada fmt` · `ada deps` · `ada package` · `ada bench` · `ada sync`\n\n" +
	"`ada up` · `ada shell` · `ada stop` · `ada start` · `ada down` · `ada rebuild` · `ada refresh`\n\n" +
	"## Everywhere\n" +
	"Every setting is also an env var (`ADA_MODEL`, `ADA_OLLAMA`, `ADA_MAX_STEPS`, …) and a\n" +
	"config-file key. Flags > env > config file > defaults. Run `ada doctor` to check your setup.\n"

func printHelp() {
	fmt.Println(banner("the assertion-driven runtime — one binary for the whole loop"))
	fmt.Println()
	if useColor {
		fmt.Println(renderMarkdown(helpMarkdown))
	} else {
		fmt.Print(helpMarkdown)
	}
}
