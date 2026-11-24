# Terminal UI Components

## Overview

This package owns terminal-facing helpers and presentation state for Buddy's interactive conversation UI. `internal/cmd` adapts daemon protocol values to UI values and remains responsible for network calls, conversation lifecycle, synchronization, and permission decisions.

## Interface

- `InputHistoryRing` loads and stores prompt history at a caller-supplied path.
- `SlashCommandRegistry` stores and matches composer commands.
- `TranscriptBlock` and `Activity` are UI-owned transcript values; `RenderMarkdown` converts source Markdown to terminal output.
- `Screen` and `RenderScreen` allocate a fullscreen frame to header, body, and footer regions and apply terminal width clipping.
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

## Side Effects

- Prompt history reads and writes only the path supplied by the caller.
- Hyperlink formatting reads terminal-related environment variables and may read the current working directory when no workspace is supplied.
- Markdown rendering reads terminal style environment variables and performs no file or network access.
- No network, subprocess, daemon, or agent side effects.

## Verification

- Run: `go test ./internal/tui ./internal/cmd`
