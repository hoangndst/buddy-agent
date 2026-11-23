# Configuration Reference

Buddy Agent loads configuration from environment variables (including `.env` files) and CLI flags with deterministic precedence:
1. **CLI Flags** (highest priority)
2. **Environment Variables** (or `.env` in the current working directory)
3. **Built-in Defaults** (lowest priority)

## Environment Variables

Buddy Agent automatically loads a `.env` file in the working directory on startup if present. You can configure provider settings, workspace boundaries, and execution constraints using environment variables:

| Variable | Default | Description |
| :--- | :--- | :--- |
| `BUDDY_API_KEY` / `OPENAI_API_KEY` | `""` | API key for the model provider. |
| `BUDDY_BASE_URL` / `OPENAI_BASE_URL` | `https://api.openai.com/v1` | Endpoint base URL. Supports Ollama (`http://localhost:11434/v1`), vLLM, and OpenRouter. |
| `BUDDY_MODEL` | `gpt-4o` | Default model identifier. |
| `BUDDY_API_TYPE` | `""` | Explicit protocol selection: `chat` or `responses`. If empty, auto-detected from model name. |
| `BUDDY_REASONING_EFFORT` | `""` | Reasoning effort for reasoning models (`low`, `medium`, `high`). |
| `BUDDY_SOCKET_PATH` | `~/.buddy/daemon.sock` | Unix domain socket path for daemon IPC. |
| `BUDDY_DB_PATH` | `~/.buddy/history.db` | Path to the SQLite trajectory database. |
| `BUDDY_WORKSPACE` | `.` | Target workspace directory. |
| `BUDDY_MAX_STEPS` | `30` | Maximum steps per turn. |
| `BUDDY_MAX_CONTEXT_TOKENS` | `64000` (cloud) / `16384` (local) | Context token limit before compaction triggers. |
| `BUDDY_MAX_OUTPUT_CHARS` | `25000` | Tool output truncation limit. |
| `BUDDY_WORKING_TAIL_SIZE` | `10` | Active turn blocks retained before compaction. |
| `BUDDY_DEBUG` | `false` | Enable verbose debug logging. |

### Example `.env` File

```bash
# Model and Provider Settings
OPENAI_API_KEY=sk-...
BUDDY_MODEL=gpt-4o
BUDDY_BASE_URL=https://api.openai.com/v1

# Execution Constraints
BUDDY_MAX_STEPS=30
BUDDY_MAX_CONTEXT_TOKENS=64000
BUDDY_MAX_OUTPUT_CHARS=25000

# Daemon and Persistence
# Leave BUDDY_SOCKET_PATH and BUDDY_DB_PATH unset to use platform defaults.
# Set either to an absolute path when overriding it.
```

---

## Model Context Protocol (MCP) Configuration

MCP servers are configured via JSON documents loaded from:
* Global configuration: `~/.buddy/mcp.json`
* Project configuration: `<workspace>/.buddy/mcp.json`

Project configuration merges on top of global configuration by server name, allowing individual projects to add servers or disable global servers via `"disabled": true`.

See [MCP Integration](mcp.md) for complete schema documentation and server configuration examples.

---

## Model Providers & Local Models

### Ollama (Local)
```bash
export BUDDY_BASE_URL="http://localhost:11434/v1"
export BUDDY_MODEL="qwen2.5-coder:14b"
export BUDDY_API_KEY="ollama"
buddy
```

### OpenAI
```bash
export OPENAI_API_KEY="sk-..."
export BUDDY_MODEL="gpt-4o"
buddy
```

### OpenRouter
```bash
export BUDDY_BASE_URL="https://openrouter.ai/api/v1"
export BUDDY_API_KEY="sk-or-..."
export BUDDY_MODEL="meta-llama/llama-3.3-70b-instruct"
buddy
```
