# Terminal User Interface (TUI)

Buddy Agent includes an interactive, full-screen terminal interface built with Charm's Bubble Tea and Lip Gloss. It manages multi-turn conversations, live text streaming, multiline prompt composition, and tool approvals.

## User Interface Layout

At standard terminal sizes (80×24 or larger), Buddy uses a two-line header, a scrollable conversation, a composer at the bottom, and one contextual help row:

```
Buddy  /path/to/project
Conversation conv_…  ·  idle

You
Please inspect the failing test.

Buddy
The failure comes from the updated renderer.

Running bash: go test ./...

╭────────────────────────────────────────────────────────────╮
│ Ask Buddy anything…                                        │
╰────────────────────────────────────────────────────────────╯
Enter send · Shift+Enter newline · F6 transcript · Ctrl+C detach
```

At compact sizes (40–79 columns or 12–23 rows), Buddy shortens the header, reduces the composer, and trims secondary hints. Below 40 columns or 12 rows, it removes decorative borders and hides header/footer details before the active interaction. If the terminal is only one row high, Buddy shows the composer or the pending approval keys. Approval remains pending if the terminal is too narrow to show every available decision key; resize before responding.

Workspace and conversation names may be shortened to fit. The transcript keeps its message order and scrolls independently from the composer. Empty assistant messages do not create blank role labels. The selected activity uses a marker as well as color, so state remains understandable without color.

Workspace and conversation pickers, `/diff`, and output inspection use the same header and contextual controls. They temporarily replace the conversation and composer while keeping the prompt draft in memory. Non-TTY execution continues to use the existing line-oriented or standalone path without alternate-screen controls.

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
* `/copy`: Request an OSC 52 clipboard copy of the transcript text currently loaded in the client. Large transient tool results copy their bounded preview; terminals may ignore clipboard requests.
* `/diff`: Review changed files in the active conversation workspace, with labels for pre-existing changes and successful Buddy file-tool edits.
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
| `Ctrl+C` | Any | Detach from the daemon-backed conversation; stop standalone execution. |
| `F6` | TUI | Switch keyboard focus between the composer and transcript activity. |
| `Up` / `Down` | Transcript focus | Select an activity; inside an output pager, scroll the current page. |
| `Enter` | Transcript focus | Open a selected output pager or expand/collapse activity details. |
| `PageUp` / `PageDown` | Transcript focus | Scroll transcript history; inside an output pager, move between output pages. |
| `Up` / `Down` | `/diff` | Select a changed file; scroll within the open diff. |
| `Enter` | `/diff` | Open the selected file diff. |
| `PageUp` / `PageDown` | `/diff` file view | Move between diff hunks. |
| `Left` / `Right` | Output pager / `/diff` file view | Scroll long lines horizontally. |
| `Esc` | Pager / `/diff` | Return to the activity or file list, then close the view. |

Use `/interrupt` to stop the active turn. Use `/quit`, `/exit`, or `Ctrl+C` to leave the client; daemon execution continues after the client exits.
