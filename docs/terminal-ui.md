# Terminal User Interface (TUI)

Buddy Agent includes an interactive, full-screen terminal interface built with Charm's Bubble Tea and Lip Gloss. It manages multi-turn conversations, live text streaming, multiline prompt composition, tool approvals, and Git working-tree reviews.

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
│ │ Type prompt, / for commands, @ for files                │ │
│ └─────────────────────────────────────────────────────────┘ │
├─────────────────────────────────────────────────────────────┤
│ [Model: gpt-4o] [Ctx: 42k/128k (32%)] [Turn: 4.2s] [Cost: $]│
└─────────────────────────────────────────────────────────────┘
```

## Core Interaction Patterns

### 1. Multiline Composer & Line-Zero History Ring
* **Multiline Editing**: Type multiline prompts naturally. Pressing `Enter` inserts newlines in multiline mode.
* **Line-Zero History Traversal**:
  * Pressing `Up Arrow` when the cursor is at line 0 traverses previous prompt history.
  * Pressing `Down Arrow` at the bottom row traverses forward; reaching the end restores the unsubmitted dirty draft.
  * `Ctrl+P` and `Ctrl+N` navigate prompt history unconditionally regardless of cursor position.
* **History Storage**: Prompts are stored in `~/.buddy/history.jsonl` (capped at 500 entries with consecutive duplicate deduplication).

### 2. Slash Commands
Type `/` in the composer to open an interactive command popup:
* `/help`: Display keyboard shortcuts and command reference.
* `/steer <instruction>`: Inject guidance into the actively running agent turn at the next model-request boundary.
* `/resume`: Open the project and conversation picker.
* `/diff`: Open the Git working-tree diff viewer.
* `/clear`: Clear the visible transcript viewport.
* `/quit`: Detach from the session and leave daemon tasks running.

### 3. File Path Autocompletion (`@`)
* Type `@` in the composer to open a fuzzy-filtered list of files in the active workspace.
* Selecting a file inserts its relative path directly into your prompt.

### 4. Interactive Tool Approvals
When a tool call requires user confirmation (e.g. destructive shell commands), an interactive modal appears above the composer:
* `[1] Allow Once`: Authorize this single tool execution.
* `[2] Allow for Session`: Whitelist this tool in the active conversation session.
* `[3] Deny`: Cancel the tool call and provide optional feedback text.
* `[4] Edit Arguments`: Open an inline JSON sub-composer to modify the tool arguments before execution.

### 5. Attributed Working-Tree Diff (`/diff`)
The `/diff` command opens a colorized diff viewer:
* Inspects `git status` and `git diff HEAD` (including untracked files).
* Cross-references file modifications against the active turn:
  * Files modified by Buddy receive an attributed green badge: `[Buddy Edit]`.
  * Pre-existing dirty files receive a neutral badge: `[Workspace Dirty]`.

### 6. Keyboard Shortcuts

| Shortcut | Context | Action |
| :--- | :--- | :--- |
| `Enter` | Idle / Working | Submit prompt (runs immediately when idle; queues when busy). |
| `Esc` | Modal / Popup | Dismiss overlay / cancel popup without losing draft. |
| `Esc` | Active Turn | Interrupt the currently running turn. |
| `Ctrl+D` | Any | Detach from the session without stopping daemon execution. |
| `Ctrl+C` | Editing | Clear current composer draft; double-press exits client. |
| `Ctrl+P` / `Ctrl+N` | Composer | Traverse prompt history ring (previous / next). |
| `PageUp` / `PageDown` | Viewport | Scroll conversation transcript history. |
