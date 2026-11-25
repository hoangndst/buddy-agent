# Terminal UI Components

## Overview

This package owns terminal-facing helpers and presentation state for Buddy's interactive conversation UI. `internal/cmd` adapts daemon protocol values to UI values and remains responsible for network calls, conversation lifecycle, synchronization, and permission decisions.

## Interface

- `InputHistoryRing` loads and stores prompt history at a caller-supplied path.
- `SlashCommandRegistry` stores and matches composer commands.
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
- Permission policy and protocol response construction stay in `internal/cmd`.

## Side Effects

- Prompt history reads and writes only the path supplied by the caller.
- Hyperlink formatting reads terminal-related environment variables and may read the current working directory when no workspace is supplied.
- Markdown rendering reads terminal style environment variables and performs no file or network access.
- No network, subprocess, daemon, or agent side effects.

## Verification

- Run: `go test ./internal/tui ./internal/cmd`
