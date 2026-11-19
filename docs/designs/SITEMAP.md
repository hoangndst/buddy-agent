# Architecture & Design Specifications Sitemap

This directory contains approved technical design documents for `buddy-agent`, following the Elephant-Goldfish Model (EGM) development lifecycle.

| Milestone | Document | Status | Date | Description |
| :---: | :--- | :---: | :---: | :--- |
| **01** | *(Implemented in PR #1)* | Approved | 2026-11-18 | Core ReAct engine, official `openai-go` SDK, streaming accumulator, canonical schema sorting, and CI. |
| **02** | [`02_subprocess_safety.md`](02_subprocess_safety.md) | Approved | 2026-11-19 | Subprocess process group isolation (`Setpgid`), escalated dual-phase termination (`SIGTERM` -> 1.5s -> `SIGKILL`), descendant liveness probing, and memory-bounded dual-stream line-aware truncation. |
| **03** | `03_context_compaction.md` | Staged | — | Context compaction pipeline, token estimation, and dual-fidelity episodic anchoring. |
| **04** | `04_model_context_protocol.md` | Staged | — | MCP integration (`modelcontextprotocol/go-sdk`) and supervised stdio process lifecycle. |
| **05** | `05_sqlite_state_persistence.md` | Staged | — | Pure Go SQLite episodic trajectory store and crash-recovery audit logger. |
| **06** | `06_subagent_orchestration.md` | Staged | — | Bounded subagent delegation (depth=1) and execution budgets. |
| **07** | `07_unix_socket_daemon.md` | Staged | — | Background daemon with Unix domain socket IPC and CLI client. |
| **08** | `08_terminal_user_interface.md` | Staged | — | Bubbletea interactive TUI with streaming output rendering. |
