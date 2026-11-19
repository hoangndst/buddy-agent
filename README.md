# buddy-agent

A lightweight, local-first, self-evolving autonomous agent harness built in pure Go.

## Overview

`buddy-agent` is an autonomous execution harness designed to run directly in your development environment. It pairs a deterministic ReAct (Reason + Act) loop with durable memory, dynamic skill acquisition, and sandboxed tool execution to perform multi-step engineering and operational workflows.

## Key Architecture

- **Autonomous ReAct Engine**: Deterministic reasoning and tool execution loop with configurable step budgets and live output streaming.
- **Provider Agnostic**: OpenAI-compatible REST integration supporting local inference runners (vLLM, Ollama) and cloud providers.
- **Durable Memory & Persona**: Context-aware memory layer using file-backed storage (`SOUL.md`, `MEMORY.md`, `USER.md`) alongside structured episodic records.
- **Self-Improving Skill System**: Dynamically captures discovered procedures and workflows into an extensible `skills/` library.
- **Context Protection**: Intelligent tool output truncation preserving head/tail context to prevent context window overflow.
- **Native Tools**: Built-in shell runner, targeted file reader, patch-based file editor, and memory management.
- **Zero CGO**: Pure Go for fast, static single-binary compilation across platforms.

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
