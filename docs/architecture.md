# System Architecture

Buddy Agent is structured around a decoupled client-server architecture over Unix domain sockets. The background daemon acts as the authoritative engine, while terminal interfaces act as non-blocking presentation projections.

```
┌─────────────────────────────────────────────────────────────┐
│                 TERMINAL CLIENTS (CLI / TUI)                │
│                                                             │
│   Interactive TUI (Bubble Tea)    One-Shot CLI Runner       │
│   • Virtualized Viewport          • Streaming STDOUT        │
│   • Multiline Composer            • Signal Handler          │
│   • Slash Command Registry        • Non-TTY Fallback        │
└──────────────────────────────┬──────────────────────────────┘
                               │ JSON-RPC 2.0 (NDJSON)
                               ▼
┌─────────────────────────────────────────────────────────────┐
│                 DAEMON & PROTOCOL BOUNDARY                  │
│                                                             │
│   Socket Manager & Listener       Session Manager           │
│   • Single-Instance File Lock     • Conversation Worker     │
│   • Peer Credential Check         • In-Memory Ring Buffer   │
│   • Event Multiplexer             • Approval State Machine  │
└──────────────────────────────┬──────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────┐
│                 CORE RUNTIME & PERSISTENCE                  │
│                                                             │
│   Agent Engine (ReAct)            Persistence & Tools       │
│   • Step Budget & Generation      • SQLite Trajectory Store │
│   • 3-Partition Memory Compactor  • Supervised MCP Manager  │
│   • Model Provider (Chat/Resp)    • Safe Bash Tool (Setpgid)│
└─────────────────────────────────────────────────────────────┘
```

## Architectural Pillars

### 1. Client-Server Separation
* **The Daemon Owns Execution**: The background daemon manages worker goroutines, step execution budgets, LLM generation, tool execution, session whitelisting, and SQLite transactions.
* **The Client Presents State**: Terminal clients render streaming text deltas and tool lifecycle notifications. Clients can disconnect, crash, or detach without affecting background tasks.

### 2. Deterministic ReAct Loop
The agent engine executes an iterative Reason + Act loop:
1. **Context Assembly**: Constructs the model prompt using the 3-partition memory manager (system anchor, episodic state table, and working tail).
2. **Model Request**: Dispatches the prompt to the configured provider using streaming HTTP protocols.
3. **Tool Execution**: Parses tool calls, performs permission checks, executes tools in isolated process groups, and captures bounded stdout/stderr.
4. **Step Persistence**: Atomically commits the step, tool calls, and results to the SQLite trajectory store.
5. **Termination Check**: Evaluates completion criteria or step limits to emit a final result or error.

### 3. Pure Go Portability
* Built with zero CGO dependencies.
* Cross-compiles to standalone static binaries for Linux, macOS, and Windows.
* Uses `modernc.org/sqlite` for pure Go embedded database operations.

### 4. Process Group Safety
* All child processes (bash commands and stdio MCP servers) run inside dedicated operating system process groups (`Setpgid: true` on POSIX, Job Objects on Windows).
* Termination uses a staged escalation sequence: `SIGTERM` followed by a 1.5-second grace period, then `SIGKILL` sent to the negative process group ID (`-PGID`).
