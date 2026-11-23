# Terminal User Interface (TUI)

Buddy Agent includes an interactive, full-screen terminal interface built with Charm's Bubble Tea and Lip Gloss. It manages multi-turn conversations, live text streaming, multiline prompt composition, and tool approvals.

## User Interface Layout

```
┌─────────────────────────────────────────────────────────────┐
│ Buddy Agent — gpt-4o | Workspace: /path/to/project          │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│ > User prompt text...                                       │
│                                                             │
│ Assistant response formatted in width-aware Markdown.       │
│ Syntax-highlighted code blocks, tables, and task lists.     │
│                                                             │
│ ⚙ bash: npm test                                            │
│   [Press Space to expand 34 lines of test output]           │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│ ┌─────────────────────────────────────────────────────────┐ │
│ │ Type prompt or / for commands                            │ │
│ └─────────────────────────────────────────────────────────┘ │
├─────────────────────────────────────────────────────────────┤
│ [Model: gpt-4o] [Ctx: 42k/128k (32%)] [Turn: 4.2s] [Cost: $]│
└─────────────────────────────────────────────────────────────┘
```

## Core Interaction Patterns

### 1. Multiline Composer
* **Multiline Editing**: Press `Shift+Enter` to insert a newline. Press `Enter` to submit the prompt.

### 2. Slash Commands
Type `/` commands in the composer:
* `/steer <instruction>`: Inject guidance into the actively running agent turn at the next model-request boundary.
* `/queue <prompt>`: Queue a follow-up prompt behind the active turn.
* `/interrupt`: Cancel the currently executing turn immediately.
* `/resume`: Open the project and conversation picker to switch sessions.
* `/copy`: Copy the full un-truncated conversation markdown transcript to the system clipboard via OSC 52 escape sequences.
* `/clear`: Clear the visible transcript viewport.
* `/quit` or `/exit`: Detach from the session and leave background daemon tasks running.

### 3. Interactive Tool Approvals
When a tool call requires user confirmation, the TUI displays an approval prompt above the composer. Press `y` or `a` to allow the tool call, or `n` or `d` to deny it.

### 4. Keyboard Shortcuts

| Shortcut | Context | Action |
| :--- | :--- | :--- |
| `Enter` | Idle / Working | Submit prompt (runs immediately when idle; queues when busy). |
| `Esc` | Session picker | Close the workspace or conversation picker. |
| `Ctrl+C` | Any | Immediately exit the client. |
| `PageUp` / `PageDown` | Viewport | Scroll conversation transcript history. |

Use `/interrupt` to stop the active turn. Use `/quit`, `/exit`, or `Ctrl+C` to leave the client; daemon execution continues after the client exits.
