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
│ │ Type prompt, / for commands, @ for files                 │ │
│ └─────────────────────────────────────────────────────────┘ │
├─────────────────────────────────────────────────────────────┤
│ [Model: gpt-4o] [Ctx: 42k/128k (32%)] [Turn: 4.2s] [Cost: $]│
└─────────────────────────────────────────────────────────────┘
```

## Core Interaction Patterns

### 1. Multiline Composer
* **Multiline Editing**: Press `Shift+Enter` to insert a newline. Press `Enter` to submit the prompt.
* **Prompt History**: Use `Up` at the first line and `Down` at the last line to browse submitted prompts. `Ctrl+P` and `Ctrl+N` browse history from any line. Returning past the newest entry restores the unsent draft. The latest 500 prompts persist in `~/.buddy/history.jsonl`; `BUDDY_HISTORY_FILE` overrides the path.

### 2. Slash Commands
Type `/` commands in the composer:
* `/steer <instruction>`: Inject guidance into the actively running agent turn at the next model-request boundary.
* `/queue <prompt>`: Queue a follow-up prompt behind the active turn.
* `/interrupt`: Cancel the currently executing turn immediately.
* `/resume`: Open the project and conversation picker to switch sessions.
* `/help`: Show available commands.
* `/copy`: Copy the full un-truncated conversation markdown transcript to the system clipboard via OSC 52 escape sequences.
* `/clear`: Clear the visible transcript viewport.
* `/quit` or `/exit`: Detach from the session and leave background daemon tasks running.

Typing `/` shows matching commands. Use `Up` and `Down` to select, `Tab` to complete, `Enter` to run a command without arguments, and `Esc` to close the list. Typing `@` shows up to 50 matching entries from the workspace root; use `Tab` or `Enter` to insert the selected relative path.

### 3. Interactive Tool Approvals
When a tool call requires confirmation, the TUI shows its arguments in an approval card while conversation events continue streaming. Press `1` to allow once, `2` to allow matching read-only tools or the exact same bash command for this conversation, `3` to deny, or `4` to edit the JSON arguments and allow once. In the editor, `Enter` submits and `Shift+Enter` inserts a newline. `Esc` returns to the approval choices.

### 4. Keyboard Shortcuts

| Shortcut | Context | Action |
| :--- | :--- | :--- |
| `Enter` | Idle / Working | Submit prompt (runs immediately when idle; queues when busy). |
| `Up` / `Down` | Composer | Navigate prompt history at the first or last line; otherwise move within the draft. |
| `Ctrl+P` / `Ctrl+N` | Composer | Navigate prompt history. |
| `Esc` | Picker | Close the workspace, conversation, or composer completion list. |
| `Ctrl+C` | Any | Immediately exit the client. |
| `PageUp` / `PageDown` | Viewport | Scroll conversation transcript history. |

Use `/interrupt` to stop the active turn. Use `/quit`, `/exit`, or `Ctrl+C` to leave the client; daemon execution continues after the client exits.
