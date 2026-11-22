# Model Context Protocol (MCP) Integration

Buddy Agent implements first-party client support for the Model Context Protocol (MCP) using the official `modelcontextprotocol/go-sdk`. This allows Buddy to discover and execute tools from external servers over stdio and remote Server-Sent Events (SSE).

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                       BUDDY AGENT                           │
│                                                             │
│   Tool Registry ──► MCP ClientManager                       │
│                           │                                 │
│        ┌──────────────────┴──────────────────┐              │
│        ▼                                     ▼              │
│   Stdio Transport                       SSE Transport       │
└────────┬─────────────────────────────────────┬──────────────┘
         │ Subprocess (Setpgid)                │ HTTP Stream
         ▼                                     ▼
  Local Stdio Server                    Remote SSE Server
  (Node / Python / Go)                  (External Service)
```

## Transport Types

### 1. Stdio Transport (Local Processes)
* **Process Group Isolation**: Stdio server processes are spawned inside dedicated operating system process groups (`Setpgid: true` on POSIX, Job Objects on Windows).
* **Supervised Staged Termination**: When the session or daemon shuts down, the MCP transport issues `SIGTERM`, grants a 1.5-second grace window, and escalates to `SIGKILL` sent to the negative process group ID (`-PGID`).
* **Environment Inheritance**: The caller's session environment (including `PATH`, API tokens, and project-specific variables) is merged into the child process. Bare commands (`npx`, `python`, `node`) are resolved against the merged `PATH` before execution.
* **Working Directory Normalization**: Relative working directories in server configurations are normalized against the project workspace.

### 2. SSE Transport (Remote Services)
* Connects to external HTTP/HTTPS endpoints serving Server-Sent Events.
* Supports custom headers for authorization (e.g. `Authorization: Bearer <token>`).

---

## Configuration

MCP servers are configured via JSON files located at `~/.buddy/mcp.json` (global) or `<workspace>/.buddy/mcp.json` (project-specific). Configurations declare servers under the `mcpServers` object key:

```json
{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_..."
      }
    },
    "postgres": {
      "command": "docker",
      "args": ["run", "-i", "--rm", "mcp/postgres", "postgresql://localhost/mydb"],
      "workDir": "."
    },
    "remote_service": {
      "url": "https://mcp.internal.example.com/sse",
      "headers": {
        "Authorization": "Bearer token..."
      }
    }
  }
}
```

---

## Tool Discovery & Execution

* On startup, Buddy initializes the configured MCP servers, exchanges protocol handshakes, and queries available tools.
* Tools are registered in Buddy's unified tool registry under their declared names. If server namespace collisions occur, tools are qualified with their server name (e.g. `github_create_issue`).
* Tool inputs are validated against the server's declared JSON Schema before execution.
