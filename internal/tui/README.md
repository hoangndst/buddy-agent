# Terminal UI Components

## Overview

This package owns terminal-facing helpers and presentation state for Buddy's interactive conversation UI. `internal/cmd` adapts daemon protocol values to UI values and remains responsible for network calls, conversation lifecycle, synchronization, and permission decisions.

## Interface

- `Run` owns Bubble Tea startup, terminal cleanup, and context/input/output wiring for the interactive view.
- `Model` owns canonical transcript blocks, stream reduction, terminal dimensions, transcript scrolling, and display metadata. It reduces protocol-neutral `Event` values; `Action` and `ActionMsg` define the runtime-intent contract used by the command adapter.
- `Composer` owns the textarea, sizing, prompt history navigation, slash-command matching, workspace-bounded file completion, submit-key behavior, and emits neutral prompt actions. The command adapter interprets slash commands and sends prompts to the daemon.
- `Model` owns ordered pending approval presentation, argument editing, approval key handling, and resize behavior. It emits a choice bound to a UI generation; the command adapter validates that generation against the pending daemon request before constructing a response.
- `Model` owns workspace and conversation picker state, cursor navigation, rendering, and local cancellation. Selection actions carry a picker kind, generation, and UI ID; the command adapter resolves them against current daemon results and discards canceled or stale list/resume responses.
- `Model` owns output/diff inspector presentation, local scrolling, panning, file selection, and keyboard navigation. The command adapter retains output page offsets, protocol conversion, RPCs, and inspection generations used to reject stale responses.
- `Model` owns transcript activity focus, activity selection, expand/inspect keys, and focus-aware scroll navigation. `internal/cmd` handles only resulting runtime actions and retains transcript rendering/cache integration during the remaining extraction.
- `Model.View` renders the canonical visible transcript, welcome screen, picker, and inspector, and composes them with adapter-supplied header/footer rows. `internal/cmd` remains the Bubble Tea runtime adapter and delegates the final frame to the TUI model.
- `InputHistoryRing` loads and stores prompt history at a caller-supplied path.
- `SlashCommandRegistry` stores and matches composer commands; `Composer` owns its instance.
- `Transcript` owns canonical blocks, reading offset, rendered-row caching, reflow, and visible-window selection; `TranscriptBlock` and `Activity` are UI-owned values.
- `RenderMarkdown` converts source Markdown to terminal output, and `WrapLines` wraps rendered terminal text by display-cell width.
- `RenderMascot` draws small welcome, compact, or large Unicode half-block pixel art from the checked-in SVG palette; `RenderWelcome` adapts the welcome panel to terminal dimensions and color settings.
- `ApprovalModal` owns approval presentation and argument editing, returning a protocol-neutral choice to the command adapter.
- `Screen` and `RenderScreen` allocate a fullscreen frame to a top-aligned body between header and footer regions and apply terminal width clipping.
- `Header` and `RenderHeader` format the compact and standard workspace/model/branch header.
- `RenderChoiceList` and `RenderCompletion` draw picker and composer completion rows.
- `FormatWorkspaceFileHyperlink` formats a workspace path for terminal display.
- `StripHyperlinks` removes OSC 8 links from terminal text.

## Invariants

- This package does not import `internal/cmd`, agent, client, daemon, persistence, or MCP packages.
- The history file remains JSONL, uses the existing cross-platform lock and 0600 permissions, and retains at most 500 entries.
- OSC 8 links are only presentation; displayed paths remain readable when links are unavailable.
- Screen layout receives rendered strings and dimensions only; it has no workspace, daemon, or runtime dependencies.
- Transcript bodies retain source Markdown; activity values contain only allowlisted display metadata.
- The command adapter may map protocol events into transcript blocks, but transcript layout and cache behavior stay in this package.
- Protocol ordering, stale-event rejection, sync generations, RPCs, and permission policy stay in `internal/cmd`; events passed to `Model` contain no protocol values or arbitrary maps.
- The command adapter retains daemon list results and calls, selected protocol values, request IDs, approval generation assignment, permission policy, response validation/construction, inspection generations, output page offsets, and RPC calls.
- The command adapter still coordinates command execution, overlay precedence, transcript focus, and fullscreen composition while the root model migration proceeds.

## Side Effects

- Prompt history reads and writes only the path supplied by the caller; `@` completion reads one directory level from the workspace path supplied by the caller and returns at most 50 matches.
- Hyperlink formatting reads terminal-related environment variables and may read the current working directory when no workspace is supplied.
- Markdown rendering reads terminal style environment variables and performs no file or network access.
- No network, subprocess, daemon, or agent side effects.

## Verification

- Run: `go test ./internal/tui ./internal/cmd`
