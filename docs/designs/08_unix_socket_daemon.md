# Milestone 8 Design Specification: Unix Domain Socket Daemon & IPC Client

* **Author:** Principal Systems & Agent Architect
* **Date:** 2026-11-21
* **Status:** Approved
* **Milestone:** 08
* **Document:** `docs/designs/08_unix_socket_daemon.md`

---

## 1. Executive Summary

Milestones 1 through 7 established `buddy-agent` as a deterministic ReAct agent harness with subprocess process group safety, three-partition episodic memory compaction, supervised Model Context Protocol (MCP) integrations, dual-protocol LLM communication, pure-Go SQLite trajectory persistence, and bounded subagent delegation.

Until Milestone 8, all executions remained strictly bound to the lifespan of the foreground terminal process. If the user's terminal emulator terminates, an SSH session disconnects, or a long-running multi-step refactoring workflow is closed, the running agent process is terminated by the operating system.

Milestone 8 decouples agent execution from the terminal client by introducing a background daemon architecture communicating over Unix Domain Sockets (UDS). This specification delivers a high-throughput, line-delimited JSON-RPC 2.0 (NDJSON) full-duplex wire protocol, non-blocking single-instance advisory file locking, pure Go zero-CGO peer credential validation across Linux, macOS, and Windows, in-memory circular ring buffers for headless execution with "tmux"-style client re-attachment, and an interactive Human-in-the-Loop (HITL) permission handshake to ensure protocol stability for Milestone 9's Bubbletea Terminal User Interface (TUI).

---

## 2. Architectural Invariants

1. **Zero CGO**: Pure Go compilation across all target platforms (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`). No external C libraries or C runtime bindings.
2. **Deterministic Single-Instance Locking**: Non-blocking advisory file locking (`flock` on `<socket_path>.lock`) paired with active dial-probing prevents socket hijacking, eliminates race conditions between concurrent spawns, and safely cleans up stale socket files after ungraceful crashes.
3. **Headless Execution Independence**: Client disconnections must never abort running agent sessions. An in-flight turn continues executing, commits step boundaries to the SQLite trajectory store, and buffers live events in an in-memory circular ring buffer.
4. **Out-of-Band Control Signaling**: Client control signals (`session.cancel`, `permission.respond`, `session.detach`) are processed by an independent connection reader and dispatched immediately to Go cancellation contexts and wait-groups, completely bypassing ReAct engine execution locks and eliminating head-of-line blocking.
5. **Multi-Client Authority Separation**: Single Primary Controller with read/write authority (input, approvals, cancellation) and arbitrary read-only Observers. Observers receive broadcast event streams with non-blocking fan-out. Slow or unresponsive subscribers are dropped without blocking the agent engine.

---

## 3. Wire Protocol & Multiplexing

### 3.1 Line-Delimited JSON-RPC 2.0 (NDJSON)

Communication over the full-duplex Unix Domain Socket consists of UTF-8 JSON objects terminated by `\n`.

```
Client                                                  Daemon
  |                                                       |
  |--- {"jsonrpc":"2.0","id":1,"method":"session.start"}->|
  |<-- {"jsonrpc":"2.0","id":1,"result":{"session_id"}}---|
  |                                                       |
  |<-- {"jsonrpc":"2.0","method":"session.event", ...}----| (Streaming Notification)
  |<-- {"jsonrpc":"2.0","method":"session.event", ...}----|
  |                                                       |
  |<-- {"jsonrpc":"2.0","method":"permission.requested"}-| (High-priority HITL)
  |--- {"jsonrpc":"2.0","id":2,"method":"permission.respond"}->|
  |                                                       |
  |<-- {"jsonrpc":"2.0","method":"session.event","finished"}--|
```

### 3.2 Protocol Envelope Definitions (`internal/daemon/protocol/types.go`)

```go
package protocol

import "encoding/json"

type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *string         `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

const (
	ErrCodeParseError       = -32700
	ErrCodeInvalidRequest   = -32600
	ErrCodeMethodNotFound   = -32601
	ErrCodeInvalidParams    = -32602
	ErrCodeInternalError    = -32603
	ErrCodeSessionNotFound  = -32001
	ErrCodePermissionDenied = -32002
)
```

### 3.3 RPC Methods & Notifications

| Type | Name | Direction | Description |
| :--- | :--- | :---: | :--- |
| **Request** | `session.start` | Client -> Daemon | Start a new agent task. Returns `session_id`. |
| **Request** | `session.attach` | Client -> Daemon | Attach to an active or historical session. Replays events from `last_event_id`. |
| **Request** | `session.detach` | Client -> Daemon | Detach client without canceling task execution. |
| **Request** | `session.cancel` | Client -> Daemon | Immediately cancel active session execution. |
| **Request** | `permission.respond` | Client -> Daemon | Deliver user approval decision (`allow` or `deny`) for a pending tool call. |
| **Request** | `daemon.ping` | Client -> Daemon | Liveness heartbeat check. |
| **Request** | `daemon.stop` | Client -> Daemon | Request graceful daemon shutdown. |
| **Notification** | `session.event` | Daemon -> Client | Live streaming event (`reasoning`, `text_delta`, `tool_call`, `tool_result`, `step_start`, `finished`). |
| **Notification** | `permission.requested` | Daemon -> Client | Prompts user for approval on dangerous tool action before execution. |
| **Notification** | `daemon.shutting_down` | Daemon -> Client | Warns clients of impending daemon termination. |

---

## 4. Daemon Lifecycle & Single-Instance Locking

### 4.1 Deterministic Socket Path Resolution

To stay strictly within the POSIX `sockaddr_un.sun_path` limits (104 bytes on macOS, 108 bytes on Linux):
1. **Linux**: `$XDG_RUNTIME_DIR/buddy/buddy.sock` (`tmpfs`, mode `0700`), fallback to `~/.buddy/buddy.sock`.
2. **macOS**: `~/.buddy/buddy.sock` (mode `0700`).
3. **Windows**: `%LOCALAPPDATA%\buddy\buddy.sock` (AF_UNIX file).

### 4.2 Non-Blocking Advisory Locking & Socket Probing (`internal/daemon/socket.go`)

To prevent multiple daemon processes from conflicting or unlinking each other's sockets:
1. Open companion `<socket_path>.lock` with `syscall.Flock(LOCK_EX | LOCK_NB)`. If lock acquisition fails, another instance is actively running; fail fast.
2. If socket file exists, perform a `net.DialTimeout("unix", path, 100ms)`. If connection succeeds, abort. If connection fails, the socket is stale from a previous ungraceful exit; safely unlink it.
3. Bind the Unix Domain Socket listener with mode `0600` (owner read/write only).

### 4.3 Graceful Shutdown Protocol

Upon receiving `SIGTERM` or `SIGINT`:
1. **Phase 1 (Immediate)**: Close socket listener to refuse new connections, broadcast `daemon.shutting_down` to connected clients, and unlink socket file.
2. **Phase 2 (5-second drain)**: Issue cancellation to in-flight sessions. The engine traps `ctx.Done()`, writes `status = 'interrupted'` to SQLite using `context.WithoutCancel`, checkpoints SQLite WAL, releases file lock, and exits cleanly.

---

## 5. Cross-Platform Peer Credential Validation (Zero CGO)

To prevent local privilege escalation on multi-user systems:
* **Linux** (`internal/daemon/security/peercred_linux.go`): Read `SO_PEERCRED` via `golang.org/x/sys/unix.GetsockoptUcred` to assert `ucred.Uid == os.Getuid()`.
* **macOS** (`internal/daemon/security/peercred_darwin.go`): Read `LOCAL_PEERCRED` via `golang.org/x/sys/unix.GetsockoptXucred` to assert `xucred.Uid == os.Getuid()`.
* **Windows** (`internal/daemon/security/peercred_windows.go`): Generate an ephemeral 32-byte cryptographic token in `%LOCALAPPDATA%\buddy\auth.token` (mode `0600`), required in the client handshake.

---

## 6. Multi-Client Orchestration & Session Re-attachment

### 6.1 In-Memory Circular Ring Buffer (`internal/daemon/ring_buffer.go`)

Each active session retains a 1,000-event circular memory ring buffer:
* Assigns monotonically increasing `uint64` event IDs (`1, 2, 3...`).
* Replaces oldest events when capacity is reached.
* Supports `ReplayAfter(afterID uint64) []BufferedEvent` so reconnecting clients resume live stream without SQLite read churn.

### 6.2 Primary Controller vs Observers

* **Primary Controller**: The connection initiating the session or claiming leadership via `session.take_control`. Only the controller may dispatch user inputs, respond to permission prompts, or cancel execution.
* **Observers**: Receive read-only event fan-out. Control attempts return `ErrCodePermissionDenied (-32002)`.
* **Slow Consumer Protection**: Fan-out channels have a capacity of 128. If a subscriber channel fills completely, events are dropped or the slow client is disconnected, guaranteeing the core agent engine never blocks.

---

## 7. Package Layout

```
internal/
├── daemon/
│   ├── server.go              # UDS listener, client connection pump, graceful drain
│   ├── socket.go              # SocketManager, flock advisory lock, socket probing
│   ├── ring_buffer.go         # Circular memory ring buffer with ReplayAfter
│   ├── session_manager.go     # Active session registry, event broadcast, controller election
│   ├── session_worker.go      # Headless engine execution runner, cancellation routing
│   ├── protocol/
│   │   ├── types.go           # JSON-RPC 2.0 envelopes, error codes, request/response models
│   │   └── codec.go           # NDJSON encoder/decoder with line-aware buffers
│   └── security/
│       ├── peercred_linux.go   # Linux SO_PEERCRED verification
│       ├── peercred_darwin.go  # Darwin LOCAL_PEERCRED verification
│       └── peercred_windows.go # Windows token handshake
├── client/
│   ├── client.go              # Daemon IPC client, auto-spawn on demand, retry logic
│   └── stream.go              # Event stream consumer, signal router
internal/cmd/
├── root.go                    # Auto-connects to daemon when running tasks
├── daemon/
│   ├── daemon.go              # buddy daemon root command
│   ├── start.go               # buddy daemon start [--detach] [--foreground]
│   ├── stop.go                # buddy daemon stop
│   └── status.go              # buddy daemon status
└── attach.go                  # buddy attach <session_id>
```

---

## 8. Verification & Acceptance Criteria

1. **Zero CGO**: Cross-compilation passes for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, and `windows/amd64` with `CGO_ENABLED=0`.
2. **Single-Instance Advisory Locking**: Concurrent daemon launches fail fast with an informative error; stale socket files are cleanly recovered.
3. **Peer Credential Enforcement**: Local socket connections from mismatched UIDs are rejected at connection accept.
4. **Headless Execution & Replay**: Detaching a client during execution does not terminate the task; re-attaching with `last_event_id` replays buffered events and resumes live streaming.
5. **Non-Blocking Control Signaling**: Cancellation and permission responses interrupt or resume execution immediately without head-of-line blocking.
6. **Test Coverage**: 100% pass rate with race detection (`go test -v -race ./...`) and zero linter warnings (`golangci-lint run ./...`).
