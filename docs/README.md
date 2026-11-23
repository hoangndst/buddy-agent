# Buddy Agent Documentation

`buddy-agent` is an autonomous developer agent harness written in pure Go. It pairs a deterministic ReAct execution loop with durable memory, background daemon execution over Unix domain sockets, an interactive terminal user interface, Model Context Protocol (MCP) integrations, and episodic SQLite state persistence.

## Documentation Index

| Guide | Description |
| :--- | :--- |
| [CLI Reference](cli.md) | Command-line interface usage, flags, subcommands, and execution modes. |
| [System Architecture](architecture.md) | High-level system design, execution loop, component boundaries, and lifecycle. |
| [Daemon & IPC](daemon.md) | Background daemon architecture, Unix domain socket IPC, and JSON-RPC 2.0 protocol. |
| [Terminal UI](terminal-ui.md) | Interactive Bubble Tea TUI, multi-turn conversation, slash commands, approvals, and `/diff`. |
| [Configuration](configuration.md) | Configuration file (`buddy.yaml`), environment variables, and model provider settings. |
| [Model Context Protocol (MCP)](mcp.md) | Integrating stdio and SSE MCP servers, process group isolation, and tool discovery. |
| [Persistence & Compaction](persistence.md) | Pure Go SQLite storage, session/turn tables, and 3-partition context compaction. |
| [Subagents](subagents.md) | Bounded subagent delegation, execution budgets, role constraints, and hierarchical sessions. |

## Core Invariants

1. **Pure Go (Zero CGO)**: Static compilation across Linux (`amd64`, `arm64`), macOS (`amd64`, `arm64`), and Windows (`amd64`). No external C runtime dependencies.
2. **Authoritative Daemon**: All turn scheduling, tool execution, permissions, and durable SQLite storage are owned by the background daemon. Clients (CLI, TUI) are presentation and controller projections.
3. **Subprocess Isolation**: Shell commands and stdio MCP servers run in dedicated OS process groups with staged termination (`SIGTERM` -> 1.5s grace -> `SIGKILL`) to prevent orphaned processes.
4. **Crash Recovery & Reconnection**: Sessions persist state at turn and step boundaries. Clients can detach without terminating background work and reconnect with ordered event replay.
