# Subagent Delegation & Orchestration

Buddy Agent supports bounded, hierarchical subagent delegation. A primary coordinator agent can delegate focused subtasks to specialized worker subagents while enforcing strict recursion depth and step budgets.

## Delegation Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                 PRIMARY COORDINATOR AGENT                   │
│                                                             │
│   • Depth = 0                                               │
│   • Full Tool Registry (Bash, FS, MCP, Subagents)           │
│   • Step Budget: 30 steps                                   │
└──────────────────────────────┬──────────────────────────────┘
                               │ spawns subagent
                               ▼
┌─────────────────────────────────────────────────────────────┐
│                  CHILD WORKER SUBAGENT                      │
│                                                             │
│   • Depth = 1 (Leaf Worker)                                 │
│   • Restricted Tool Registry (Subagent spawning blocked)    │
│   • Isolated Step Budget (e.g. 5-10 steps)                  │
│   • Hierarchical SQLite Session (parent_id linked)          │
└─────────────────────────────────────────────────────────────┘
```

## Safety Constraints & Invariants

### 1. Bounded Recursion Depth (`depth = 1`)
* Subagents operate under a strict depth limit: `max_depth = 1`.
* When a child subagent is initialized, its tool registry excludes subagent delegation tools (`spawn_subagent`).
* Child subagents cannot spawn further descendants, eliminating recursive fork bombs and runaway token loops.

### 2. Isolated Step Budgets
* Each subagent receives an isolated, bounded step budget (default: 10 steps) independent of the coordinator's budget.
* If a subagent exhausts its step limit, execution terminates safely with an emergency synthesis step, returning its partial findings to the coordinator.

### 3. Hierarchical SQLite Session Tracking
* Child subagent sessions are linked to the parent via `sessions.parent_id`.
* Trajectories and steps executed by the subagent are tagged with `depth = 1` and the child `session_id`.
* Subagent sessions are excluded from top-level conversation listings (`buddy --continue` and `/resume` only list root conversations where `depth = 0`).

### 4. TUI Visual Nesting
In the terminal user interface, subagent execution steps are visually indented under the parent turn's activity block:
```
⚙ spawn_subagent: Research MCP Documentation
  │ [Step 1] Read specification file
  │ [Step 2] Search for server configurations
  └ Subagent finished in 2 steps (returned summary)
```
Outputs are collapsed by default to prevent subagent logs from cluttering the linear conversation transcript.
