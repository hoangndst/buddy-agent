# CLI Reference

`buddy-agent` provides a unified command-line interface with interactive conversation, one-shot task execution, background daemon management, and live session reattachment.

## Synopsis

```bash
buddy [flags] [task...]
buddy [command] [flags]
```

## Execution Modes

### 1. Interactive Conversation (Default)
Running `buddy` without arguments in an interactive terminal starts the Bubble Tea terminal user interface:
```bash
buddy
```
* Starts a new multi-turn conversation.
* Communicates with the local background daemon over Unix domain socket.
* Spawns the daemon automatically in the background if it is not already running.

### 2. Continue Previous Conversation
Resume the latest conversation in the active workspace directory:
```bash
buddy --continue
```
* Restores conversation transcript, turn history, and model profile from SQLite.
* Exits with an informative message if no conversation exists in the current workspace.

### 3. One-Shot Task Execution
Execute a non-interactive task and output streaming events to stdout:
```bash
buddy "Inspect the repository and run all unit tests"
```
* Submits task to the daemon or executes in-process if `--standalone` is passed.
* Stream events render to stdout and exit upon completion.

### 4. Standalone Mode
Execute entirely in-process without connecting to or spawning the background daemon:
```bash
buddy --standalone [task...]
```
* Uses an in-process conversation runtime and SQLite store.
* Useful for constrained environments, CI pipelines, or single-process execution.

---

## Global Flags

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `--config` | string | `""` | Path to custom configuration file (`buddy.yaml`). |
| `--workspace` | string | `"."` | Target workspace directory path. |
| `--socket` | string | `""` | Path to custom Unix domain socket (default: `~/.buddy/daemon.sock`). |
| `--standalone` | bool | `false` | Run in-process without daemon connection or spawning. |
| `--continue` | bool | `false` | Resume the latest conversation in the active workspace. |
| `-v`, `--verbose` | bool | `false` | Enable verbose debug logging to stderr. |

---

## Subcommands

### `buddy daemon start`
Start the background daemon process.

```bash
buddy daemon start [flags]
```

Flags:
* `--foreground`: Run the daemon in the foreground instead of detaching.
* `--socket string`: Override the Unix domain socket path.

### `buddy attach`
Attach to a running or completed session on the daemon.

```bash
buddy attach <session-id> [flags]
```

* Replays buffered session events from the in-memory ring buffer.
* Connects as a primary controller or read-only observer.
* Detach at any time using `Ctrl+D` without interrupting active background execution.

### `buddy resume`
Resume an interrupted or failed task execution from the SQLite trajectory store.

```bash
buddy resume [session-id]
```

* Rebuilds engine state and memory partitions from the latest durable snapshot.
* Resumes execution at the next pending step.
