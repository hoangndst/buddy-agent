# buddy-agent

A robust, self-evolving autonomous agent harness built in pure Go, supporting both local and cloud environments.

## Overview

`buddy-agent` is an autonomous execution harness designed to run directly in your development environment. It pairs a deterministic ReAct (Reason + Act) loop with durable memory, dynamic skill acquisition, and sandboxed tool execution to perform multi-step engineering and operational workflows.

## Key Architecture

- **Autonomous ReAct Engine**: Deterministic reasoning and tool execution loop with configurable step budgets and live output streaming.
- **Context Compaction & Episodic Anchoring**: Pure Go Three-Partition memory management ($P_1$ immutable system anchor, $P_2$ bounded episodic anchor with deterministic state tables, $P_3$ unpruned working tail), URL topology-aware context capacity ceilings (16k local loopbacks, 64k cloud endpoints), staged boundary micro-pruning, Tier 2 LLM distillation, deterministic Tier 3 fallback, and closed-loop reactive HTTP 400 self-healing recovery.
- **Subprocess Safety & Process Groups**: Executes shell commands within isolated OS process groups (`Setpgid: true` on POSIX, Job Objects on Windows) with escalated dual-phase termination (`SIGTERM` -> 1.5s grace -> `SIGKILL` to negative PGID) and descendant liveness probing to eliminate orphan processes.
- **Memory-Bounded Streaming Truncation**: Line-aware, streaming dual-buffer truncators (70% stdout / 30% stderr) with circular rolling tail ring buffers and delimiter-free UTF-8 rune fallback.
- **Provider Agnostic**: OpenAI-compatible REST integration using official `openai-go` SDK supporting local inference runners and cloud providers.
- **Durable Memory & Persona**: Context-aware memory layer using file-backed storage (`SOUL.md`, `MEMORY.md`, `USER.md`) alongside structured episodic records.
- **Self-Improving Skill System**: Dynamically captures discovered procedures and workflows into an extensible `skills/` library.
- **Zero CGO**: Pure Go for fast, static single-binary compilation across platforms (`linux`, `darwin`, `windows`).

## Architecture & Design Specifications

Approved milestone design specifications are maintained under [`docs/designs/`](docs/designs/):
- [`docs/designs/02_subprocess_safety.md`](docs/designs/02_subprocess_safety.md): Subprocess process group isolation, staged termination, and output safety.
- [`docs/designs/03_context_compaction.md`](docs/designs/03_context_compaction.md): Context compaction pipeline, atomic turn blocks, dual token accounting, and episodic anchoring.

## Getting Started

### Prerequisites
- Go 1.22+
- An OpenAI-compatible API endpoint (local runner or cloud provider)

### Setup

1. Copy the environment configuration:
   ```bash
   cp .env.example .env
   ```
2. Configure your API base URL, key, and model name in `.env`.
3. Build the binary:
   ```bash
   go build -o bin/buddy-agent ./cmd/buddy
   ```
4. Run:
   ```bash
   ./bin/buddy-agent "Inspect the repository and run all tests"
   ```
