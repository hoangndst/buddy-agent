# Milestone 7: Bounded Subagent Delegation (depth=1) and Execution Budgets

* **Author:** hoangndst
* **Date:** 2026-11-20
* **Status:** Approved
* **Target Path:** `docs/designs/07_subagent_orchestration.md`
* **Artifact Path:** `docs/designs/07_subagent_orchestration.md`

---

## 1. Problem Statement

Complex developer workflows frequently exhaust single-agent context windows and degrade LLM reasoning fidelity:
1. **Context Window Flooding from Exploratory Output**:
   Deep codebase exploration (e.g. running broad directory searches, inspecting build dependency graphs, reading test fixtures, or parsing large log outputs) consumes large quantities of context tokens. In a single-agent architecture, all intermediate tool results must remain in the conversation history, triggering rapid context compaction, micro-pruning, and eventually premature context exhaustion.
2. **Context Dilution and Goal Drift**:
   When an agent must interleave high-level planning with low-level diagnostic grep queries in the same context, the model's attention is divided across hundreds of irrelevant lines of raw text. This increases hallucination rates, degrades reasoning accuracy, and inflates API billing costs.
3. **Missing Delegation Boundaries**:
   Without subagents, the primary engine must execute all operations directly. There is no mechanism to delegate an isolated investigation, receive a distilled synthesis, and proceed with a clean, focused context.
4. **Unmitigated Risks of Unbounded Delegation**:
   Naive multi-agent architectures introduce failure modes:
   * *Recursive Fork Bombs*: Agents spawning agents without depth limits, creating exponential API spend and cascading thread allocation.
   * *Security Escapes*: Child agents inheriting dangerous tools (such as arbitrary shell execution or persistent memory modification) while running unmonitored sub-tasks.
   * *Disappearing Work on Timeout*: If a subagent times out or exhausts its step limit, intermediate discoveries are lost if the engine returns an empty failure string.

Milestone 7 resolves these challenges by introducing a bounded, in-process subagent delegation system strictly enforced at `depth=1` with static role gating, hierarchical SQLite session tracking, token metric decoupling, and reserve-step exhaustion synthesis.

---

## 2. Overview & Core Architectural Principles

Milestone 7 extends `buddy-agent` with bounded subagent delegation governed by seven architectural invariants:

1. **Strict Depth Ceiling (`depth=1`) via Tool Omission**:
   - The root engine executes at `depth=0`.
   - Delegated subagents execute strictly at `depth=1`.
   - Bounded depth is enforced at compile and registration time: `invoke_subagent` is strictly omitted from all child tool registries. A child agent possesses no delegation tool and cannot spawn further subagents under any circumstance.
2. **In-Process Synchronous Execution with Panic Cushioning**:
   - Subagents execute as in-process Go instances sharing the parent process's memory space, client connection pools, and SQLite database handle.
   - The parent engine pauses synchronously during `invoke_subagent.Execute(ctx)` while the child executes its dedicated ReAct loop.
   - Child execution runs within an explicit `defer / recover` boundary that intercepts runtime panics and converts them into structured failure outputs without crashing the parent host process.
3. **Static Role Whitelists (Elimination of "Read-Only Bash")**:
   - Two immutable role envelopes govern subagent tool permissions:
     * `researcher`: Read-only file inspection and querying (`read_file`, directory traversal, grep, read-only MCP tools). `bash` is completely excluded, eliminating the security vulnerability of attempting to filter arbitrary shell grammar.
     * `worker`: Full development toolset (`bash`, `read_file`, `write_file`, `edit_file`, MCP tools).
   - Administrative and recursive tools (`invoke_subagent`, `save_memory`, `create_skill`) are barred from all subagents, preventing memory poisoning and skill contamination.
4. **StateTracker Invariant Inheritance & Clean State Rollup**:
   - Subagents inherit a deep copy of the parent's `Invariants` list, ensuring child agents adhere to project-wide coding standards and constraints.
   - Subagents initialize with a clean slate: empty `ModifiedFiles` map and empty `RecentErrors` queue.
   - Upon successful completion of a `worker` subagent, modified file paths merge into the parent's `StateTracker.ModifiedFiles`.
5. **Reserve-Step Exhaustion Synthesis**:
   - When a child agent reaches its step budget (`step == totalMaxSteps`) without finishing, the engine does not abort with an empty string.
   - Instead, the engine reserves the final step for an automated non-tool synthesis prompt, compelling the model to summarize partial findings and return them to the parent with status `budget_exhausted`.
6. **Token Accounting Decoupling**:
   - Child token consumption (prompt, completion, and reasoning tokens) accumulates into the SQLite database billing ledger for the parent session.
   - Child tokens NEVER calibrate the parent's `history.Session` token estimator or affect parent context window watermarks, preventing premature parent context compaction.
7. **Hierarchical SQLite Persistence via Migration 002**:
   - SQLite `sessions` table is expanded with `parent_session_id`, `role`, and `depth`.
   - An `AFTER DELETE` trigger `trg_cascade_delete_sessions` guarantees clean cascading deletion of child sessions and their associated turns upon parent session deletion, resolving the SQLite restriction on self-referential `ALTER TABLE` foreign keys.

---

## 3. System Architecture & Concurrency Contract

```
+---------------------------------------------------------------------------------------+
|                                    Parent Process                                     |
|                                                                                       |
|  +---------------------------------------------------------------------------------+  |
|  |                            Parent Engine (depth=0)                              |  |
|  |  - Active Context: history.Session (Parent working tail)                        |  |
|  |  - StateTracker: Invariants, Global Touched Files, Recent Errors                |  |
|  |  - ToolRegistry: [write_file, edit_file, bash, memory_tools, invoke_subagent]     |  |
|  +---------------------------------------+-----------------------------------------+  |
|                                          |                                            |
|                       Turn Step: Model invokes tool:                                  |
|                       invoke_subagent(task="...", role="researcher", max_steps=10)    |
|                                          |                                            |
|                                          v                                            |
|  +---------------------------------------------------------------------------------+  |
|  |                     InvokeSubagentTool.Execute(parentCtx)                       |  |
|  |  1. Check horizontal throttle (invocations <= max_subagent_calls: 5)            |  |
|  |  2. Construct child tool registry (omit invoke_subagent, apply role whitelist)  |  |
|  |  3. Construct subagent system prompt (non-interactive, factual synthesis)       |  |
|  |  4. Derive child context: context.WithTimeout(parentCtx, childTimeout: 120s)   |  |
|  |  5. Wrap execution in defer recover() boundary                                  |  |
|  +---------------------------------------+-----------------------------------------+  |
|                                          |                                            |
|                       Synchronous in-process execution:                               |
|                                          |                                            |
|                                          v                                            |
|  +---------------------------------------------------------------------------------+  |
|  |                             Child Engine (depth=1)                              |  |
|  |  - Fresh history.Session: Isolated context window (0 tokens at start)           |  |
|  |  - Isolated StateTracker: Inherited Invariants, empty ModifiedFiles/Errors      |  |
|  |  - Filtered Registry: [read_file, mcp_read] (Zero bash, zero delegation)        |  |
|  |  - ReAct Turn Loop: Runs up to max_steps (default: 10)                          |  |
|  |  - Budget Cushioning: On step 10, forces emergency synthesis if uncompleted     |  |
|  +---------------------------------------+-----------------------------------------+  |
|                                          |                                            |
|                      Returns: SubagentResult struct                                   |
|                                          |                                            |
|                                          v                                            |
|  +---------------------------------------------------------------------------------+  |
|  |                            Parent Turn Resolution                               |  |
|  |  1. Merge child ModifiedFiles into parent StateTracker (if worker)              |  |
|  |  2. Add child token usage to parent DB billing record                           |  |
|  |  3. Do NOT add child tokens to parent history.Session token estimator          |  |
|  |  4. Format synthesized report as tool result string in parent turn block        |  |
|  +---------------------------------------------------------------------------------+  |
|                                                                                       |
+---------------------------------------------------------------------------------------+
```

### 3.1 Concurrency & Execution Flow
1. **Calling Thread Blocking**: When the parent engine generates a tool call for `invoke_subagent`, `tools.Registry.Dispatch` calls `InvokeSubagentTool.Execute`. The parent engine goroutine pauses execution while awaiting the tool result.
2. **Synchronous Execution**: The child engine executes directly within `Execute` (or in a monitored worker goroutine with a channel completion join). There is no out-of-process IPC serialization, standard input/output piping, or duplicate process initialization.
3. **Database Concurrency**:
   - Both parent and child engines share the single `*persistence.SQLiteStore` instance.
   - Because parent execution is suspended while waiting for the child tool result, parent and child turn commits to `writerDB` are naturally serialized across time.
   - Write operations acquire `s.mu.Lock()` sequentially without contention.

---

## 4. Subagent Tool Permission Model & Static Role Whitelists

### 4.1 Rejection of "Read-Only Bash"
In Linux and Unix environments, executing `/bin/bash -c <cmd>` cannot be restricted to read-only operations via command blacklists or regular expressions. Shell features such as output redirection (`echo data > file.txt`), destructive parameters (`find . -delete`), in-line scripting (`python3 -c "import os; os.remove(...)"`), process substitution, and source control operations (`git clean -fd`) bypass string filters trivially.

True operating system sandboxing requires Landlock LSM, seccomp filters, or Linux container namespaces, which violate our strict requirement for pure-Go, zero-CGO cross-platform compatibility across Linux, macOS, and Windows.

Consequently, `buddy-agent` eliminates shell access entirely from read-only subagents.

### 4.2 Role Definitions
Tools are allocated according to static, immutable role definitions:

| Tool | Root Engine (`depth=0`) | Subagent: `researcher` (`depth=1`) | Subagent: `worker` (`depth=1`) |
| :--- | :---: | :---: | :---: |
| `read_file` | Allowed | Allowed | Allowed |
| MCP Inspection Tools | Allowed | Allowed | Allowed |
| `write_file` | Allowed | **Blocked** | Allowed |
| `edit_file` | Allowed | **Blocked** | Allowed |
| `bash` | Allowed | **Blocked** | Allowed |
| MCP Mutation Tools | Allowed | **Blocked** | Allowed |
| `invoke_subagent` | Allowed | **Blocked** (Compile-time) | **Blocked** (Compile-time) |
| `save_memory` | Allowed | **Blocked** (Admin filter) | **Blocked** (Admin filter) |
| `create_skill` | Allowed | **Blocked** (Admin filter) | **Blocked** (Admin filter) |

### 4.3 Administrative Tool Exclusion
Subagents must never mutate the host agent's durable knowledge base:
* `save_memory`: Prohibited. If a subagent modifies `.buddy/memory/notes/`, those notes are reloaded into the parent system anchor upon restart, creating a memory poisoning vulnerability.
* `create_skill`: Prohibited. Subagents cannot create or edit files in `skills/`.

---

## 5. StateTracker Partitioning & State Rollup

### 5.1 Invariant Inheritance
When initializing the child engine's `history.StateTracker`:
* `RootGoal`: Initialized to the subagent task string specified in `invoke_subagent` arguments.
* `Invariants`: Deep copied from `parentStateTracker.Invariants`. The child inherits all active project rules (e.g. "pure Go zero CGO", "unit tests required").
* `ModifiedFiles`: Initialized to a fresh, empty `map[string]FileRecord`.
* `RecentErrors`: Initialized to a fresh, empty `[]string` slice.

### 5.2 State Rollup Contract
When the child finishes:
1. If the child role is `worker` and `status == "completed"`:
   - For every entry in `childStateTracker.ModifiedFiles`, the orchestrator calls:
     ```go
     parentStateTracker.RecordFileAccess(record.Path, record.Action)
     ```
   - This ensures that subsequent parent compaction passes retain knowledge of files modified by the subagent.
2. If the child role is `researcher` or execution failed:
   - File rollup is skipped.
3. Child error log entries (`RecentErrors`) remain internal to the child. The synthesized final response captures any relevant error messages for the parent without polluting the parent error buffer.

---

## 6. Execution Budgets, Horizontal Throttling & Reserve-Step Synthesis

### 6.1 Execution Budgets
The parent engine enforces strict ceilings on subagent resource consumption:
* **Step Limit (`max_steps`)**: Default: 10 steps. Maximum permitted ceiling: 15 steps.
* **Execution Timeout (`timeout`)**: Default: 120 seconds. Maximum permitted ceiling: 300 seconds. Governed via `context.WithTimeout(parentCtx, timeout)`.
* **Context Token Ceilings**: Child engines instantiate with a dedicated `history.Session` capped at model default limits (e.g. 64,000 tokens). Because child steps are bounded to 10-15 steps, context exhaustion is rare.

### 6.2 Horizontal Delegation Throttling
To prevent infinite delegation loops where a parent continuously spawns subagents sequentially without making progress:
* The parent engine tracks `subagentCallCount` per root session.
* A hard ceiling of `MaxSubagentCalls = 5` is enforced.
* If the parent attempts a 6th subagent invocation, `InvokeSubagentTool` immediately rejects the call:
  `"Maximum subagent delegation quota reached (5/5). You must synthesize existing findings and complete the task directly."`

### 6.3 Reserve-Step Budget Exhaustion Synthesis
In the current engine, reaching `totalMaxSteps` results in an immediate failure:
```go
maxStepErr := fmt.Errorf("reached maximum step budget (%d) without finishing", totalMaxSteps)
return "", maxStepErr
```
If a subagent executes 10 research turns and hits the budget ceiling, all findings are lost.

Milestone 7 introduces **Reserve-Step Cushioning**:
1. When `step == totalMaxSteps` and the model has not produced a final response:
   - The engine strips all tools from the completion request.
   - The engine injects an emergency synthesis user prompt:
     `"You have reached your maximum step budget. Do not call any tools. Provide a concise, complete synthesis of your findings, touched files, and any unresolved blockers based on your execution so far."`
2. The engine executes a single non-tool completion request.
3. If the model returns text, the engine:
   - Sets session status to `persistence.StatusFailed` (or dedicated `budget_exhausted`).
   - Returns the synthesized text alongside a clear status prefix:
     `"[Subagent: budget_exhausted] Summary of partial progress: ..."`
4. The parent receives the analytical output of the research rather than an empty failure.

---

## 7. Token Accounting Decoupling

Subagent token accounting requires strict separation between billing metrics and context window estimation:

### 7.1 Billing Rollup
* Child engines record their cumulative token consumption (`PromptTokens`, `CompletionTokens`, `TotalTokens`).
* Upon child completion, child token totals are added directly into the database record of the parent session:
  ```go
  _ = store.IncrementSessionTokens(ctx, parentSessionID, childTokens)
  ```
* This ensures that `buddy session show <parent-id>` displays the true total token cost of the entire operation.

### 7.2 Context Window Decoupling
* Child token usage must **NEVER** be passed to `parentSession.CalibrateUsage(...)` or added to parent context token estimators.
* The parent's `history.Session` context window only absorbs the tokens of the synthesized response string returned by `invoke_subagent`.
* This prevents artificial token spikes from triggering premature and aggressive micro-pruning or compaction in the parent context.

---

## 8. SQLite Hierarchical Storage & Migration 002

### 8.1 Schema Migration 002
SQLite does not allow adding self-referential foreign keys via `ALTER TABLE ADD COLUMN` statements (`ALTER TABLE sessions ADD COLUMN parent_session_id TEXT REFERENCES sessions(id)` fails with an error).

Migration 002 resolves this using standard column addition combined with an `AFTER DELETE` trigger:

```sql
-- Migration 002: Hierarchical subagent sessions
ALTER TABLE sessions ADD COLUMN parent_session_id TEXT;
ALTER TABLE sessions ADD COLUMN role TEXT NOT NULL DEFAULT 'root';
ALTER TABLE sessions ADD COLUMN depth INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id);
CREATE INDEX IF NOT EXISTS idx_sessions_depth ON sessions(depth);

CREATE TRIGGER IF NOT EXISTS trg_cascade_delete_sessions
AFTER DELETE ON sessions
FOR EACH ROW
BEGIN
    DELETE FROM sessions WHERE parent_session_id = OLD.id;
END;
```

### 8.2 Session Hierarchy Inspection
1. **Root Session List**:
   - `buddy session list` filters by `WHERE depth = 0` by default, keeping child subagent sessions from cluttering the primary CLI view.
   - A `--all` flag allows inspecting all sessions including subagents.
2. **Session Detail**:
   - `buddy session show <parent-id>` displays the parent trajectory. When a subagent call occurred, it displays the tool invocation, child session ID, role, and summary.
   - Running `buddy session show <child-id>` displays the child agent's detailed turn-by-turn trajectory.
3. **Cascading Deletion**:
   - Deleting or pruning a parent session automatically activates `trg_cascade_delete_sessions`, which deletes all child session rows.
   - Child turns and child snapshots are automatically deleted via their existing foreign key constraints (`REFERENCES sessions(id) ON DELETE CASCADE`).

---

## 9. Subagent System Prompt & Terminal UX Formatting

### 9.1 Non-Interactive System Prompt
Child engines do not use the conversational REPL prompt (`SOUL.md`). Instead, subagents receive a dedicated, non-interactive system prompt:

```markdown
You are a specialized subagent operating under the supervision of a primary agent.
Your role is: {{.Role}}
Your task is: {{.Task}}

Guidelines:
1. Focus strictly on executing the assigned task within your tool permissions.
2. Do not attempt conversational pleasantries or ask clarifying questions.
3. When finished, provide a concise, factual synthesis of your findings, actions taken, and touched files.
4. If you encounter blockers or cannot complete the task, state the exact reason clearly.
```

### 9.2 Indented Terminal Event Formatting
To prevent child tool execution output from interleaving confusingly with parent stdout:
1. Child step events are decorated with two-space indentation and a role badge:
   ```
   ⚙ [Step 4] invoke_subagent: researcher (Task: Find all token counters)
     │ ⚙ read_file: internal/history/estimator.go
     │ ⚙ read_file: internal/history/session.go
     └─ ✓ Subagent finished (2 steps, 1,420 tokens)
   ```
2. Low-level reasoning tokens from child completions are suppressed from terminal stdout to maintain high-level readability for the developer.

---

## 10. Failure Modes, Panic Isolation & Edge Cases

| Failure Mode | Root Cause | Architectural Mitigation |
| :--- | :--- | :--- |
| **Child Engine Panic** | Nil pointer dereference or tool runtime panic inside child execution. | `InvokeSubagentTool.Execute` wraps invocation in a `defer / recover()` block. Intercepts panic, updates child session to `failed`, and returns structured error text to parent. |
| **User Abort (`Ctrl+C`)** | Developer presses `Ctrl+C` while subagent is running. | `parentCtx.Done()` fires. Cancellation propagates immediately to child context via standard Go context hierarchy. Engine records `interrupted` status and terminates. |
| **Child Timeout Exceeded** | Subagent executes slow network or tool operations exceeding timeout (120s). | `childCtx.Err() == context.DeadlineExceeded`. Child terminates, updates child session to `failed`, and returns `Status: budget_exhausted` tool output to parent. Parent ReAct loop does NOT crash. |
| **Horizontal Delegation Thrashing** | Parent agent gets stuck in a reasoning loop invoking subagents repeatedly. | Hard limit enforced: maximum 5 subagent calls per parent session. Subsequent calls fail immediately. |
| **Recursive Delegation Attempt** | Malicious prompt or model hallucination tries to call `invoke_subagent` from child. | `invoke_subagent` is physically omitted from child tool registry. Model receives standard "unknown tool" error if attempted. |
| **SQLite Writer Lock Contention** | Multiple engines attempting concurrent database writes. | Parent engine pauses while child runs, serializing writes. Database pool uses `writerDB.SetMaxOpenConns(1)` with Go mutex `s.mu.Lock()`. |

---

## 11. Alternatives Weighed & Rejected

1. **Subprocess CLI Execution (`buddy run --child`)**:
   - *Rejected*: Incurs 150ms-300ms startup latency per subagent (binary bootstrap, environment parsing, SQLite connection opening, migration validation). Introduces SQLite WAL file lock contention between separate OS processes and requires serialized JSON IPC over pipes.
2. **"Read-Only Bash" via Shell Command Blacklists**:
   - *Rejected*: Shell grammar allows trivial breakouts (`echo >`, `rm`, `sed -i`, python scripts, git mutations). Filtering shell text provides an illusion of security without real isolation. Banning `bash` from `researcher` completely is the only secure approach.
3. **Dynamic Tool Whitelists Provided by Parent LLM**:
   - *Rejected*: Allowing the parent LLM to dynamically configure tool permissions introduces prompt injection vulnerabilities. Static, immutable roles (`researcher`, `worker`) guarantee deterministic security boundaries.
4. **Inheriting Full Parent StateTracker (`ModifiedFiles` and `RecentErrors`)**:
   - *Rejected*: Passing the parent's full file history and error log to the child consumes prompt tokens unnecessarily and biases the child's focus away from its dedicated sub-task.

---

## 12. Implementation Plan & Acceptance Criteria

### Phase 1: Database Migration 002
* [ ] Implement Migration 002 in `internal/persistence/sqlite.go`:
  - Add `parent_session_id`, `role`, and `depth` columns to `sessions`.
  - Add indices `idx_sessions_parent` and `idx_sessions_depth`.
  - Add trigger `trg_cascade_delete_sessions`.
  - Bump `PRAGMA user_version` to 2.
* [ ] Update `persistence.SessionRecord` to include `ParentSessionID`, `Role`, and `Depth`.
* [ ] Add unit test verifying that deleting a parent session deletes all associated child sessions and child turns.

### Phase 2: Engine Reserve Synthesis & Subagent Runner
* [ ] Update `internal/agent/engine.go` execution loop:
  - Detect when `step == totalMaxSteps` and tool calls remain incomplete.
  - Execute a reserve non-tool completion prompt to generate an emergency synthesis before returning.
  - Return synthesized content with error indication if step limit is reached.
* [ ] Add `internal/agent/subagent.go`:
  - Implement `SubagentRunner` struct with `RunSubagent(ctx, task, role, parentSessionID, budget)`.
  * Wrap child execution in `defer recover()` to guarantee parent safety.
  * Decouple token tracking: add child token usage to parent DB record without touching parent `history.Session`.
  * Roll up child `ModifiedFiles` into parent `StateTracker`.

### Phase 3: Tool Implementation & Role Gating
* [ ] Implement `InvokeSubagentTool` in `internal/tools/subagent.go`:
  * Define schema: `task` (string, required), `role` (enum: "researcher", "worker", default: "researcher"), `max_steps` (int, default: 10, max: 15).
  * Filter child registry:
    * `researcher`: `read_file` + read-only MCP tools.
    * `worker`: `bash`, `read_file`, `write_file`, `edit_file` + MCP tools.
    * Strip `invoke_subagent`, `save_memory`, `create_skill` from all child registries.
  * Enforce parent session quota of 5 subagent calls per session.
* [ ] Register `InvokeSubagentTool` in `internal/cmd/root.go` only when engine is running at `depth=0`.

### Phase 4: System Prompt & Terminal UX
* [ ] Create embedded non-interactive subagent prompt template in `internal/agent/prompts/subagent_system.md`.
* [ ] Update `internal/cmd/root.go` event handler to format subagent events with two-space indentation and role prefixes.
* [ ] Add integration tests in `internal/agent/subagent_test.go` verifying depth limit enforcement, tool restriction, token accounting, and budget exhaustion synthesis.
