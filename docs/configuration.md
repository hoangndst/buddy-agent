# Configuration Reference

Buddy Agent loads configuration from YAML files, environment variables, and CLI flags with deterministic precedence:
1. **CLI Flags** (highest priority)
2. **Environment Variables**
3. **Configuration File** (`buddy.yaml` or `.buddy/config.yaml`)
4. **Built-in Defaults** (lowest priority)

## Configuration File (`buddy.yaml`)

An example `buddy.yaml` file located in the workspace root or `~/.buddy/config.yaml`:

```yaml
# Model and Provider Settings
model: "gpt-4o"
base_url: "https://api.openai.com/v1"
api_key: "${OPENAI_API_KEY}"
api_type: "chat"              # "chat" (/v1/chat/completions), "responses" (/v1/responses), or auto-detect
reasoning_effort: "medium"    # "low", "medium", "high" (for reasoning models)

# Execution Constraints
max_steps: 30                 # Maximum ReAct steps per turn
max_context_tokens: 64000     # Context window capacity ceiling
max_output_chars: 25000       # Maximum output characters per tool result
working_tail_size: 10         # Number of active turn blocks retained before compaction

# Environment and Workspace
workspace_dir: "."
socket_path: "~/.buddy/daemon.sock"
db_path: "~/.buddy/history.db"

# Model Context Protocol (MCP) Servers
mcp_servers:
  filesystem:
    command: "npx"
    args: ["-y", "@modelcontextprotocol/server-filesystem", "."]
  github:
    command: "npx"
    args: ["-y", "@modelcontextprotocol/server-github"]
    env:
      GITHUB_PERSONAL_ACCESS_TOKEN: "${GITHUB_TOKEN}"
```

---

## Environment Variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `BUDDY_API_KEY` / `OPENAI_API_KEY` | `""` | API key for the model provider. |
| `BUDDY_BASE_URL` / `OPENAI_BASE_URL` | `https://api.openai.com/v1` | Endpoint base URL. Supports Ollama (`http://localhost:11434/v1`), vLLM, and OpenRouter. |
| `BUDDY_MODEL` | `gpt-4o` | Default model identifier. |
| `BUDDY_API_TYPE` | `""` | Explicit protocol selection: `chat` or `responses`. If empty, auto-detected from model name. |
| `BUDDY_REASONING_EFFORT` | `""` | Reasoning effort for reasoning models (`low`, `medium`, `high`). |
| `BUDDY_SOCKET_PATH` | `~/.buddy/daemon.sock` | Unix domain socket path for daemon IPC. |
| `BUDDY_DB_PATH` | `~/.buddy/history.db` | Path to the SQLite trajectory database. |
| `BUDDY_WORKSPACE_DIR` | `.` | Target workspace directory. |
| `BUDDY_MAX_STEPS` | `30` | Maximum steps per turn. |
| `BUDDY_MAX_CONTEXT_TOKENS` | `64000` | Context token limit before compaction triggers. |
| `BUDDY_MAX_OUTPUT_CHARS` | `25000` | Tool output truncation limit. |
| `BUDDY_WORKING_TAIL_SIZE` | `10` | Unpruned turn blocks retained in working tail. |
| `BUDDY_NOTIFICATIONS` | `unfocused` | Desktop notifications: `unfocused`, `always`, or `never`. |

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
