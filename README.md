# teams-tui

`teams-tui` is a read-only terminal UI (TUI) and CLI for browsing and querying the local SQLite message archive (`~/.local/share/teams-archive/teams.db`), paired with `message-logger.js` to capture messages from `teams-for-linux`.

## Why We Use This

Microsoft Teams history is slow to search and browse inside the Electron client. By injecting `message-logger.js` into `teams-for-linux`, incoming, outgoing, and historical messages, channels, team names, authors, reactions, and mentions are recorded into a local SQLite database (`~/.local/share/teams-archive/teams.db`). `teams-tui` then provides a fast, read-only IRC-style terminal interface and CLI reporting tools on top of that archive.

### Features

- **Automatic & On-Demand Archive Logger (`message-logger.js`)**:
  - Captures live messages, reactions, mentions, attachments, and call/membership events from the Teams DOM and WebSocket/fetch traffic.
  - Archives images and file attachments by SHA-256 checksum under `~/.local/share/teams-archive/attachments/ab/cd/ef/<checksum>.<ext>` and links them in the `message_attachments` table (`message_id`, `filename`, `checksum`, `extension`).
  - Runs SQLite writes in a dedicated Node worker process (`--db-worker`) in WAL mode so the Teams UI never blocks.
  - Adds an **Archive Sync** application menu (`Sync Current Channel`, `Re-sync`, `Full Sync`, `Pause / Resume`, `Cancel`) and a right-click `Sync channel archive` context menu item inside `teams-for-linux`.
- **IRC-style TUI (`teams-tui`)**:
  - Left pane lists channels (`#`), group chats (`&`), meeting chats (`*`), and direct chats (`@`) ordered by most recent activity; right pane renders messages chronologically with lightweight 1-second change detection.
  - Deterministic per-author, per-channel, and per-thread colors derived from the first 6 hex characters of the MD5 hash (`#rrggbb`).
  - Visual thread gutters (`•` for single-line standalone thread messages, `┌` / `├` / `│` / `└` for multi-line and multi-message threads), quoted reply blocks (`│ `), fenced code blocks (`▎ ` with `[lang]` header), inline code spans, and highlighted `@mentions`.
  - Channel name filtering (`c`), in-channel message search (`f`), and global message search across all channels (`s`).
- **CLI Subcommands (`messages` and `stats`)**:
  - `teams-tui messages`: Exports archived messages for a date range (defaulting to today) as a formatted terminal table or JSON.
  - `teams-tui stats`: Ranks top channels by total messages or messages sent by you, with optional channel name and type filters.

## How We Implemented This

The repository contains both the `teams-for-linux` injection script and the Go CLI/TUI:

- `message-logger.js`: Single-file injection script that operates in three modes depending on how it is invoked:
  - **Renderer preload (`process.type === "renderer"`)**: Observes DOM mutations, intercepts real-time conversation activity, drives ChatSvc API backfills, and batches records over IPC.
  - **Electron main process (`process.type === "browser"`)**: Wraps `appMenu.orig.js` to add the `Archive Sync` menu, registers the `teams-logger-db` IPC channel, and supervises the background DB worker.
  - **Database worker (`cmd/teams-worker` + `pkg/worker`)**: Standalone Go binary (`CGO_ENABLED=0`, `glebarez/sqlite` in WAL mode) spawned by `message-logger.js` over `stdin`/`stdout` NDJSON to persist batches and `0400` attachment files into `~/.local/share/teams-archive/`.
- `cmd/teams-tui/main.go`: Cobra CLI entrypoint launching the interactive TUI by default, plus the `messages` and `stats` subcommands.
- `pkg/archive/archive.go`: Read-only immutable SQLite store (`gorm.io/gorm` + `github.com/glebarez/sqlite`, `CGO_ENABLED=0`), change token queries (`LastChange`), filters, and `table`/`json` output writers (`github.com/aquasecurity/table`).
- `pkg/tui/tui.go`: Interactive terminal UI built with `github.com/charmbracelet/bubbletea`, `github.com/charmbracelet/lipgloss`, and `github.com/charmbracelet/x/ansi`.

## How to Enable the Logger in `teams-for-linux`

`message-logger.js` is injected into `teams-for-linux` by unpacking `resources/app.asar`, hooking `preload.js` (renderer) and `appMenu.js` (main process), and setting `TEAMS_LOGGER_NODE` to a Node.js binary with `node:sqlite` support (Node.js 22.5+).

### Option 1: Nix / Home Manager

In your Home Manager configuration (e.g. `programs/teams.nix`), extract the `teams-for-linux` AppImage, fetch `message-logger.js` from GitHub, and wire both hooks in `postExtract`:

```nix
{pkgs, ...}: let
  pname = "teams-for-linux";
  version = "2.28.0";

  src = pkgs.fetchurl {
    name = pname;
    url = "https://github.com/IsmaelMartinez/teams-for-linux/releases/download/v${version}/teams-for-linux-${version}.AppImage";
    sha256 = "Qloi1YpEjyGoNSKpMk5s/85zHB5MWmZOGvTr0XTmn1w=";
  };

  messageLogger = pkgs.fetchurl {
    url = "https://github.com/jovandeginste/teams-tui/raw/refs/heads/master/message-logger.js";
    hash = "sha256-+Z3h5pBhL1HuHm6URerG8IuAK1sIrtLlzv+HAzb9waA=";
  };

  extracted = pkgs.appimageTools.extract {
    inherit pname version src;
    postExtract = ''
      ${pkgs.asar}/bin/asar extract $out/resources/app.asar $out/resources/app
      rm $out/resources/app.asar
      cp ${messageLogger} $out/resources/app/app/browser/message-logger.js
      echo 'require("./message-logger.js");' >> $out/resources/app/app/browser/preload.js
      mv $out/resources/app/app/menus/appMenu.js $out/resources/app/app/menus/appMenu.orig.js
      echo 'module.exports = require("../browser/message-logger.js");' > $out/resources/app/app/menus/appMenu.js
    '';
  };

  wrapper = pkgs.writeShellScriptBin pname ''
    export TEAMS_LOGGER_NODE="${pkgs.nodejs}/bin/node"
    appimage-run -w ${extracted} -- "$@"
  '';
in {
  home.packages = [pkgs.appimage-run wrapper];
}
```

### Option 2: Manual `asar` Patching

For a standard `teams-for-linux` installation where `resources/app.asar` and `resources/app.asar.unpacked` are located in `/opt/teams-for-linux/resources` (keep `app.asar.unpacked` in the same directory when running `asar extract`):

```bash
cd /opt/teams-for-linux/resources
npx @electron/asar extract app.asar app
rm app.asar

cp /path/to/teams-tui/message-logger.js app/app/browser/message-logger.js
echo 'require("./message-logger.js");' >> app/app/browser/preload.js

mv app/app/menus/appMenu.js app/app/menus/appMenu.orig.js
echo 'module.exports = require("../browser/message-logger.js");' > app/app/menus/appMenu.js
```

When launching `teams-for-linux`, ensure `teams-worker` is in `PATH` or set `TEAMS_LOGGER_WORKER=/path/to/teams-worker`.

### Option 3: Build a Custom AppImage from the Upstream AppImage

Use `./build-appimage.sh` (or `make appimage`) to download the official `teams-for-linux` AppImage, inject `message-logger.js` and `teams-worker`, and repackage it into `bin/teams-for-linux-<version>-logger.AppImage` using [`appimagetool`](https://github.com/AppImage/appimagetool):

```bash
# Build using the default version (2.28.0) or specify a version with -V / V=...
./build-appimage.sh -V 2.28.0
# or via Make:
make appimage V=2.28.0

# On Nix / Home Manager (provides asar, appimage-run, and cached appimagetool via nix-shell):
./build-appimage-nix.sh -V 2.28.0
# or via Make:
make appimage-nix V=2.28.0
```

Manual steps performed by the script:

```bash
# 1. Build teams-tui and teams-worker
make build

# 2. Download and extract the upstream AppImage (creates ./squashfs-root)
VERSION="2.28.0"
curl -fLO "https://github.com/IsmaelMartinez/teams-for-linux/releases/download/v${VERSION}/teams-for-linux-${VERSION}.AppImage"
chmod +x "teams-for-linux-${VERSION}.AppImage"
./"teams-for-linux-${VERSION}.AppImage" --appimage-extract

# 3. Unpack app.asar inside squashfs-root/resources
#    (keep app.asar.unpacked alongside app.asar during extraction)
npx @electron/asar extract squashfs-root/resources/app.asar squashfs-root/resources/app
rm squashfs-root/resources/app.asar

# 4. Inject message-logger.js into preload.js and appMenu.js
cp message-logger.js squashfs-root/resources/app/app/browser/message-logger.js
echo 'require("./message-logger.js");' >> squashfs-root/resources/app/app/browser/preload.js
mv squashfs-root/resources/app/app/menus/appMenu.js squashfs-root/resources/app/app/menus/appMenu.orig.js
echo 'module.exports = require("../browser/message-logger.js");' > squashfs-root/resources/app/app/menus/appMenu.js

# 5. Bundle teams-worker inside the AppDir (AppRun adds $APPDIR to PATH automatically)
cp bin/teams-worker squashfs-root/teams-worker

# 6. Optional: repack resources/app back into app.asar
#    (if omitted, Electron loads resources/app directly)
npx @electron/asar pack squashfs-root/resources/app squashfs-root/resources/app.asar \
  --unpack-dir "node_modules/@cbor-extract"
rm -rf squashfs-root/resources/app

# 7. Repackage squashfs-root into a new AppImage
ARCH=x86_64 appimagetool squashfs-root "teams-for-linux-${VERSION}-logger.AppImage"
```

### Triggering Archive Sync in `teams-for-linux`

- **Automatic**: Viewing channels or receiving messages logs them automatically and syncs unread/delta history in the background.
- **Single Channel**: Right-click a channel or chat in the sidebar and click **Sync channel archive**, or open the conversation and choose **Archive Sync → Sync Current Channel** from the application menu bar.
- **All Conversations**: Open the application menu bar (`Alt`) and choose **Archive Sync → Full Sync** (or **Re-sync** to only fetch conversations with new activity since their last sync).

## How to Build and Run `teams-tui`

Use the `Makefile` targets:

```bash
# Build ./bin/teams-tui and ./bin/teams-worker binaries
make build

# Install into $GOBIN / $GOPATH/bin
make install

# Run unit tests
make test

# Run golangci-lint
make lint

# Tidy and vendor dependencies
make deps

# Launch teams-for-linux live-injecting ./message-logger.js from this repo
make run-teams
```

### Interactive TUI

```bash
# Launch TUI with default archive (~/.local/share/teams-archive/teams.db)
teams-tui

# Launch TUI pre-filtered to a channel name
teams-tui -c "General"

# Use a custom database path
teams-tui -d /path/to/teams.db
```

#### Keyboard Shortcuts (`?` in TUI)

- `↑`/`↓`, `j`/`k`, mouse wheel: Navigate channels or scroll messages in the focused pane
- `tab`, `h`/`l`, `←`/`→`, mouse click: Switch focus between the channel list and message pane
- `J`/`K`, `pgup`/`pgdn`, `ctrl+u`/`ctrl+d`: Scroll messages directly from either pane
- `home`/`end`, `g`/`G`: Jump to top or bottom
- `c` (or `/` in channel pane): Filter channels by name
- `f` (or `/` in message pane): Search messages in the current channel
- `s`: Search messages across all channels (filters both channels and messages)
- `o` (or click an `[attachment: ...]` line): Open attachment with `xdg-open`
- `esc`: Clear active search filter or return focus to channel list
- `?`: Toggle keyboard shortcuts overlay popup
- `q`, `ctrl+c`: Quit

### CLI Subcommands

#### `messages`

```bash
# Show all messages from today as a table
teams-tui messages

# Filter by date range and channel name
teams-tui messages --from 2026-10-30 --to 2026-11-02 -c "General"

# Filter by author, direction, and content substring, output as JSON
teams-tui messages -a "Alice" --direction received -q "deploy" -o json
```

#### `stats`

```bash
# Show top 20 channels by total messages
teams-tui stats

# Show top 10 direct or group chats ranked by messages sent by you
teams-tui stats -t direct -t group -s -n 10

# Output statistics as JSON
teams-tui stats -o json
```

## Relevant Links

- [teams-for-linux](https://github.com/IsmaelMartinez/teams-for-linux)
- [Bubble Tea](https://github.com/charmbracelet/bubbletea)
- [Lipgloss](https://github.com/charmbracelet/lipgloss)
- [aquasecurity/table](https://github.com/aquasecurity/table)
