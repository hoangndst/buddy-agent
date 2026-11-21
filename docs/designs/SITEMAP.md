# Architecture & Design Specifications Sitemap

This directory contains approved technical design documents for `buddy-agent`, following the Elephant-Goldfish Model (EGM) development lifecycle.

| Milestone | Document | Status | Date | Description |
| :---: | :--- | :---: | :---: | :--- |
| **01** | *(Implemented in PR #1)* | Approved | 2026-11-18 | Core ReAct engine, official `openai-go` SDK, streaming accumulator, canonical schema sorting, and CI. |
| **02** | [`02_subprocess_safety.md`](02_subprocess_safety.md) | Approved | 2026-11-19 | Subprocess process group isolation (`Setpgid`), escalated dual-phase termination (`SIGTERM` -> 1.5s -> `SIGKILL`), descendant liveness probing, and memory-bounded dual-stream line-aware truncation. |
| **03** | [`03_context_compaction.md`](03_context_compaction.md) | Approved | 2026-11-19 | Three-partition context memory, atomic turn block eviction, zero-CGO dual token accounting, two-stage compaction (micro-pruning & distillation), and safe halt fallback. |
| **04** | [`04_model_context_protocol.md`](04_model_context_protocol.md) | Approved | 2026-11-20 | MCP client integration (`modelcontextprotocol/go-sdk`), supervised stdio process groups (`Setpgid`), remote SSE, two-tier JSON config, and tool adapters. |
| **05** | [`05_universal_model_protocols_and_ux.md`](05_universal_model_protocols_and_ux.md) | Approved | 2026-11-20 | Dual API protocol engine (`/v1/chat/completions` & `/v1/responses`), reasoning effort management, and clean human-centric REPL UX. |
| **06** | [`06_sqlite_state_persistence.md`](06_sqlite_state_persistence.md) | Approved | 2026-11-20 | Pure Go SQLite episodic trajectory store, atomic turn persistence, secret redaction, and crash recovery. |
| **07** | [`07_subagent_orchestration.md`](07_subagent_orchestration.md) | Approved | 2026-11-20 | Bounded subagent delegation (depth=1), static role gating, reserve-step synthesis, and hierarchical SQLite sessions. |
| **08** | `08_unix_socket_daemon.md` | Staged | — | Background daemon with Unix domain socket IPC and CLI client. |
| **09** | `09_terminal_user_interface.md` | Staged | — | Bubbletea interactive TUI with streaming output rendering. |
