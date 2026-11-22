# Persistence & Context Compaction

Buddy Agent provides durable episodic state persistence and memory compaction implemented in pure Go with zero CGO dependencies.

## SQLite Episodic Store

Trajectory history is recorded in an embedded SQLite database (`~/.buddy/history.db` or workspace-local `.buddy/history.db`) using `modernc.org/sqlite`.

### Schema Architecture (Version 003)

```sql
-- Conversations and task sessions
CREATE TABLE IF NOT EXISTS sessions (
    id                TEXT PRIMARY KEY,
    workspace_path    TEXT NOT NULL,
    interaction_kind  TEXT NOT NULL DEFAULT 'task', -- 'task' or 'conversation'
    parent_id         TEXT,
    profile_json      TEXT,
    created_at        TIMESTAMP NOT NULL,
    updated_at        TIMESTAMP NOT NULL
);

-- Turn and ReAct step execution history
CREATE TABLE IF NOT EXISTS turns (
    id                TEXT PRIMARY KEY,
    session_id        TEXT NOT NULL,
    turn_number       INTEGER NOT NULL,
    step              INTEGER NOT NULL,
    model             TEXT NOT NULL,
    input_tokens      INTEGER NOT NULL,
    output_tokens     INTEGER NOT NULL,
    thought           TEXT,
    tool_calls        TEXT, -- JSON array of tool invocations
    tool_results      TEXT, -- JSON array of tool outputs
    created_at        TIMESTAMP NOT NULL,
    FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

-- Durable snapshots for crash recovery and resume
CREATE TABLE IF NOT EXISTS snapshots (
    id                TEXT PRIMARY KEY,
    session_id        TEXT NOT NULL,
    step              INTEGER NOT NULL,
    state_json        TEXT NOT NULL,
    created_at        TIMESTAMP NOT NULL,
    FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
```

### Connection Pools & Invariants
* **Single-Writer Actor**: Write transactions are serialized through a dedicated single-writer pool to eliminate `SQLITE_BUSY` database lock contention under SQLite WAL mode.
* **Concurrent Readers**: Read operations (history queries, transcript paging) execute concurrently across a read-only connection pool.
* **Secret Redaction**: Environment variables containing known secret patterns (`*_KEY`, `*_SECRET`, `*_TOKEN`, `*_PASSWORD`) are redacted before persisting turns or state snapshots to disk.

---

## Context Compaction Pipeline

To prevent context exhaustion during long-running tasks, Buddy implements a three-partition memory model:

```
┌─────────────────────────────────────────────────────────────┐
│              THREE-PARTITION CONTEXT LEDGER                 │
├─────────────────────────────────────────────────────────────┤
│ Partition 1: Immutable System Anchor                        │
│ • System instructions, workspace environment, tool schema   │
├─────────────────────────────────────────────────────────────┤
│ Partition 2: Bounded Episodic Anchor                        │
│ • Key-value state table (compiler errors, modified files)   │
│ • Distilled task summary (high-signal facts from past turns)│
├─────────────────────────────────────────────────────────────┤
│ Partition 3: Unpruned Working Tail                          │
│ • Most recent N turns (default: 10) retained verbatim       │
└─────────────────────────────────────────────────────────────┘
```

### Compaction Execution
1. **Trigger**: When the total token count approaches the configured capacity ceiling (`max_context_tokens`, default: 64k for cloud endpoints, 16k for local models).
2. **Stage 1 (Micro-Pruning)**: Drops redundant tool output payloads (e.g., successful bash outputs or file read blocks) in older turns outside the working tail, replacing them with tombstone summaries.
3. **Stage 2 (Distillation)**: If micro-pruning is insufficient, Buddy prompts the model to summarize intermediate reasoning steps into the episodic state table ($P_2$), preserving compiler error diagnostics, modified file paths, and objective status.
4. **Stage 3 (Deterministic Fallback)**: If distillation fails (e.g. context limit reached), Buddy prunes turns deterministically starting with the oldest unpinned steps, ensuring execution never crashes.
