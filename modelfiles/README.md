# ADA Modelfiles

ADA-tuned [Ollama Modelfiles](https://docs.ollama.com/modelfile). Each one wraps
a coder model at **q8_0 quantization** (instead of the library default q4_K_M)
with decoding parameters matched to ADA's grammar-constrained, JSON-only loop.

## Why bother

- **q8_0 over q4_K_M.** The default `qwen2.5-coder:14b` tag is 4-bit. In
  practice that shows up in the run logs as a stream of
  `incomplete Task: id/command/assertion.channel are required` retries — the
  schema forces valid JSON *shape*, but a heavily quantized model fills required
  fields with empty strings or gives up mid-thought. q8_0 roughly halves the
  quantization error for ~2x the memory.
- **No "q16".** 16-bit is not a quantization — it is the unquantized `fp16`
  release. Every Modelfile notes the matching fp16 tag; swap the `FROM` line if
  you have the (V)RAM (a Modelfile cannot re-quantize an already-quantized
  base, so picking the right `FROM` tag *is* the quantization choice).
- **`repeat_penalty 1.0`.** Ollama's default 1.1 penalizes exactly the tokens a
  JSON schema repeats every emission (`{`, `"`, `"pattern"`, `"channel"`…).
- **Note:** `ada/llm.go` sends `temperature` / `top_p` / `num_predict` /
  `num_ctx` per request, which overrides the same-named `PARAMETER`s here. They
  are still set so standalone `ollama run <name>` behaves like the agent.

## Models

| Modelfile | Base (q8_0) | Weights | fp16 alternative |
|---|---|---|---|
| `ada-qwen2.5-coder.Modelfile` | `qwen2.5-coder:14b-instruct-q8_0` | ~16 GB | `14b-instruct-fp16` (~30 GB) |
| `ada-qwen3-coder.Modelfile` | `qwen3-coder:30b-a3b-q8_0` (MoE, 3B active) | ~32 GB | `30b-a3b-fp16` (~61 GB) |
| `ada-qwen3.5-coder.Modelfile.template` | — not on the Ollama library yet | — | see the template header |

## Build & run

```bash
scripts/create-model.sh                  # build every *.Modelfile
scripts/create-model.sh ada-qwen3-coder  # build one
make create-models                       # same, via make (NAMES="..." for a subset)

ADA_MODEL=ada-qwen2.5-coder make bench
bin/ada_agent -objective "..." -model ada-qwen3-coder
```

The first build pulls the q8_0 base tag, which is a large download.
