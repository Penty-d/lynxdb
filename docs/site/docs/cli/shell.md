---
sidebar_position: 10
title: Interactive Shell
description: LynxDB interactive SPL2 REPL with tab completion, query history, slash commands, and result scrolling.
---

# Interactive Shell

Start an interactive SPL2 REPL with tab completion, query history, and slash commands.

```
lynxdb shell [flags]
```

Aliases: `sh`, `repl`, `console`.

## Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--file` | `-f` | | File mode (load file into ephemeral engine) |
| `--since` | `-s` | `15m` | Default time range for queries |

## Examples

```bash
# Connect to server
lynxdb shell

# Query a local file
lynxdb shell --file access.log

# Default time range
lynxdb shell --since 1h
```

## Console Output

```
  LynxDB v0.1.0 — Interactive Shell
  Connected to http://localhost:3100
  Type /help for commands.

lynxdb> level=error | stats count by source
  source          count
  nginx           340
  api-gateway     120

  2 rows — 45ms

lynxdb>
```

## Slash Commands

Meta-operations available inside the shell:

| Command | Description |
|---------|-------------|
| `/help` (`/h`) | Show help |
| `/quit` (`/exit`, `/q`) | Exit the shell |
| `/clear` (`/cls`) | Clear the screen |
| `/history` | Show query history |
| `/fields` | List known fields (server mode) |
| `/sources` | List event sources (server mode) |
| `/explain <query>` | Show query execution plan (server mode) |
| `/set since <val>` | Change default time range |
| `/since <duration>` | Shorthand for `/set since <val>` |
| `/format <fmt>` | Set output format: `table`, `json`, `csv`, `raw` |
| `/timing [on\|off]` | Toggle elapsed time display |
| `/server` | Show server version, health, uptime (server mode) |
| `/save <name> [query]` | Save last query or a specified query (server mode) |
| `/run <name>` | Run a saved query by name (server mode) |
| `/queries` | List saved queries (server mode) |
| `/tail <query>` | Start live tail with an SPL2 filter (server mode) |

## Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `Enter` | Run the query |
| `Shift+Enter` | Insert a newline (multi-line input) |
| `Tab` | Accept inline suggestion / completion |
| `Ctrl+Space` | Open the completion popup |
| `Up` / `Down`, `Ctrl+P` / `Ctrl+N` | Navigate query history |
| `Ctrl+A` / `Ctrl+E` | Jump to line start / end |
| `Ctrl+L` | Clear the results area |
| `Ctrl+C` | Cancel a running query / clear input (twice on empty input: exit) |
| `Ctrl+D` | Exit (on empty input) |
| `Esc` | Close popup / leave scroll mode (twice on idle prompt: exit) |
| `PgUp` / `PgDn` (or `Option+↑` / `Option+↓`) | Scroll results (enters scroll mode) |
| `F2` | Toggle the sidebar |

## Scrolling Results

Press `PgUp` or `PgDn` to move focus from the prompt into the results area
(scroll mode). On Mac keyboards without physical paging keys use `Option+↑` /
`Option+↓` (or `Fn+↑` / `Fn+↓`). Inside scroll mode the full set of Vim-style
keys works:

| Key | Action |
|-----|--------|
| `j` / `k`, arrow keys | Scroll line by line |
| `u` / `d` (also `Ctrl+U`) | Half page up / down |
| `b` / `f`, `PgUp` / `PgDn`, `Option+↑` / `Option+↓`, `Space` | Page up / down |
| `g` / `G` (also `Home` / `End`) | Jump to top / bottom |
| `y` / `Y` | Copy last result table (plain / Markdown) |
| `Esc` | Back to the prompt |

While in scroll mode the shell also captures the mouse, so the wheel scrolls
the results and sidebar fields are clickable. At the prompt the mouse is left
to the terminal for native text selection - note that in most terminals the
wheel then sends arrow keys, which navigate query history instead.

The status bar at the bottom always shows the shortcuts available in the
current mode.

## Exiting

All of these leave the shell:

- `/quit` (or `/exit`, `/q`)
- Typing `quit`, `exit`, `logout`, `q`, `:q`, or `\q` (ClickHouse-style exit words)
- `Ctrl+D` on an empty prompt
- Pressing `Ctrl+C` or `Esc` twice on an empty prompt - the first press shows
  a hint, the second within 3 seconds exits

On exit the shell prints a short session summary (queries run, last result,
elapsed time).

## Tab Completion

The shell tab-completes:

- SPL2 command names (`stats`, `where`, `eval`, `sort`, etc.)
- Aggregation function names (`count`, `avg`, `sum`, `p99`, etc.)
- Field names from the server's field catalog

## Query History

- Navigate previous queries with the up/down arrow keys
- History is persisted to `~/.local/share/lynxdb/history`
- History survives between shell sessions

## Multi-Line Input

Press `Shift+Enter`, or end a line with `|`, to continue on the next line:

```
lynxdb> level=error |
   ...> stats count by source |
   ...> sort -count |
   ...> head 10
```

## File Mode

When started with `--file`, the shell loads the file into an ephemeral
in-memory engine. All queries run locally without a server:

```bash
lynxdb shell --file /var/log/nginx/access.log
```

```
  LynxDB v0.1.0 — Interactive Shell
  Loaded: /var/log/nginx/access.log (50,000 events)
  Type /help for commands.

lynxdb> | stats count by status
  status    count
  200       45000
  404       3000
  500       2000
```

## See Also

- [query](/docs/cli/query) for one-shot queries
- [Shortcuts](/docs/cli/shortcuts) for quick access commands
- [Lynx Flow Reference](/docs/lynx-flow/overview) for the query language reference
