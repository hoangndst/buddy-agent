# MEMORY.md - Persistent Memory & Scratchpad

## Workspace Knowledge
- Project initialized as Go module `github.com/hoangndst/buddy-agent`.
- Core architecture follows a deterministic ReAct loop with durable file-based memory and dynamic skills.

## Discovered Procedures & Rules
- All new features require unit tests and must pass CI checks.
- Keep tool output bounded to avoid context blowout.
