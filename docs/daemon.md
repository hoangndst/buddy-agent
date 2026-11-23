# Daemon & Wire Protocol

The `buddy-agent` background daemon manages long-running sessions, multi-turn conversation workers, tool authorizations, and client event multiplexing over a Unix domain socket.

## Daemon Lifecycle & Spawning

### Socket Management & Locking
* **Socket Location**: Defaults to `~/.buddy/daemon.sock` (configurable via `--socket` or `BUDDY_SOCKET_PATH`).
* **Single-Instance Exclusion**: Uses a companion advisory lock file (`~/.buddy/daemon.sock.lock`). The daemon acquires an exclusive, non-blocking file lock (`flock`) on this file before binding the socket.
* **Inode Preservation**: The lock file is retained permanently on disk across reboots and shutdowns. The daemon releases the file lock and closes the file descriptor without unlinking the file, preventing inode recreation races.
* **Auto-Spawning**: If an interactive CLI invocation (`buddy`) detects that no daemon is listening, it detaches a new background daemon process (`buddy daemon start --socket ...`) and waits up to 2.5 seconds for the socket to become active.

---

## Wire Protocol (JSON-RPC 2.0)

Communication over the socket uses line-delimited JSON-RPC 2.0 (NDJSON). Each frame is a single UTF-8 JSON object terminated by `\n`. The maximum frame size is 1 MiB (`protocol.DefaultMaxMessageSize`).

### RPC Methods

| Method | Role | Description |
| :--- | :--- | :--- |
| `conversation.start` | Client $\rightarrow$ Daemon | Start a new multi-turn conversation with an initial prompt. |
| `conversation.submit` | Client $\rightarrow$ Daemon | Submit a follow-up prompt or steering input to an active conversation. |
| `conversation.resume` | Client $\rightarrow$ Daemon | Resume a previous conversation by ID or select the latest workspace conversation. |
| `conversation.sync` | Client $\rightarrow$ Daemon | Subscribe to conversation events with monotonic epoch/sequence cursors. |
| `conversation.list` | Client $\rightarrow$ Daemon | List eligible conversations for a workspace. |
| `turn.interrupt` | Client $\rightarrow$ Daemon | Abort the actively executing turn or tool in a conversation. |
| `permission.respond` | Client $\rightarrow$ Daemon | Deliver user authorization decision (`allow_once`, `allow_session`, `deny`). |
| `session.cancel` | Client $\rightarrow$ Daemon | Cancel an active task execution. |
| `daemon.stop` | Client $\rightarrow$ Daemon | Programmatically request daemon graceful shutdown. |

### Streaming Notifications

| Notification | Role | Description |
| :--- | :--- | :--- |
| `session.event` | Daemon $\rightarrow$ Client | Live stream event notification (`text_delta`, `tool_call`, `tool_result`, `text_complete`). |
| `session.permission_request` | Daemon $\rightarrow$ Client | Request human-in-the-loop authorization for a tool call. |
| `conversation.evicted` | Daemon $\rightarrow$ Client | Notification that an idle conversation worker was evicted from memory. |

---

## Session Workers & Detachment

### Primary Controller vs. Observers
* **Primary Controller**: The client connection that initiated or explicitly attached to a conversation with `take_control=true`. Has permission to submit input, approve tool calls, or cancel execution.
* **Observers**: Additional attached clients receiving read-only event broadcasts.

### Detached Ergonomics & Zero Deadlocks
* When a client disconnects (`Ctrl+D`, closed terminal, or SSH drop), the worker continues running in the background.
* Live events are buffered in a monotonic in-memory ring buffer (up to 2,048 events) for subsequent reattachment.
* A pending approval expires after 15 minutes from creation. The first controller disconnect shortens the deadline to five minutes from that disconnect; reconnects and later disconnects do not extend it. Reattaching immediately replays the pending request.
* `allow_session` caches read-only tool names or the exact raw bash command string in the current conversation worker. Modified arguments are allowed once and are recorded as executed.
