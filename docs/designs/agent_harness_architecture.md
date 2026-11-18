# Design: Comprehensive Agent Harness Architecture for buddy-agent

* **Author:** hoangndst
* **Date:** 2026-11-18
* **Status:** Finalized (Ready for Review / Implementation)
* **Target Package:** `github.com/hoangndst/buddy-agent`

---

## 1. Overview & Problem Statement

### 1.1 The Problem
Building an autonomous coding and operational agent in Go requires more than a simple request-response loop. Existing naive agent implementations suffer from five critical failure modes:
1. **Context Window Exhaustion & Amnesia**: Multi-turn tasks accumulate unbounded tool outputs. Naive prose summarization drops compiler errors, file paths, and exact line numbers, causing agents to repeatedly execute failed actions.
2. **Subprocess Orphan Leaks**: When shell commands or external Model Context Protocol (MCP) servers spawn wrappers (e.g., `npx`, `uvx`, bash scripts), terminating the parent process leaves rogue background child processes running on the host.
3. **Database Concurrency Deadlocks**: In multi-task environments, concurrent writes from background scheduled tasks, CLI runs, and subagents cause SQLite write lock collisions (`database is locked`).
4. **Prompt Cache Degradation**: Non-deterministic tool ordering and fluctuating tool definitions invalidate provider KV-caches (such as OpenAI and Anthropic), dramatically increasing latency and API costs.
5. **Unbounded Subagent Recursion**: Without strict hierarchy limits, delegating tasks to subagents can trigger infinite recursive fork-bombs that exhaust API quotas and system resources.

### 1.2 Solution Overview
`buddy-agent` is a production-grade, local-first autonomous agent harness implemented in pure Go (zero CGO). It establishes:
* **First-Party SDK Alignment**: Standardization on `github.com/openai/openai-go` and `github.com/modelcontextprotocol/go-sdk`.
* **Three-Partition Context Compaction**: Preservation of technical facts via a deterministic Semantic Delta Accumulator and spill-to-disk for outputs exceeding 64KB.
* **Process-Group Isolation**: POSIX `Setpgid` process group encapsulation for all shell tools and MCP subprocesses.
* **Three-Tier Memory Architecture**: Versioned procedural skills (`skills/*.md`), human-editable semantic memory (`SOUL.md`, `USER.md`, `MEMORY.md`), and SQLite episodic history with a single-writer actor.
* **Bounded Subagent Delegation**: Strict depth = 1 delegation with dedicated sub-budgets and isolated execution contexts.
* **Dual Execution Topologies**: Ephemeral CLI (`buddy run`) with UNIX pipe support, and a background service (`buddy daemon`) with Unix domain socket IPC.

---

## 2. Goals & Non-Goals

### 2.1 In-Scope Goals
* **G1: Official SDK Modernization**: Complete migration to `github.com/openai/openai-go` with streaming delta accumulation and lexicographical schema canonicalization.
* **G2: Process-Safe MCP Integration**: Stdio supervisor using `github.com/modelcontextprotocol/go-sdk` with `Setpgid` group cleanup and two-tier timeouts (10s discovery, 60s execution).
* **G3: Memory & Anti-Amnesia Compactor**: Pure-Go SQLite episodic storage (`modernc.org/sqlite`) in WAL mode with a single-writer channel actor, paired with a three-partition context compactor.
* **G4: Bounded Delegation & Daemon Scheduler**: `delegate_task` tool limited to depth=1, alongside `buddy daemon` featuring cron scheduling and local Unix socket IPC (`0600`).
* **G5: LLM Ops & Deterministic Evals**: Structured error taxonomy, cost/token metrics, and deterministic offline benchmark suite.

### 2.2 Out-of-Scope Non-Goals
* **NG1: Multi-Tenant Cloud Hosting**: `buddy-agent` is designed exclusively as a local-first single-user harness; multi-tenant auth and cloud databases are out of scope.
* **NG2: Arbitrary Deep Subagent Nesting**: Subagents are strictly forbidden from spawning further subagents (max depth = 1).
* **NG3: Dynamic Untrusted Code Installation**: The agent will not autonomously download or install unconfigured packages from the web during task turns.

---

## 3. Technical Plan & Architecture

```
 ┌─────────────────────────────────────────────────────────────────────────────┐
 │                               Entrypoints                                   │
 │       buddy run "<task>" (CLI)        │      buddy daemon (Background)       │
 └───────────────────────┬───────────────────────────────┬─────────────────────┘
                         │ Pipe Stdin                    │ Unix Socket (~/.buddy/daemon.sock)
                         ▼                               ▼
 ┌─────────────────────────────────────────────────────────────────────────────┐
 │                           Agent Engine Core                                 │
 │  ┌───────────────────────────────────────────────────────────────────────┐  │
 │  │                         Context Assembler                             │  │
 │  │  - Immutable Anchor (SOUL.md, USER.md, Task Instructions)             │  │
 │  │  - Semantic Delta Accumulator (Files, Invariants, Failed Hypotheses)   │  │
 │  │  - Sliding Window (Last 6 turns verbatim)                             │  │
 │  └───────────────────────────────────┬───────────────────────────────────┘  │
 │                                      ▼                                      │
 │  ┌───────────────────────────────────────────────────────────────────────┐  │
 │  │                   openai-go Client & Streaming Engine                 │  │
 │  │  - Canonical Lexicographical Schema Ordering (KV-Cache Optimization)   │  │
 │  │  - Streaming Delta Accumulator (map[int]*toolCallAccumulator)         │  │
 │  └───────────────────────────────────┬───────────────────────────────────┘  │
 └──────────────────────────────────────┼──────────────────────────────────────┘
                                        │ Dispatches Tool Call
         ┌──────────────────────────────┴──────────────────────────────┐
         ▼                                                             ▼
 ┌───────────────────────────────────────────┐   ┌───────────────────────────────────────────┐
 │               Native Tools                │   │           MCP Extension Bridge            │
 │ - bash_posix.go (Setpgid + SIGKILL group) │   │ - modelcontextprotocol/go-sdk             │
 │ - file_ops.go (read, write, edit)         │   │ - 10s Discovery / 60s Execution Timeouts  │
 │ - delegate.go (Child agent, depth=1 limit)│   │ - Per-server Request Serialization Mutex  │
 └─────────────────────┬─────────────────────┘   └─────────────────────┬─────────────────────┘
                       │ Payload > 64KB                                │
                       ▼                                               ▼
 ┌───────────────────────────────────────────────────────────────────────────────────────────┐
 │                                   Storage & Telemetry                                     │
 │  - Payload Spill Disk Store (.buddy/spills/<uuid>.log)                                    │
 │  - Single-Writer Actor Channel (chan EpisodicRecord, cap 256)                             │
 │  - Pure-Go SQLite Store (modernc.org/sqlite, WAL mode)                                    │
 │  - Structured slog & Cost Metrics Exporter                                                │
 └───────────────────────────────────────────────────────────────────────────────────────────┘
```

### 3.1 SDK Migration & Prompt Cache Optimization (`internal/agent`)
* **Streaming Delta Accumulator:** Streaming chunks from `client.Chat.Completions.NewStreaming` deliver partial tool call deltas. An in-flight accumulator maps `delta.ToolCalls[i].Index` to reconstruct the full function name and arguments JSON before execution.
* **Deterministic Lexicographical Canonicalization:**
  1. All tools (native and MCP) are sorted alphabetically by their final identifier (`bash`, `edit_file`, `mcp__postgres__query`, `read_file`, `write_file`).
  2. Nested JSON Schema parameter maps are recursively sorted by key.
  This ensures byte-for-byte prefix identity, maximizing OpenAI KV prompt cache hits across multi-turn sessions.

### 3.2 Process-Safe Subprocess & MCP Supervisor (`internal/mcp` & `internal/tools`)
* **Process-Group Containment:**
  ```go
  cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
  // Cancellation sends SIGKILL to the entire process group:
  _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
  ```
  Windows builds are separated using build tags (`bash_posix.go` and `bash_windows.go`).
* **Two-Tier Timeouts:**
  - *Discovery Timeout (10s):* Spawning an MCP server and calling `ListTools` is bound to 10 seconds. If exceeded, the server is marked `degraded` and the agent proceeds with remaining tools.
  - *Execution Timeout (60s):* Tool calls have strict per-invocation deadlines.
* **Single-Threaded Serialization:** To prevent buffer corruption on single-threaded MCP implementations (Node.js/Python), `MCPManager` maintains a `sync.Mutex` per server instance to serialize concurrent JSON-RPC requests across the stdio pipe.

### 3.3 Three-Partition Compactor & Anti-Amnesia Ledger (`internal/context`)
When total tokens exceed 75% of the model window limit:
1. **Immutable Anchor:** Retained verbatim (System Prompt, `SOUL.md`, `USER.md`, original task prompt).
2. **Accumulator Ledger:** An LLM compression pass converts turns older than the sliding window into an invariant-preserving Markdown schema:
   * `### Modified Files`: List of changed files and applied diff summaries.
   * `### Discovered Constraints`: Compiler error messages, missing packages, environment realities.
   * `### Discarded Hypotheses`: Approaches that failed and why (eliminating retry loops).
3. **Sliding Window:** The 6 most recent interaction turns are kept verbatim.
4. **Spill-to-Disk Handler:** Any tool output exceeding 64KB writes its full payload to `.buddy/spills/<uuid>.log`, returning a 2KB preview and file pointer to the model.

### 3.4 Episodic SQLite Store with Single-Writer Actor (`internal/memory`)
* **Zero CGO:** Uses `modernc.org/sqlite` with `PRAGMA journal_mode = WAL` and `PRAGMA busy_timeout = 5000`.
* **Single-Writer Actor:**
  ```go
  type SQLiteActor struct {
      recordCh chan EpisodicRecord // cap 256
      db       *sql.DB
  }
  ```
  All writes (CLI turns, background cron metrics, subagent summaries) post to `recordCh`. A dedicated single goroutine executes batch inserts, eliminating `SQLITE_BUSY` errors under concurrency.

### 3.5 Bounded Task Delegation (`internal/agent/subagent.go`)
* **Depth Limit = 1:** The `delegate_task` tool uses an agent factory that instantiates a child `Engine` with `delegate_task` omitted from its tool registry. Grandchild agents cannot be spawned.
* **Budget Slicing:** The parent allocates a dedicated step limit (default 10 steps).
* **Context Boundary:** When the subagent terminates, child turns are discarded. Only a structured summary (`status`, `findings`, `modified_files`) is returned as the tool response to the parent.

### 3.6 Dual Execution Topologies & Daemon IPC (`internal/daemon`)
* **Ephemeral Runner (`cmd/buddy/run.go`):** One-shot execution. Detects if `os.Stdin` is a pipe to support `cat error.log | buddy run "Explain"` and emits standard Unix exit codes.
* **Background Daemon (`cmd/buddy/daemon.go`):**
  - Manages background cron schedules (`cron_job`).
  - Listens on a local Unix domain socket (`~/.buddy/daemon.sock`, permission `0600`) for CLI status checks and task triggers.

---

## 4. Alternatives Considered

### 4.1 Third-Party Community Libraries vs. Official First-Party SDKs
* **Alternative:** Continue utilizing `sashabaranov/go-openai` and `mark3labs/mcp-go`.
* **Decision:** *Rejected by author.* Community libraries suffer from protocol version lag and divergence from upstream specifications (such as Structured Outputs and real-time MCP framing updates). Standardizing on `openai/openai-go` and `modelcontextprotocol/go-sdk` provides long-term stability, direct upstream vendor support, and first-party bug fixes.

### 4.2 Freeform Prose Summarization vs. Semantic Delta Accumulator
* **Alternative:** Prompting an LLM to generate unstructured prose summaries of conversation history when compacting.
* **Decision:** *Rejected by author.* Prose summaries consistently discard technical details like modified line numbers, compiler error codes, and exact file paths. This results in repetitive hallucination loops where the agent retries failed actions. Enforcing a structured Markdown schema (`Modified Files`, `Discovered Constraints`, `Discarded Hypotheses`) guarantees essential operational state survives compaction.

### 4.3 Unconstrained Recursive Subagents vs. Bounded Depth = 1
* **Alternative:** Permitting subagents to call `delegate_task` to spawn arbitrary child trees.
* **Decision:** *Rejected by author.* Unbounded nesting introduces fork-bombs, runaway token consumption, and fragile child process lifecycle management. Bounding subagents to depth = 1 solves 95% of task isolation needs (e.g., deep file inspection or isolated test runs) with zero risk of recursive process explosions.

### 4.4 Direct Parent Process Termination vs. POSIX Process Groups
* **Alternative:** Standard `exec.CommandContext` without process group flags.
* **Decision:** *Rejected by author.* Calling `cmd.Process.Kill()` terminates only the parent shell when commands invoke wrappers (such as `npx`, `uvx`, or bash pipelines), leaving orphaned background processes running on the host. Setting `SysProcAttr: &syscall.SysProcAttr{Setpgid: true}` and signaling the negative PID (`-cmd.Process.Pid`) ensures the entire process subtree is terminated reliably.

### 4.5 Direct Concurrent SQLite Writes vs. Single-Writer Actor
* **Alternative:** Allowing all goroutines (daemon cron, CLI tasks, subagents) to write directly to SQLite using standard connection pools.
* **Decision:** *Rejected by author.* Under concurrent workloads, SQLite produces `SQLITE_BUSY` (database is locked) errors. Channeling all write operations through a single-writer actor goroutine eliminates write contention entirely while WAL mode permits concurrent, non-blocking reads.

---

## 5. Detailed Implementation Plan

### 5.1 Single Implementation Sequence

The implementation is sequenced across five sequential phases to ensure non-breaking verification at each milestone:

```
Phase 1: SDK Modernization & Deterministic Tool Schemas
  ↳ Phase 2: Process-Safe Subprocess Supervision & MCP Bridge
      ↳ Phase 3: Anti-Amnesia Context Compactor & Episodic SQLite Actor
          ↳ Phase 4: Bounded Task Delegation & Daemon Unix Socket IPC
              ↳ Phase 5: Structured Error Taxonomy & Benchmark Eval Suite
```

### 5.2 Complete File Enumeration & Modification Rationales

#### 1. Dependencies & Configuration
* **[`go.mod`](file:///home/hoangndst/Server/buddy-agent/go.mod) & [`go.sum`](file:///home/hoangndst/Server/buddy-agent/go.sum)**
  * *Change:* Add `github.com/openai/openai-go`, `github.com/modelcontextprotocol/go-sdk`, `modernc.org/sqlite`. Remove `github.com/sashabaranov/go-openai`.
  * *Rationale:* Standardizes harness on official first-party SDKs and pure-Go SQLite for zero-CGO static builds.
* **[`internal/config/config.go`](file:///home/hoangndst/Server/buddy-agent/internal/config/config.go)**
  * *Change:* Add configuration loaders for `MCPConfigPath` (default `~/.buddy/mcp.json`), `DaemonSocketPath` (default `~/.buddy/daemon.sock`), and `SpillDir` (default `.buddy/spills`).
  * *Rationale:* Externalizes runtime paths for daemon socket and spill storage.

#### 2. Agent Core & SDK Integration (`internal/agent`)
* **[`internal/agent/types.go`](file:///home/hoangndst/Server/buddy-agent/internal/agent/types.go)**
  * *Change:* Refactor `ChatClient` interface and engine events to consume `openai-go` request and message parameter types (`openai.ChatCompletionNewParams`).
  * *Rationale:* Decouples ReAct engine from concrete HTTP transports and supports offline mock testing with the official SDK types.
* **[`internal/agent/accumulator.go`](file:///home/hoangndst/Server/buddy-agent/internal/agent/accumulator.go)** *(New File)*
  * *Change:* Implements `ToolCallAccumulator` assembling fragmented streaming deltas (`delta.ToolCalls[i]`) into complete JSON payloads.
  * *Rationale:* Prevents JSON unmarshaling errors during streaming tool call execution.
* **[`internal/agent/engine.go`](file:///home/hoangndst/Server/buddy-agent/internal/agent/engine.go)**
  * *Change:* Refactor ReAct loop to call `client.Chat.Completions.New` with official SDK params, dispatch through accumulator, and record episodic turns.
  * *Rationale:* Migrates primary reasoning loop to the official OpenAI SDK.
* **[`internal/agent/subagent.go`](file:///home/hoangndst/Server/buddy-agent/internal/agent/subagent.go)** *(New File)*
  * *Change:* Implements `SubagentRunner` instantiating isolated child `Engine` instances with depth=1 guard and sub-budget ceilings.
  * *Rationale:* Executes delegated subtasks in clean contexts without polluting parent message history.

#### 3. Tool Engine & Process Groups (`internal/tools`)
* **[`internal/tools/registry.go`](file:///home/hoangndst/Server/buddy-agent/internal/tools/registry.go)**
  * *Change:* Add recursive lexicographical key sorting for JSON Schema maps and alphabetical tool ordering in `OpenAITools()`.
  * *Rationale:* Preserves exact byte-for-byte prefix identity for provider KV prompt caching.
* **[`internal/tools/bash_posix.go`](file:///home/hoangndst/Server/buddy-agent/internal/tools/bash_posix.go)** *(New File, `//go:build !windows`)*
  * *Change:* Implements `execPOSIXProcess` configuring `SysProcAttr: &syscall.SysProcAttr{Setpgid: true}` and group termination via `syscall.Kill(-pid, syscall.SIGKILL)`.
  * *Rationale:* Eliminates orphaned background processes spawned by shell wrappers on Unix systems.
* **[`internal/tools/bash_windows.go`](file:///home/hoangndst/Server/buddy-agent/internal/tools/bash_windows.go)** *(New File, `//go:build windows`)*
  * *Change:* Implements Windows fallback process termination using job objects or taskkill.
  * *Rationale:* Prevents compilation failures on Windows while preserving process termination safety.
* **[`internal/tools/bash.go`](file:///home/hoangndst/Server/buddy-agent/internal/tools/bash.go)**
  * *Change:* Refactor to delegate OS execution to `bash_posix.go` or `bash_windows.go`.
  * *Rationale:* Separates OS-specific process-group logic from generic bash argument validation.
* **[`internal/tools/delegate.go`](file:///home/hoangndst/Server/buddy-agent/internal/tools/delegate.go)** *(New File)*
  * *Change:* Implements `delegate_task` tool exposing task delegation to the parent model.
  * *Rationale:* Enables parent models to offload deep investigation to isolated child harnesses.

#### 4. Model Context Protocol Bridge (`internal/mcp`)
* **[`internal/mcp/manager.go`](file:///home/hoangndst/Server/buddy-agent/internal/mcp/manager.go)** *(New File)*
  * *Change:* Implements `MCPManager` supervising subprocess lifecycles, two-tier timeouts (10s discovery, 60s execution), restart caps, and clean `session.Close()` teardown.
  * *Rationale:* Manages external MCP server processes with fault isolation.
* **[`internal/mcp/client.go`](file:///home/hoangndst/Server/buddy-agent/internal/mcp/client.go)** *(New File)*
  * *Change:* Implements client bridge over `modelcontextprotocol/go-sdk`, mapping MCP tools into the internal `tools.Tool` interface with `<server>__<tool>` namespacing.
  * *Rationale:* Connects external JSON-RPC stdio servers to the unified tool registry.

#### 5. Context Compaction & Spill Handling (`internal/context`)
* **[`internal/context/compactor.go`](file:///home/hoangndst/Server/buddy-agent/internal/context/compactor.go)** *(New File)*
  * *Change:* Implements three-partition compactor generating the structured Semantic Delta Accumulator (`Modified Files`, `Discovered Constraints`, `Discarded Hypotheses`).
  * *Rationale:* Prevents amnesia and cyclic retry loops during long-running tasks.
* **[`internal/context/spill.go`](file:///home/hoangndst/Server/buddy-agent/internal/context/spill.go)** *(New File)*
  * *Change:* Implements payload spill manager storing tool outputs >64KB to `.buddy/spills/<uuid>.log` and returning 2KB previews.
  * *Rationale:* Protects token context windows from massive terminal logs or SQL query dumps.

#### 6. Three-Tier Memory & SQLite Actor (`internal/memory`)
* **[`internal/memory/sqlite_store.go`](file:///home/hoangndst/Server/buddy-agent/internal/memory/sqlite_store.go)** *(New File)*
  * *Change:* Pure-Go SQLite schema initialization (`sessions`, `turns`, `tool_executions`) with WAL mode.
  * *Rationale:* Provides durable episodic history and audit trail without CGO requirements.
* **[`internal/memory/actor.go`](file:///home/hoangndst/Server/buddy-agent/internal/memory/actor.go)** *(New File)*
  * *Change:* Implements single-writer channel actor (`chan EpisodicRecord`, capacity 256) executing serialized SQLite writes.
  * *Rationale:* Guarantees zero `SQLITE_BUSY` errors under concurrent daemon and subagent execution.

#### 7. Daemon Service & Scheduled Scheduler (`internal/daemon`)
* **[`internal/daemon/scheduler.go`](file:///home/hoangndst/Server/buddy-agent/internal/daemon/scheduler.go)** *(New File)*
  * *Change:* Implements ticker-based cron runner executing scheduled tasks in the background.
  * *Rationale:* Supports periodic maintenance, repo health checks, and scheduled evaluation runs.
* **[`internal/daemon/socket.go`](file:///home/hoangndst/Server/buddy-agent/internal/daemon/socket.go)** *(New File)*
  * *Change:* Implements Unix domain socket server (`~/.buddy/daemon.sock`, permission `0600`) for CLI status checks and task triggers.
  * *Rationale:* Provides secure, local IPC between the ephemeral CLI and background daemon without exposing TCP ports.

#### 8. Entrypoints & CLI Topologies (`cmd/buddy`)
* **[`cmd/buddy/run.go`](file:///home/hoangndst/Server/buddy-agent/cmd/buddy/run.go)** *(New File)*
  * *Change:* Implements `buddy run "<prompt>"` one-shot command with `os.Stdin` pipeline detection and semantic exit codes.
  * *Rationale:* Supports command-line scripting and UNIX pipeline composition.
* **[`cmd/buddy/daemon.go`](file:///home/hoangndst/Server/buddy-agent/cmd/buddy/daemon.go)** *(New File)*
  * *Change:* Implements `buddy daemon` service subcommand with signal handling and graceful socket cleanup.
  * *Rationale:* Hosts long-running background cron runners and socket listeners.
* **[`cmd/buddy/main.go`](file:///home/hoangndst/Server/buddy-agent/cmd/buddy/main.go)**
  * *Change:* Refactor root command into a router dispatching subcommands (`run`, `daemon`, `repl`, `eval`).
  * *Rationale:* Unifies CLI commands under a clean CLI taxonomy.

#### 9. Verification & Unit Test Suite
* **[`internal/agent/engine_test.go`](file:///home/hoangndst/Server/buddy-agent/internal/agent/engine_test.go)**: Update mock client to verify `openai-go` delta assembly and ReAct execution.
* **[`internal/agent/subagent_test.go`](file:///home/hoangndst/Server/buddy-agent/internal/agent/subagent_test.go)** *(New File)*: Verify child subagents cannot call `delegate_task` (depth=1 enforcement) and enforce sub-budget cutoffs.
* **[`internal/tools/registry_test.go`](file:///home/hoangndst/Server/buddy-agent/internal/tools/registry_test.go)**: Test deterministic schema key canonicalization and alphabetical ordering.
* **[`internal/tools/bash_test.go`](file:///home/hoangndst/Server/buddy-agent/internal/tools/bash_test.go)**: Verify process group termination kills child background processes on timeout.
* **[`internal/context/compactor_test.go`](file:///home/hoangndst/Server/buddy-agent/internal/context/compactor_test.go)** *(New File)*: Test that the Semantic Delta Accumulator preserves file lists and error constraints across compaction.
* **[`internal/context/spill_test.go`](file:///home/hoangndst/Server/buddy-agent/internal/context/spill_test.go)** *(New File)*: Test that payloads >64KB spill to disk and return formatted preview notices.
* **[`internal/memory/actor_test.go`](file:///home/hoangndst/Server/buddy-agent/internal/memory/actor_test.go)** *(New File)*: Test 50 concurrent goroutines writing episodic turns to SQLite without lock contention.
* **[`internal/mcp/manager_test.go`](file:///home/hoangndst/Server/buddy-agent/internal/mcp/manager_test.go)** *(New File)*: Test 10s discovery timeout and namespaced tool registration.
