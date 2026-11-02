# AGENTS.md

## Overview

`teams-tui` is a read-only Go TUI and CLI for the SQLite archive (`~/.local/share/teams-archive/teams.db`) populated by `teams-for-linux` via `~/.config/home-manager/programs/teams/message-logger.js`.

## Repository Structure

- `cmd/teams-tui/main.go`: CLI entrypoint (`spf13/cobra`). Running `teams-tui` starts the interactive TUI; `teams-tui messages` prints messages for a date range (defaulting to today).
- `pkg/archive/archive.go`: Read-only SQLite store (`gorm.io/gorm` + `github.com/glebarez/sqlite`), query filters (`ChannelsFiltered`, `Messages`), date range parsing (`ParseDate`), and `table`/`json` output (`github.com/aquasecurity/table`).
- `pkg/archive/archive_test.go`: Unit tests for `pkg/archive`.
- `pkg/tui/tui.go`: Interactive IRC-style Bubble Tea TUI (`github.com/charmbracelet/bubbletea`, `lipgloss`, `x/ansi`).
- `pkg/tui/tui_test.go`: Unit tests for message rendering, colors, scrolling, search modes, and help overlay.
- `vendor/`: Vendored Go module dependencies.

## Build, Test, and Lint

Always use the `Makefile` targets:

```bash
make build   # CGO_ENABLED=0 go build ./cmd/teams-tui
make install # CGO_ENABLED=0 go install ./cmd/teams-tui
make test    # go test ./...
make lint    # golangci-lint run --allow-parallel-runners
make deps    # go mod tidy && go mod vendor
```

## Key Implementation Details

- **Read-only Immutable SQLite**: `archive.Open` opens the SQLite file with `mode=ro&immutable=1` (and `SetMaxIdleConns(0)` so periodic TUI refreshes see new writes) so it never locks or mutates the database while `teams-for-linux` writes to it.
- **Bounded Message Loading**: The TUI queries the 200 most recent messages per channel by default (`defaultMessageLimit = 200`) and expands the limit in steps of 200 only when the user scrolls up to the top of the loaded buffer. Returning to the bottom (`G` / `end`) or switching channels resets the limit to 200.
- **Colors (`hashColor`)**: Both `AuthorColor` and `ThreadColor` take the first 6 hex characters of `md5.Sum` (`#%x` of `sum[:3]`).
- **Threads**: Thread root messages (`*msg.ThreadID == msg.ID`) render a bold, underlined timestamp in `ThreadColor` followed by `┬ `; thread replies (`*msg.ThreadID != msg.ID`) render `│ ` in `ThreadColor`.
- **Emoji Width & Scrollbar Alignment**: `sanitizeText` strips `U+FE0F` (Variation Selector-16) so 1-cell BMP dingbats like `❤` (`U+2764`) and `☝` (`U+261D`) have matching widths in `lipgloss.Width` and terminal `wcwidth`, keeping the right-hand scrollbar aligned.
- **Linter Conventions**: The global `~/.golangci.yml` enforces `wsl_v5`, `funlen` (60 lines / 40 statements), `gocognit` (<= 20), `goconst`, `misspell`, and `exhaustruct_v5`. Run `make lint` after every change.
