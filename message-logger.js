const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const PANE_SELECTOR = '[data-tid="message-pane-layout"]';
const MESSAGE_SELECTOR = [
  '[data-tid="chat-pane-message"][data-mid]',
  '[data-tid="control-message-renderer"][data-mid]',
  '[data-testid="message-body-flex-wrapper"][data-mid]',
  '[id^="message-body-"][data-mid]',
].join(", ");
const TITLE_SELECTOR = [
  '[data-tid="channelTitle-text"]',
  '[data-tid="chat-title"]',
  '[data-tid="chatTitle"]',
  '[data-tid="threadTitle"]',
  '[data-tid="postTitle-text"]',
].join(", ");

const UUID_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function normalizeAuthorId(raw) {
  if (!raw || typeof raw !== "string") return null;
  let id = raw.trim();
  const contactsIdx = id.indexOf("/contacts/");
  if (contactsIdx !== -1) {
    id = id.slice(contactsIdx + "/contacts/".length);
  }
  if (UUID_RE.test(id)) {
    return `8:orgid:${id.toLowerCase()}`;
  }
  return id || null;
}

const DIRECT_CHAT_RE =
  /^19:([0-9a-f-]{36})_([0-9a-f-]{36})@unq\.gbl\.spaces$/i;

function isCanonicalChannelId(id) {
  return typeof id === "string" && (id.startsWith("19:") || id === "48:notes");
}

function isPersonMri(id) {
  if (!id || typeof id !== "string") return false;
  if (id.startsWith("28:") || id.startsWith("4:") || id.startsWith("19:")) {
    return false;
  }
  return (
    id.startsWith("8:orgid:") ||
    id.startsWith("8:live:") ||
    id.startsWith("8:teamsvisitor:") ||
    id.startsWith("8:sipe:")
  );
}

function normalizeChannelName(raw) {
  if (!raw || typeof raw !== "string") return null;
  const parts = raw
    .split(/\r?\n/)
    .map((s) => s.trim())
    .filter(Boolean);
  return parts.length > 0 ? parts.join(", ") : null;
}

function shouldKeepExistingGroupChannelName(existingName, nextName) {
  if (!existingName || !nextName || existingName === nextName) return false;
  if (/\+\d+$/.test(nextName) && !/\+\d+$/.test(existingName)) return true;
  const existingParts = existingName
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
  const nextParts = nextName
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
  const existingHasFullNames = existingParts.some((p) => p.includes(" "));
  const nextAllSingleWord =
    nextParts.length > 0 && nextParts.every((p) => !p.includes(" "));
  if (existingHasFullNames && nextAllSingleWord) {
    const existingFirsts = new Set(
      existingParts.map((p) => p.split(/\s+/)[0].toLowerCase())
    );
    if (nextParts.every((p) => existingFirsts.has(p.toLowerCase()))) {
      return true;
    }
  }
  return false;
}

function dedupeParticipantNames(rawNames, me = null) {
  const cleaned = [];
  const seenLower = new Set();
  const myNameLower = me?.name?.trim()?.toLowerCase() || null;
  for (const raw of rawNames) {
    if (!raw || typeof raw !== "string") continue;
    const name = raw.trim();
    if (
      !name ||
      name === "bot" ||
      name === "app" ||
      name === "Polls" ||
      name === "Everyone" ||
      name.startsWith("8:") ||
      name.startsWith("28:") ||
      name.startsWith("4:")
    ) {
      continue;
    }
    const lower = name.toLowerCase();
    if (myNameLower && lower === myNameLower) continue;
    if (seenLower.has(lower)) continue;
    seenLower.add(lower);
    cleaned.push(name);
  }
  const multiWordFirsts = new Set(
    cleaned
      .filter((n) => n.includes(" "))
      .map((n) => n.split(/\s+/)[0].toLowerCase())
  );
  return cleaned.filter(
    (n) => n.includes(" ") || !multiWordFirsts.has(n.toLowerCase())
  );
}

function formatReactionsSummary(reactions) {
  if (!reactions || reactions.length === 0) return null;
  return reactions
    .map((r) => `${r.emoji || r.key} ${r.count}`)
    .join(", ");
}

function chooseBetterAuthorName(existing, next) {
  if (!next || next === "" || next.startsWith("8:")) return existing ?? null;
  if (!existing || existing === "" || existing.startsWith("8:")) return next;
  if (existing.includes(" ") && !next.includes(" ")) return existing;
  return next;
}

// --- Background SQLite Worker Process ---
if (process.argv.includes("--db-worker")) {
  const { DatabaseSync } = require("node:sqlite");
  const dbDir = path.join(os.homedir(), ".local", "share", "teams-archive");
  fs.mkdirSync(dbDir, { recursive: true });

  const workerDb = new DatabaseSync(path.join(dbDir, "teams.db"));
  workerDb.exec(`
    PRAGMA busy_timeout = 5000;
    PRAGMA journal_mode = WAL;
    PRAGMA synchronous = NORMAL;
    CREATE TABLE IF NOT EXISTS authors (
      id TEXT PRIMARY KEY,
      name TEXT
    );
    CREATE TABLE IF NOT EXISTS channels (
      id TEXT PRIMARY KEY,
      name TEXT,
      team_name TEXT,
      type TEXT,
      last_sync TIMESTAMP
    );
    CREATE TABLE IF NOT EXISTS message_reactions (
      message_id INTEGER,
      key TEXT,
      emoji TEXT,
      count INTEGER,
      PRIMARY KEY (message_id, key)
    );
    CREATE TABLE IF NOT EXISTS message_mentions (
      message_id INTEGER,
      author_id TEXT,
      PRIMARY KEY (message_id, author_id),
      FOREIGN KEY (message_id) REFERENCES messages(id),
      FOREIGN KEY (author_id) REFERENCES authors(id)
    );
    CREATE TABLE IF NOT EXISTS messages (
      id INTEGER PRIMARY KEY,
      thread_id INTEGER,
      channel_id TEXT,
      direction TEXT,
      author_id TEXT,
      content TEXT,
      reactions TEXT,
      timestamp TIMESTAMP,
      recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );
    CREATE INDEX IF NOT EXISTS idx_messages_channel_timestamp ON messages(channel_id, timestamp);
    CREATE INDEX IF NOT EXISTS idx_messages_thread_id ON messages(thread_id);
    CREATE INDEX IF NOT EXISTS idx_messages_author_id ON messages(author_id);
    CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp);
    CREATE INDEX IF NOT EXISTS idx_message_mentions_author_id ON message_mentions(author_id);
    CREATE INDEX IF NOT EXISTS idx_authors_name ON authors(name);
    CREATE INDEX IF NOT EXISTS idx_channels_name ON channels(name);
    CREATE INDEX IF NOT EXISTS idx_messages_channel_id ON messages(channel_id, id DESC);
  `);
  try {
    workerDb.exec("ALTER TABLE channels ADD COLUMN last_sync TIMESTAMP");
  } catch {
    // Column already exists
  }
  try {
    workerDb.exec("ALTER TABLE channels ADD COLUMN team_name TEXT");
  } catch {
    // Column already exists
  }

  const wUpdateLastSync = workerDb.prepare(`
    UPDATE channels
    SET last_sync = datetime(? / 1000, 'unixepoch')
    WHERE id = ? AND (last_sync IS NULL OR unixepoch(last_sync) * 1000 < ?)
  `);
  const wUpsertAuthor = workerDb.prepare(`
    INSERT INTO authors (id, name) VALUES (?, ?)
    ON CONFLICT(id) DO UPDATE SET
      name = CASE
        WHEN excluded.name IS NULL OR excluded.name = '' OR excluded.name LIKE '8:%' THEN authors.name
        WHEN authors.name IS NULL OR authors.name = '' OR authors.name LIKE '8:%' THEN excluded.name
        WHEN authors.name LIKE '% %' AND excluded.name NOT LIKE '% %' THEN authors.name
        ELSE excluded.name
      END
  `);
  const wSelectChannelInfo = workerDb.prepare(`
    SELECT name, team_name, type FROM channels WHERE id = ?
  `);
  const wUpsertChannelStmt = workerDb.prepare(`
    INSERT INTO channels (id, name, team_name, type) VALUES (?, ?, ?, ?)
    ON CONFLICT(id) DO UPDATE SET
      name = COALESCE(excluded.name, channels.name),
      team_name = COALESCE(excluded.team_name, channels.team_name),
      type = COALESCE(excluded.type, channels.type)
  `);
  const wUpsertMessage = workerDb.prepare(`
    INSERT INTO messages (
      id, thread_id, channel_id, direction, author_id, content, reactions, timestamp
    ) VALUES (?, ?, ?, ?, ?, ?, ?, datetime(? / 1000, 'unixepoch'))
    ON CONFLICT(id) DO UPDATE SET
      thread_id = COALESCE(messages.thread_id, excluded.thread_id),
      channel_id = CASE
        WHEN excluded.channel_id LIKE '19:%' OR excluded.channel_id = '48:notes' THEN excluded.channel_id
        ELSE COALESCE(messages.channel_id, excluded.channel_id)
      END,
      direction = CASE
        WHEN excluded.direction IN ('sent', 'received') THEN excluded.direction
        ELSE COALESCE(messages.direction, excluded.direction)
      END,
      author_id = COALESCE(messages.author_id, excluded.author_id),
      content = CASE WHEN excluded.content != '' THEN excluded.content ELSE messages.content END,
      reactions = COALESCE(excluded.reactions, messages.reactions),
      recorded_at = CASE
        WHEN excluded.content != '' AND excluded.content IS NOT messages.content THEN CURRENT_TIMESTAMP
        ELSE messages.recorded_at
      END
  `);
  const wDeleteReactions = workerDb.prepare(`
    DELETE FROM message_reactions WHERE message_id = ?
  `);
  const wUpsertReaction = workerDb.prepare(`
    INSERT INTO message_reactions (message_id, key, emoji, count)
    VALUES (?, ?, ?, ?)
    ON CONFLICT(message_id, key) DO UPDATE SET
      emoji = COALESCE(excluded.emoji, message_reactions.emoji),
      count = excluded.count
  `);
  const wDeleteMentions = workerDb.prepare(`
    DELETE FROM message_mentions WHERE message_id = ?
  `);
  const wUpsertMention = workerDb.prepare(`
    INSERT OR IGNORE INTO message_mentions (message_id, author_id)
    VALUES (?, ?)
  `);
  const wSelectAuthorName = workerDb.prepare(
    `SELECT name FROM authors WHERE id = ?`
  );
  const wSelectChannelMessagesForNaming = workerDb.prepare(`
    SELECT m.id, m.direction, m.author_id, a.name AS author_name, m.content
    FROM messages m
    LEFT JOIN authors a ON a.id = m.author_id
    WHERE m.channel_id = ?
    ORDER BY m.id ASC
  `);
  const wSelectEventMentionsForNaming = workerDb.prepare(`
    SELECT a.name
    FROM message_mentions mm
    JOIN authors a ON a.id = mm.author_id
    WHERE mm.message_id = ?
  `);

  function workerUpsertChannel(channelId, rawName, channelType, rawTeamName = null) {
    let name = normalizeChannelName(rawName);
    const teamName = normalizeChannelName(rawTeamName);
    if (channelId) {
      const existing = wSelectChannelInfo.get(channelId);
      const existingName = normalizeChannelName(existing?.name);
      if (
        (channelType === "group chat" || channelId.endsWith("@thread.v2")) &&
        shouldKeepExistingGroupChannelName(existingName, name)
      ) {
        name = existingName;
      } else if (
        existingName &&
        teamName &&
        name === teamName &&
        existingName !== teamName
      ) {
        name = existingName;
      }
    }
    wUpsertChannelStmt.run(
      channelId,
      name ?? null,
      teamName ?? null,
      channelType ?? null
    );
    return name;
  }

  function workerResolvePersonName(rawId, fallback = null) {
    const id = normalizeAuthorId(rawId);
    if (!id) return fallback || null;
    if (id.startsWith("28:app:")) return fallback || "app";
    if (id.startsWith("28:")) return fallback || "bot";
    const row = wSelectAuthorName.get(id);
    if (row?.name) return row.name;
    return fallback || id;
  }

  function workerComputeGroupChatNameFromDb(channelId, me = null) {
    if (!isCanonicalChannelId(channelId)) return null;
    const rows = wSelectChannelMessagesForNaming.all(channelId);
    if (rows.length === 0) return null;

    const myId = me?.id || null;
    const myName = me?.name?.trim() || null;
    const active = new Set();
    const all = new Set();
    const removed = new Set();

    const addName = (raw, force = false) => {
      if (!raw) return;
      let name = raw.trim();
      if (name.startsWith("8:")) {
        name = workerResolvePersonName(name);
      }
      if (
        !name ||
        name === myName ||
        name === "bot" ||
        name === "app" ||
        name === "Polls" ||
        name === "Everyone" ||
        name === "Conference Virtual Assistant" ||
        name.startsWith("8:") ||
        name.startsWith("28:") ||
        name.startsWith("4:") ||
        name.startsWith("R.ICTS.")
      ) {
        return;
      }
      if (force) removed.delete(name);
      all.add(name);
      if (!removed.has(name)) active.add(name);
    };

    const removeName = (raw) => {
      if (!raw) return;
      let name = raw.trim();
      if (name.startsWith("8:")) {
        name = workerResolvePersonName(name);
      }
      if (!name) return;
      active.delete(name);
      removed.add(name);
      for (const a of [...active]) {
        if (a.split(/\s+/)[0].toLowerCase() === name.toLowerCase()) {
          active.delete(a);
          removed.add(a);
        }
      }
    };

    for (const r of rows) {
      if (r.direction !== "event") {
        if (r.author_id && (!myId || r.author_id !== myId)) {
          addName(r.author_name || workerResolvePersonName(r.author_id), false);
        }
        continue;
      }
      const text = (r.content || "").trim();
      const evMentions = wSelectEventMentionsForNaming
        .all(r.id)
        .map((x) => x.name)
        .filter((n) => n && n !== myName);

      if (text.includes(" removed ") || text.includes(" verwijderd")) {
        const idx = text.indexOf(" removed ");
        if (idx !== -1) {
          addName(text.slice(0, idx).trim(), false);
          for (const p of text.slice(idx + 9).split(",")) removeName(p);
        } else if (evMentions.length >= 2) {
          addName(evMentions[0], false);
          for (const p of evMentions.slice(1)) removeName(p);
        }
      } else if (
        text.endsWith(" left") ||
        text.includes(" heeft de chat verlaten")
      ) {
        if (evMentions.length > 0) {
          for (const p of evMentions) removeName(p);
        } else {
          removeName(text.replace(/\s*(?:left|heeft de chat verlaten).*$/, ""));
        }
      } else if (text.includes(" added ") || text.includes(" toegevoegd")) {
        for (const p of evMentions) addName(p, true);
        if (text.includes(" added ")) {
          const idx = text.indexOf(" added ");
          addName(text.slice(0, idx).trim(), true);
          for (const p of text.slice(idx + 7).split(",")) addName(p, true);
        }
      } else if (
        text.startsWith("Added ") ||
        text.startsWith("Members added: ")
      ) {
        const rest = text.replace(/^(?:Added |Members added: )/, "");
        for (const p of rest.split(",")) addName(p, true);
      } else if (/^\d{13}8:orgid:/i.test(text)) {
        const mris = [...text.matchAll(/8:orgid:[0-9a-f-]{36}/gi)].map((m) =>
          m[0].toLowerCase()
        );
        if (mris.length === 2 && mris[0] === mris[1]) {
          removeName(mris[0]);
        } else {
          for (const id of mris) addName(id, true);
        }
      } else if (text.startsWith("Call ended — ")) {
        for (const p of text.slice("Call ended — ".length).split(",")) {
          const clean = p.replace(/\s*\([^)]+\)\s*$/, "").trim();
          addName(clean, false);
        }
      }
    }

    const chosen = active.size > 0 ? [...active] : [...all];
    const deduped = dedupeParticipantNames(chosen, me);
    return deduped.length > 0 ? deduped.join(", ") : null;
  }

  function sendInitSnapshot() {
    if (!process.send) return;
    const initAuthors = workerDb.prepare("SELECT id, name FROM authors").all();
    const initChannels = workerDb
      .prepare(
        `SELECT
          c.id,
          c.name,
          c.team_name,
          c.type,
          unixepoch(c.last_sync) * 1000 AS last_sync_ms,
          (SELECT MAX(m.id) FROM messages m WHERE m.channel_id = c.id) AS max_id
        FROM channels c`
      )
      .all();
    process.send({
      type: "init",
      authors: initAuthors,
      channels: initChannels,
    });
  }

  sendInitSnapshot();

  process.on("message", (msg) => {
    if (!msg) return;
    if (msg.type === "request-init") {
      sendInitSnapshot();
      return;
    }
    if (msg.type !== "flush") return;
    const {
      authors = [],
      channels = [],
      records = [],
      lastSyncs = [],
      groupNameRequests = [],
    } = msg;

    const updatedGroupNames = [];
    workerDb.exec("BEGIN");
    try {
      for (const a of authors) {
        if (a.id) wUpsertAuthor.run(a.id, a.name ?? null);
      }
      for (const c of channels) {
        if (c.id) {
          workerUpsertChannel(c.id, c.name, c.type, c.teamName);
        }
      }
      for (const rec of records) {
        if (rec.channelId) {
          workerUpsertChannel(
            rec.channelId,
            rec.channelName ?? null,
            rec.channelType ?? null,
            rec.teamName ?? null
          );
        }
        if (rec.authorId) {
          wUpsertAuthor.run(rec.authorId, rec.authorName ?? null);
        }
        wUpsertMessage.run(
          rec.id,
          rec.threadId ?? null,
          rec.channelId ?? null,
          rec.direction,
          rec.authorId ?? null,
          rec.content ?? "",
          rec.reactionsSummary ?? null,
          rec.id
        );
        if (rec.reactions && rec.reactions.length > 0) {
          wDeleteReactions.run(rec.id);
          for (const r of rec.reactions) {
            wUpsertReaction.run(rec.id, r.key, r.emoji ?? null, r.count);
          }
        }
        wDeleteMentions.run(rec.id);
        if (rec.mentions && rec.mentions.length > 0) {
          for (const m of rec.mentions) {
            if (!m.id) continue;
            wUpsertAuthor.run(m.id, m.name ?? null);
            wUpsertMention.run(rec.id, m.id);
          }
        }
      }
      for (const ls of lastSyncs) {
        if (ls.channelId && ls.syncTs) {
          wUpdateLastSync.run(ls.syncTs, ls.channelId, ls.syncTs);
        }
      }
      for (const req of groupNameRequests) {
        const computed = workerComputeGroupChatNameFromDb(
          req.channelId,
          req.me
        );
        if (computed) {
          const finalName = workerUpsertChannel(
            req.channelId,
            computed,
            "group chat",
            null
          );
          if (finalName) {
            updatedGroupNames.push({
              channelId: req.channelId,
              name: finalName,
            });
          }
        }
      }
      workerDb.exec("COMMIT");
      try {
        workerDb.exec("PRAGMA wal_checkpoint(PASSIVE)");
      } catch {
        // ignore checkpoint error
      }
    } catch (err) {
      try {
        workerDb.exec("ROLLBACK");
      } catch {
        // ignore rollback error
      }
      console.error("[message-logger-worker] flush error:", err);
    }

    if (updatedGroupNames.length > 0 && process.send) {
      process.send({
        type: "group-names",
        updates: updatedGroupNames,
      });
    }
  });

  process.on("disconnect", () => process.exit(0));
  return;
}

// --- Electron Main Process (DB worker supervisor + Archive Sync menu) ---
if (process.type === "browser") {
  const { ipcMain, webContents } = require("electron");
  const { fork } = require("node:child_process");
  const origAppMenu = require("../menus/appMenu.orig.js");
  try {
    const { allowedChannels } = require("../security/ipcValidator");
    allowedChannels.add("teams-logger-db");
  } catch {
    // ignore if ipcValidator is absent
  }

  let dbWorker = null;
  let lastInitMsg = null;

  function ensureDbWorker() {
    if (dbWorker && dbWorker.connected) return dbWorker;
    try {
      dbWorker = fork(__filename, ["--db-worker"], {
        execPath: process.env.TEAMS_LOGGER_NODE || "node",
        stdio: ["ignore", "inherit", "inherit", "ipc"],
      });
      dbWorker.on("message", (msg) => {
        if (!msg) return;
        if (msg.type === "init") {
          lastInitMsg = msg;
        }
        for (const wc of webContents.getAllWebContents()) {
          try {
            wc.send("teams-logger-db", msg);
          } catch {
            // ignore destroyed webContents
          }
        }
      });
      dbWorker.on("exit", () => {
        dbWorker = null;
      });
    } catch (err) {
      console.error("[message-logger] Main failed to fork db-worker:", err);
    }
    return dbWorker;
  }

  ensureDbWorker();

  ipcMain.on("teams-logger-db", (event, payload) => {
    if (!payload || typeof payload !== "object") return;
    const worker = ensureDbWorker();
    if (payload.type === "request-init") {
      if (lastInitMsg) {
        try {
          event.sender.send("teams-logger-db", lastInitMsg);
        } catch {
          // ignore
        }
      } else if (worker && worker.connected) {
        worker.send({ type: "request-init" });
      }
      return;
    }
    if (worker && worker.connected) {
      worker.send(payload);
    }
  });

  function sendSyncAction(action) {
    for (const wc of webContents.getAllWebContents()) {
      wc.send("full-archive-sync", action);
    }
  }

  module.exports = (Menus) => {
    const menu = origAppMenu(Menus);
    menu.submenu.unshift({
      label: "Archive Sync",
      submenu: [
        {
          label: "Sync Current Channel",
          click: () => sendSyncAction("sync-current"),
        },
        {
          label: "Re-sync",
          click: () => sendSyncAction("resync"),
        },
        {
          label: "Full Sync",
          click: () => sendSyncAction("start"),
        },
        {
          label: "Pause / Resume",
          click: () => sendSyncAction("pause"),
        },
        {
          label: "Cancel",
          click: () => sendSyncAction("cancel"),
        },
      ],
    });
    return menu;
  };
  return;
}

// --- Renderer Process (Non-blocking in-memory cache + IPC queue to Main's DB Worker) ---
const { ipcRenderer } = require("electron");

const authorCache = new Map();
const channelCache = new Map();

let pendingAuthors = new Map();
let pendingChannels = new Map();
let pendingRecords = [];
let pendingLastSyncs = new Map();
let pendingGroupNameRequests = new Map();
let flushTimer = null;
let dbBridgeInitialized = false;

function initDbBridge() {
  if (dbBridgeInitialized) return;
  dbBridgeInitialized = true;

  ipcRenderer.on("teams-logger-db", (_event, msg) => {
    if (!msg) return;
    if (msg.type === "init") {
      for (const a of msg.authors || []) {
        if (a.id && a.name) {
          authorCache.set(
            a.id,
            chooseBetterAuthorName(authorCache.get(a.id), a.name)
          );
        }
      }
      for (const c of msg.channels || []) {
        if (!c.id) continue;
        const cur = channelCache.get(c.id);
        channelCache.set(c.id, {
          name: c.name ?? cur?.name ?? null,
          team_name: c.team_name ?? cur?.team_name ?? null,
          type: c.type ?? cur?.type ?? null,
          last_sync_ms: Math.max(
            c.last_sync_ms || 0,
            cur?.last_sync_ms || 0
          ) || null,
          max_id: Math.max(c.max_id || 0, cur?.max_id || 0) || null,
        });
      }
      seenStateByMid.clear();
      runIdle(() => syncAll());
    } else if (msg.type === "group-names") {
      for (const u of msg.updates || []) {
        const cur = channelCache.get(u.channelId);
        if (cur && u.name) {
          cur.name = u.name;
        }
      }
    }
  });

  try {
    ipcRenderer.send("teams-logger-db", { type: "request-init" });
  } catch (err) {
    console.warn("[message-logger] request-init failed:", err.message);
  }
}

function scheduleDbFlush() {
  if (flushTimer) return;
  flushTimer = setTimeout(() => {
    flushTimer = null;
    flushToDbWorker();
  }, 200);
}

function flushToDbWorker() {
  if (
    pendingAuthors.size === 0 &&
    pendingChannels.size === 0 &&
    pendingRecords.length === 0 &&
    pendingLastSyncs.size === 0 &&
    pendingGroupNameRequests.size === 0
  ) {
    return;
  }
  initDbBridge();

  const payload = {
    type: "flush",
    authors: [...pendingAuthors.values()],
    channels: [...pendingChannels.values()],
    records: pendingRecords,
    lastSyncs: [...pendingLastSyncs.values()],
    groupNameRequests: [...pendingGroupNameRequests.values()],
  };
  pendingAuthors = new Map();
  pendingChannels = new Map();
  pendingRecords = [];
  pendingLastSyncs = new Map();
  pendingGroupNameRequests = new Map();

  try {
    ipcRenderer.send("teams-logger-db", payload);
  } catch (err) {
    console.warn("[message-logger] IPC send to DB worker failed:", err.message);
  }
}

const db = {
  exec() {
    // Handled inside DB worker transactions
  },
};

const selectChannelLastSync = {
  get(channelId) {
    const entry = channelCache.get(channelId);
    return entry ? { last_sync_ms: entry.last_sync_ms } : null;
  },
};

const selectChannelMaxMsgId = {
  get(channelId) {
    const entry = channelCache.get(channelId);
    return entry ? { max_id: entry.max_id } : null;
  },
};

const updateChannelLastSync = {
  run(syncTs, channelId) {
    if (!channelId || !syncTs) return;
    let entry = channelCache.get(channelId);
    if (!entry) {
      entry = {
        name: null,
        team_name: null,
        type: null,
        last_sync_ms: syncTs,
        max_id: null,
      };
      channelCache.set(channelId, entry);
    } else if (!entry.last_sync_ms || entry.last_sync_ms < syncTs) {
      entry.last_sync_ms = syncTs;
    }
    const prev = pendingLastSyncs.get(channelId);
    if (!prev || prev.syncTs < syncTs) {
      pendingLastSyncs.set(channelId, { channelId, syncTs });
      scheduleDbFlush();
    }
  },
};

const upsertAuthor = {
  run(id, name) {
    if (!id) return;
    const better = chooseBetterAuthorName(authorCache.get(id), name);
    authorCache.set(id, better);
    pendingAuthors.set(id, { id, name: better });
    scheduleDbFlush();
  },
};

const selectChannelInfo = {
  get(channelId) {
    return channelCache.get(channelId) || null;
  },
};

const upsertChannel = {
  run(channelId, rawName, channelType, rawTeamName = null) {
    if (!channelId) return;
    let name = normalizeChannelName(rawName);
    const teamName = normalizeChannelName(rawTeamName);
    const existing = channelCache.get(channelId);
    const existingName = normalizeChannelName(existing?.name);
    if (
      (channelType === "group chat" || channelId.endsWith("@thread.v2")) &&
      shouldKeepExistingGroupChannelName(existingName, name)
    ) {
      name = existingName;
    } else if (
      existingName &&
      teamName &&
      name === teamName &&
      existingName !== teamName
    ) {
      name = existingName;
    }
    const mergedName = name ?? existing?.name ?? null;
    const mergedTeam = teamName ?? existing?.team_name ?? null;
    const mergedType = channelType ?? existing?.type ?? null;
    channelCache.set(channelId, {
      name: mergedName,
      team_name: mergedTeam,
      type: mergedType,
      last_sync_ms: existing?.last_sync_ms ?? null,
      max_id: existing?.max_id ?? null,
    });
    pendingChannels.set(channelId, {
      id: channelId,
      name: mergedName,
      teamName: mergedTeam,
      type: mergedType,
    });
    scheduleDbFlush();
  },
};

function storeRecord({
  id,
  threadId,
  channelId,
  channelName,
  teamName,
  channelType,
  direction,
  authorId,
  authorName,
  content,
  reactions,
  mentions,
}) {
  if (channelId) {
    upsertChannel.run(
      channelId,
      channelName ?? null,
      channelType ?? null,
      teamName ?? null
    );
    const ch = channelCache.get(channelId);
    if (ch && (!ch.max_id || id > ch.max_id)) {
      ch.max_id = id;
    }
  }
  if (authorId) {
    upsertAuthor.run(authorId, authorName ?? null);
  }
  if (mentions && mentions.length > 0) {
    for (const m of mentions) {
      if (m.id) upsertAuthor.run(m.id, m.name ?? null);
    }
  }
  const reactionsSummary = formatReactionsSummary(reactions);
  pendingRecords.push({
    id,
    threadId: threadId ?? null,
    channelId: channelId ?? null,
    channelName: channelName ?? null,
    teamName: teamName ?? null,
    channelType: channelType ?? null,
    direction,
    authorId: authorId ?? null,
    authorName: authorName ?? null,
    content: content ?? "",
    reactionsSummary,
    reactions: reactions && reactions.length > 0 ? reactions : null,
    mentions: mentions && mentions.length > 0 ? mentions : null,
  });
  scheduleDbFlush();
}

function inferMyAuthorIdFromChannelIds(channelIds) {
  const counts = new Map();
  for (const cid of channelIds) {
    const m = cid?.match(DIRECT_CHAT_RE);
    if (!m) continue;
    for (const raw of [m[1], m[2]]) {
      const norm = normalizeAuthorId(raw);
      if (norm) counts.set(norm, (counts.get(norm) || 0) + 1);
    }
  }
  let bestId = null;
  let bestCount = 1;
  for (const [id, count] of counts) {
    if (count > bestCount) {
      bestId = id;
      bestCount = count;
    }
  }
  return bestId;
}

const selectAuthorName = {
  get(id) {
    const name = authorCache.get(id);
    return name ? { name } : null;
  },
};

const selectCanonicalChannelByName = {
  get(name) {
    if (!name) return null;
    let fallbackId = null;
    const suffix = `: ${name}`;
    for (const [id, info] of channelCache) {
      if (!isCanonicalChannelId(id) || !info?.name) continue;
      if (info.name === name) return { id };
      if (!fallbackId && info.name.endsWith(suffix)) fallbackId = id;
    }
    return fallbackId ? { id: fallbackId } : null;
  },
};

let cachedCurrentUser = null;

function getCurrentUser(extraChannelIds = []) {
  const coreServices =
    globalThis.window?.teamsForLinuxReactHandler?._getTeams2CoreServices?.();
  const authUser = coreServices?.authenticationUser;
  const rawId = authUser?.id || authUser?.profile?.oid || null;
  let id = normalizeAuthorId(rawId) || cachedCurrentUser?.id || null;
  let name =
    authUser?.displayName ||
    authUser?.profile?.name ||
    cachedCurrentUser?.name ||
    null;

  if (!id) {
    const dbChannelIds = [];
    for (const cid of channelCache.keys()) {
      if (cid.endsWith("@unq.gbl.spaces")) dbChannelIds.push(cid);
    }
    id = inferMyAuthorIdFromChannelIds([...dbChannelIds, ...extraChannelIds]);
  }
  if (id && !name) {
    name = selectAuthorName.get(id)?.name || null;
  }
  if (id) {
    cachedCurrentUser = { id, name };
  }
  return { id, name };
}

function formatDurationSeconds(sec) {
  if (! Number.isFinite(sec) || sec <= 0) return "";
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  const parts = [];
  if (h > 0) parts.push(`${h}h`);
  if (m > 0) parts.push(`${m}m`);
  if (s > 0 || parts.length === 0) parts.push(`${s}s`);
  return parts.join(" ");
}

function resolvePersonName(rawId, fallback = null) {
  const id = normalizeAuthorId(rawId);
  if (!id) return fallback || null;
  if (id.startsWith("28:app:")) return fallback || "app";
  if (id.startsWith("28:")) return fallback || "bot";
  const row = selectAuthorName.get(id);
  if (row?.name) return row.name;
  return fallback || id;
}

const PARTLIST_ENTRY_RE =
  /(8:orgid:[0-9a-f-]{36}|8:live:\.cid\.[0-9a-f]+|8:live:[a-z0-9_.-]+|8:teamsvisitor:[0-9a-f]{32}|28:(?:app:)?[0-9a-f_-]{36,73})([^0-9\n]*?)(\d+)(?=8:orgid:|8:live:|8:teamsvisitor:|28:|[A-Za-z0-9._%+-]+@|\r|\n|$)/gi;

function formatEventContent(rawContent, msgType = "") {
  if (!rawContent) return "";
  const text = String(rawContent).trim();
  if (!text) return "";

  // 1. Raw XML payloads from ChatSvc API
  if (text.startsWith("<")) {
    if (/<callEventType>\s*callStarted\s*<\/callEventType>/i.test(text) || /<partlist\b[^>]*type="callStarted"/i.test(text)) {
      const org = text.match(/<Organizer>([^<]+)<\/Organizer>/i)?.[1]?.trim();
      return org ? `Call started (${org})` : "Call started";
    }
    if (/<callEventType>\s*callEnded\s*<\/callEventType>/i.test(text) || /<partlist\b/i.test(text)) {
      const parts = [];
      const seen = new Set();
      for (const m of text.matchAll(/<part\b[^>]*>([\s\S]*?)<\/part>/gi)) {
        const inner = m[1];
        const rawId = inner.match(/<name>([^<]+)<\/name>/i)?.[1]?.trim();
        const disp = inner.match(/<displayName>([^<]+)<\/displayName>/i)?.[1]?.trim();
        const dur = Number(inner.match(/<duration>(\d+)<\/duration>/i)?.[1] || 0);
        const normId = normalizeAuthorId(rawId);
        if (normId && disp && !normId.startsWith("28:") && !normId.startsWith("4:")) {
          upsertAuthor.run(normId, disp);
        }
        const name = resolvePersonName(rawId, disp);
        if (!name || name === "bot" || name === "app" || seen.has(name)) continue;
        seen.add(name);
        const durStr = formatDurationSeconds(dur);
        parts.push(durStr ? `${name} (${durStr})` : name);
      }
      return parts.length > 0 ? `Call ended — ${parts.join(", ")}` : "Call ended";
    }
    if (/<(?:addmember|deletemember|roleupdate)\b/i.test(text)) {
      const isDelete = /<deletemember\b/i.test(text);
      const isRole = /<roleupdate\b/i.test(text);
      const action = isDelete
        ? "removed"
        : isRole
          ? "updated role for"
          : "added";
      const initiatorId = text.match(/<initiator>([^<]+)<\/initiator>/i)?.[1]?.trim();
      const initiator = resolvePersonName(initiatorId);
      const allTargetNames = [];
      const targets = [];
      for (const tm of text.matchAll(/<target>([^<]+)<\/target>/gi)) {
        const name = resolvePersonName(tm[1].trim());
        if (name && !allTargetNames.includes(name)) allTargetNames.push(name);
        if (name && name !== initiator && !targets.includes(name)) targets.push(name);
      }
      if (initiator && targets.length > 0) return `${initiator} ${action} ${targets.join(", ")}`;
      if (targets.length > 0) return `Members ${action}: ${targets.join(", ")}`;
      if (isDelete && initiator && allTargetNames.includes(initiator)) return `${initiator} left`;
      if (initiator && allTargetNames.length > 0) return `${initiator} ${action} ${allTargetNames.join(", ")}`;
    }
    if (/<topicupdate\b/i.test(text)) {
      const initiator = resolvePersonName(text.match(/<initiator>([^<]+)<\/initiator>/i)?.[1]);
      const val = htmlToText(text.match(/<value>([\s\S]*?)<\/value>/i)?.[1] || "");
      return initiator ? `${initiator} changed topic to "${val}"` : `Topic changed to "${val}"`;
    }
  }

  // 2. JSON event payloads
  if (text.startsWith("{")) {
    try {
      const obj = JSON.parse(text);
      if (Array.isArray(obj.members)) {
        const initiator = resolvePersonName(obj.initiator);
        const members = obj.members
          .map((m) => resolvePersonName(m.id, m.friendlyname?.trim() || null))
          .filter((n) => n && n !== initiator);
        if (initiator && members.length > 0) return `${initiator} added ${members.join(", ")}`;
        if (members.length > 0) return `Added ${members.join(", ")}`;
      }
      if (typeof obj.topic === "string") {
        const user = resolvePersonName(obj.user);
        return user ? `${user} changed topic to "${obj.topic}"` : `Topic changed to "${obj.topic}"`;
      }
      if ("newValue" in obj) {
        const user = resolvePersonName(obj.user);
        let summary = String(obj.newValue ?? "").trim();
        if (summary.startsWith("[")) {
          try {
            const arr = JSON.parse(summary);
            if (Array.isArray(arr)) {
              const names = arr.map((x) => x?.name).filter(Boolean);
              if (names.length > 0) {
                return user
                  ? `${user} updated channels (${names.join(", ")})`
                  : `Updated channels (${names.join(", ")})`;
              }
            }
          } catch {
            // keep raw summary
          }
        }
        summary = summary.replace(/\s+/g, " ");
        return user ? `${user} updated description: "${summary}"` : `Updated description: "${summary}"`;
      }
    } catch {
      // Fall through to legacy stripped XML patterns
    }
  }

  // 3. Stripped XML callStarted
  if (/(?:^|false|true|Adhoc|Scheduled|Recurring|Occurrence)callStarted\b/i.test(text)) {
    const orgMatch = text.match(/([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})(?:Scheduled|Recurring|Adhoc)/i);
    return orgMatch ? `Call started (${orgMatch[1]})` : "Call started";
  }

  // 4. Stripped XML callEnded / partlist with participant durations
  if (
    text.includes("callEnded") ||
    /^(?:8:orgid:|8:live:|8:teamsvisitor:|28:)/.test(text) ||
    /^\d{13}(?:8:orgid:|19:)/.test(text)
  ) {
    // Check if it's a member-add/update event containing 13-digit epoch timestamps and no names
    const withoutTimestamps = text.replace(/\d{13}/g, " ");
    const idMatches = [
      ...withoutTimestamps.matchAll(
        /(8:orgid:[0-9a-f-]{36}|8:teamsvisitor:[0-9a-f]{32}|28:app:[0-9a-f_-]+|28:[0-9a-f-]{36})/gi
      ),
    ].map((m) => m[1]);

    if (/\d{13}/.test(text) && !text.includes("callEnded") && idMatches.length > 0) {
      const startsWithTs = /^\d{13}/.test(text);
      const initiatorRaw = startsWithTs ? idMatches[0] : idMatches[idMatches.length - 1];
      const initiator = resolvePersonName(initiatorRaw);
      const targets = [];
      for (const raw of idMatches) {
        const name = resolvePersonName(raw);
        if (name && name !== initiator && !targets.includes(name)) targets.push(name);
      }
      if (initiator && targets.length > 0) return `${initiator} added ${targets.join(", ")}`;
      if (
        initiator &&
        idMatches.length === 2 &&
        normalizeAuthorId(idMatches[0]) === normalizeAuthorId(idMatches[1])
      ) {
        return `${initiator} left`;
      }
      if (initiator) return `${initiator} updated conversation`;
    }

    const parts = [];
    const seen = new Set();
    PARTLIST_ENTRY_RE.lastIndex = 0;
    for (const m of text.matchAll(PARTLIST_ENTRY_RE)) {
      const rawId = m[1];
      const inlineName = m[2]?.trim() || null;
      const dur = Number(m[3]);
      if (rawId.startsWith("28:")) continue;
      const name = resolvePersonName(rawId, inlineName);
      if (!name || seen.has(name)) continue;
      seen.add(name);
      const durStr = formatDurationSeconds(dur);
      parts.push(durStr ? `${name} (${durStr})` : name);
    }
    if (parts.length > 0 || text.includes("callEnded")) {
      return parts.length > 0 ? `Call ended — ${parts.join(", ")}` : "Call ended";
    }
  }

  return msgType && !msgType.startsWith("RichText") && msgType !== "Text"
    ? htmlToText(text)
    : text;
}
function getContentEl(mid) {
  return (
    document.getElementById(`content-${mid}`) ||
    document.getElementById(`content-control-message-${mid}`) ||
    document.getElementById(`control-message-${mid}`)
  );
}

function getSubjectText(msg, mid) {
  const subjEl =
    document.getElementById(`subject-line-${mid}`) ||
    msg.querySelector?.('[data-tid="subject-line"]');
  if (subjEl?.innerText?.trim()) return subjEl.innerText.trim();
  const fiberMsg = getFiberMessage(msg);
  const raw =
    fiberMsg?.subject ||
    fiberMsg?.properties?.subject ||
    fiberMsg?.properties?.title ||
    "";
  return typeof raw === "string" ? raw.trim() : "";
}

const BLOCK_TAGS = new Set([
  "DIV",
  "P",
  "LI",
  "TR",
  "H1",
  "H2",
  "H3",
  "H4",
  "H5",
  "H6",
  "BLOCKQUOTE",
  "PRE",
]);

function isInlineMentionNode(node) {
  return (
    node.hasAttribute?.("data-mention-type") ||
    node.hasAttribute?.("data-person-mri") ||
    node.getAttribute?.("itemtype") === "http://schema.skype.com/Mention"
  );
}

function formatQuoteBlock(author, time, preview) {
  const lines = [];
  if (author) {
    lines.push(`> ${author}${time ? ` (${time})` : ""}:`);
  }
  if (preview) {
    for (const line of preview.split("\n")) {
      lines.push(`> ${line}`);
    }
  }
  return lines.length > 0 ? `${lines.join("\n")}\n` : "";
}

function normalizeCodeLang(raw) {
  if (!raw) return "";
  const lower = raw.trim().toLowerCase();
  if (lower === "plaintext" || lower === "plain text" || lower === "none" || lower === "text") {
    return "";
  }
  return lower;
}

function extractDomCodeText(root) {
  let out = "";
  function walkCode(node) {
    if (node.nodeType === 3) {
      out += node.nodeValue;
      return;
    }
    if (node.nodeType !== 1) return;
    if (node.getAttribute("data-tid") === "code-block-line-numbers") return;
    if (node.tagName === "BR") {
      out += "\n";
      return;
    }
    for (const child of node.childNodes) {
      walkCode(child);
    }
  }
  walkCode(root);
  return out.replace(/\u00a0/g, " ").replace(/\r\n?/g, "\n").replace(/\n+$/, "");
}

function extractDomTextWithEmojis(root) {
  if (!root) return "";
  let out = "";
  function walk(node) {
    if (node.nodeType === 3) {
      out += node.nodeValue;
      return;
    }
    if (node.nodeType !== 1) return;
    const tid = node.getAttribute("data-tid");
    if (
      tid === "code-block-editor-deserialized-header" ||
      tid === "code-block-line-numbers"
    ) {
      return;
    }
    const tag = node.tagName;
    if (tag === "BR") {
      out += "\n";
      return;
    }
    if (tag === "IMG") {
      const alt = node.getAttribute("alt") || node.getAttribute("itemid") || "";
      out += alt;
      return;
    }
    if (tag === "PRE") {
      const langMatch = node.className?.match?.(/\blanguage-([a-z0-9_+-]+)/i);
      const lang = normalizeCodeLang(langMatch?.[1]);
      const code = extractDomCodeText(node);
      if (out && !out.endsWith("\n")) out += "\n";
      out += `\`\`\`${lang}\n${code}\n\`\`\`\n`;
      return;
    }
    if (tag === "CODE") {
      out += `\`${extractDomCodeText(node)}\``;
      return;
    }
    if (
      node.getAttribute("data-message-object") === "quoted-reply" ||
      tid === "quoted-reply-card"
    ) {
      const card =
        tid === "quoted-reply-card"
          ? node
          : node.querySelector('[data-tid="quoted-reply-card"]') || node;
      const timeEl = card.querySelector('[data-tid="quoted-reply-timestamp"]');
      const previewEl = card.querySelector(
        '[data-tid="quoted-reply-preview-content"]'
      );
      let author = "";
      for (const span of card.querySelectorAll("span")) {
        if (span === timeEl || span === previewEl || previewEl?.contains(span)) {
          continue;
        }
        const t = span.textContent?.trim();
        if (t) {
          author = t;
          break;
        }
      }
      const time = timeEl?.textContent?.trim() || "";
      const preview = extractDomTextWithEmojis(previewEl);
      if (out && !out.endsWith("\n")) out += "\n";
      out += formatQuoteBlock(author, time, preview);
      return;
    }
    const inlineMention = isInlineMentionNode(node);
    for (const child of node.childNodes) {
      walk(child);
    }
    if (inlineMention && node.hasAttribute?.("data-person-mri") && !/\s$/.test(out)) {
      out += " ";
    } else if (!inlineMention && BLOCK_TAGS.has(tag) && !out.endsWith("\n")) {
      out += "\n";
    }
  }
  walk(root);
  return out
    .replace(/\u00a0/g, " ")
    .replace(/\r\n?/g, "\n")
    .replace(/[ \t]+\n/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

function mergeMentionTokens(tokens) {
  const byId = new Map();
  let prevId = null;
  let prevParts = [];
  const flush = () => {
    if (!prevId) return;
    const candidate = prevParts.join(" ").trim() || null;
    const existing = byId.get(prevId);
    if (
      !existing ||
      (candidate && candidate.length > (existing.name?.length || 0))
    ) {
      byId.set(prevId, { id: prevId, name: candidate });
    }
    prevId = null;
    prevParts = [];
  };
  for (const { id, name } of tokens) {
    if (!id) continue;
    if (id !== prevId) {
      flush();
      prevId = id;
    }
    if (name && !prevParts.includes(name)) {
      prevParts.push(name);
    }
  }
  flush();
  return [...byId.values()];
}

function extractDomMentions(contentEl) {
  if (!contentEl) return [];
  const tokens = [];
  for (const el of contentEl.querySelectorAll("[data-person-mri]")) {
    const id = normalizeAuthorId(el.getAttribute("data-person-mri"));
    if (!id) continue;
    const textEl =
      el.querySelector('[itemtype="http://schema.skype.com/Mention"]') || el;
    const name = textEl.textContent?.trim() || null;
    tokens.push({ id, name });
  }
  return mergeMentionTokens(tokens);
}

function extractDomReactions(msg, mid) {
  const host =
    document.getElementById(`message-body-${mid}`) ||
    msg.closest(`[id="message-body-${mid}"]`) ||
    msg.closest('[data-tid="chat-pane-item"]') ||
    msg;
  const pills = host.querySelectorAll(
    '[data-tid="diverse-reaction-pill-button"]'
  );
  const reactions = [];
  for (const pill of pills) {
    const img = pill.querySelector("img");
    const emoji = img?.getAttribute("alt") || null;
    const key =
      img?.getAttribute("itemid") ||
      emoji ||
      pill.getAttribute("aria-labelledby") ||
      "reaction";
    const labelEl = pill.querySelector(`[id^="message-"][id$="-${mid}"]`);
    const labelText = labelEl?.textContent?.trim() || pill.textContent?.trim() || "";
    const countMatch = labelText.match(/^(\d+)/);
    const count = countMatch ? Number(countMatch[1]) : 1;
    reactions.push({ key, emoji: emoji || key, count });
  }
  return reactions;
}

function isEventMessage(msg, mid) {
  return (
    msg.dataset.tid === "control-message-renderer" ||
    Boolean(
      document.getElementById(`content-control-message-${mid}`) ||
        document.getElementById(`control-message-${mid}`)
    )
  );
}

function isReadyMessage(msg) {
  const mid = msg.dataset.mid;
  // Valid Teams message IDs are 13-digit unix epoch ms (skip -1 and 19-digit optimistic clientMessageIds)
  if (!mid || !/^\d{13}$/.test(mid)) return false;
  return Boolean(
    getContentEl(mid) || document.getElementById(`subject-line-${mid}`)
  );
}

function extractConvIdFromFiber(el) {
  if (!el) return null;
  const key = Object.keys(el).find((k) => k.startsWith("__reactFiber$"));
  let fiber = key ? el[key] : null;
  for (let i = 0; i < 30 && fiber; i++, fiber = fiber.return) {
    const p = fiber.memoizedProps;
    if (!p) continue;
    const m = p.message || p.item;
    const candidates = [
      p.conversationId,
      p.convId,
      p.threadId,
      p.chatId,
      p.channelId,
      p.entityId,
      p.id,
      p.chat?.id,
      p.channel?.id,
      m?.conversationId,
      m?.threadId,
      m?.conversationLink,
      m?.id,
    ];
    for (const c of candidates) {
      if (typeof c !== "string") continue;
      if (isCanonicalChannelId(c)) return c;
      const match = c.match(/(?:19:[^/;,\s"']+|48:notes)/);
      if (match) return match[0];
    }
  }
  return null;
}

function getChannelId(pane, channelName = null) {
  const header =
    pane.querySelector('[id^="chat-header-"]') ||
    document.querySelector('[id^="chat-header-"]');
  const headerId = header?.id?.slice("chat-header-".length);
  if (isCanonicalChannelId(headerId)) return headerId;

  const convEl =
    pane.querySelector("[data-convid]") || document.querySelector("[data-convid]");
  const convId = convEl?.getAttribute("data-convid");
  if (isCanonicalChannelId(convId)) return convId;

  const fiberConvId =
    extractConvIdFromFiber(pane) ||
    extractConvIdFromFiber(pane.querySelector(MESSAGE_SELECTOR));
  if (isCanonicalChannelId(fiberConvId)) return fiberConvId;

  const viewport = pane.querySelector("[data-track-databag-id]");
  const databagId = viewport?.getAttribute("data-track-databag-id");
  if (isCanonicalChannelId(databagId)) return databagId;

  if (channelName) {
    const existing = selectCanonicalChannelByName.get(
      channelName,
      channelName,
      channelName
    );
    if (existing?.id) return existing.id;
    return channelName;
  }

  return document.title || "unknown";
}

function getFiberChat(el) {
  if (!el) return null;
  const key = Object.keys(el).find((k) => k.startsWith("__reactFiber$"));
  let fiber = key ? el[key] : null;
  for (let i = 0; i < 25 && fiber; i++, fiber = fiber.return) {
    const p = fiber.memoizedProps;
    if (
      p?.chat &&
      (Array.isArray(p.chat.memberIds) ||
        Array.isArray(p.chat.membersLimited) ||
        p.chat.title ||
        p.chat.id)
    ) {
      return p.chat;
    }
  }
  return null;
}

function getChannelName(pane) {
  const titleEl =
    pane.querySelector(TITLE_SELECTOR) || document.querySelector(TITLE_SELECTOR);
  const headerEl =
    pane.querySelector('[id^="chat-header-"]') ||
    document.querySelector('[id^="chat-header-"]');
  const fiberChat = getFiberChat(titleEl) || getFiberChat(headerEl);

  if (Array.isArray(fiberChat?.membersLimited)) {
    for (const m of fiberChat.membersLimited) {
      const mId = normalizeAuthorId(m?.id || m?.mri);
      const mName = m?.displayName?.trim();
      if (mId && mName && isPersonMri(mId)) {
        upsertAuthor.run(mId, mName);
      }
    }
  }

  if (fiberChat?.title?.trim()) {
    return normalizeChannelName(fiberChat.title);
  }

  if (
    Array.isArray(fiberChat?.memberIds) &&
    fiberChat.memberIds.length > 2 &&
    !fiberChat.isMeeting
  ) {
    const me = getCurrentUser();
    const personIds = fiberChat.memberIds
      .map((raw) => normalizeAuthorId(raw))
      .filter((id) => isPersonMri(id) && (!me.id || id !== me.id));
    if (personIds.length > 0) {
      const resolved = dedupeParticipantNames(
        personIds.map((id) => resolvePersonName(id)),
        me
      );
      if (resolved.length === personIds.length) {
        return resolved.join(", ");
      }
    }
  }

  if (titleEl) {
    const customEl = titleEl.querySelector?.("#custom-chat-topic-renderer");
    if (customEl?.textContent?.trim()) {
      return normalizeChannelName(customEl.textContent);
    }
    const personEls = titleEl.querySelectorAll?.('[id^="chat-topic-person-"]');
    if (personEls && personEls.length > 0) {
      const me = getCurrentUser();
      const names = [];
      for (const pel of personEls) {
        const rawMri = pel.id.slice("chat-topic-person-".length);
        const normId = normalizeAuthorId(rawMri);
        const fallback = pel.textContent?.trim() || null;
        const resolved = normId ? resolvePersonName(normId, fallback) : fallback;
        if (resolved && !resolved.startsWith("8:")) {
          names.push(resolved);
        } else if (fallback) {
          names.push(fallback);
        }
      }
      const deduped = dedupeParticipantNames(names, me);
      if (deduped.length > 0) return deduped.join(", ");
    }
    if (titleEl.innerText?.trim()) {
      return normalizeChannelName(titleEl.innerText);
    }
  }

  return normalizeChannelName(
    document.title
      .replace(/^\(\d+\)\s*/, "")
      .replace(/\s*\|\s*Microsoft Teams$/i, "")
  );
}

function classifyChannelType(
  channelId,
  hasChannelTitle = false,
  hasParticipantCount = false,
  threadProps = null
) {
  if (
    hasChannelTitle ||
    channelId?.endsWith("@thread.tacv2") ||
    threadProps?.productThreadType === "Channels" ||
    threadProps?.threadType === "topic" ||
    threadProps?.threadType === "space"
  ) {
    return "channel";
  }
  if (channelId?.includes("meeting_") || threadProps?.threadType === "meeting") {
    return "meeting chat";
  }
  if (channelId?.endsWith("@unq.gbl.spaces") || channelId === "48:notes") {
    return "direct chat";
  }
  if (hasParticipantCount || channelId?.endsWith("@thread.v2")) {
    return "group chat";
  }
  return "direct chat";
}

function getChannelType(pane, channelId) {
  const hasChannelTitle = Boolean(
    pane.querySelector('[data-tid="channelTitle-text"]') ||
      document.querySelector('[data-tid="channelTitle-text"]')
  );
  const hasParticipantCount = Boolean(
    pane.querySelector('[data-tid="chat-header-participant-count"]') ||
      document.querySelector('[data-tid="chat-header-participant-count"]')
  );
  return classifyChannelType(channelId, hasChannelTitle, hasParticipantCount);
}

function extractParentSpaceId(rawParentSpaces) {
  if (!rawParentSpaces) return null;
  if (typeof rawParentSpaces === "string") {
    try {
      const parsed = JSON.parse(rawParentSpaces);
      const id = parsed?.linkedSpaceInfoItems?.[0]?.spaceThreadId;
      if (isCanonicalChannelId(id)) return id;
    } catch {
      // ignore malformed JSON
    }
  } else if (typeof rawParentSpaces === "object") {
    const id = rawParentSpaces?.linkedSpaceInfoItems?.[0]?.spaceThreadId;
    if (isCanonicalChannelId(id)) return id;
  }
  return null;
}

function getConvTeamInfo(conv) {
  const tp = conv?.threadProperties;
  if (!tp) return { teamId: conv?.teamId || null, teamName: null, groupId: null };
  const parentSpaceId = extractParentSpaceId(tp.parentSpaces);
  const spaceId = isCanonicalChannelId(tp.spaceId) ? tp.spaceId : null;
  const teamId =
    parentSpaceId ||
    spaceId ||
    (isCanonicalChannelId(conv?.teamId) ? conv.teamId : null) ||
    (tp.threadType === "space" && isCanonicalChannelId(conv?.id)
      ? conv.id
      : null);
  const isRootTeamSpace =
    !parentSpaceId &&
    (!spaceId || spaceId === conv?.id) &&
    tp.threadType === "space";
  const teamName = isRootTeamSpace
    ? normalizeChannelName(tp.spaceThreadTopic)
    : null;
  return { teamId, teamName, groupId: tp.groupId || null };
}

function getTeamName(pane, channelId, channelType) {
  if (channelType !== "channel" && !channelId?.endsWith("@thread.tacv2")) {
    return null;
  }
  const els = [
    pane.querySelector('[data-tid="channelTitle-text"]') ||
      document.querySelector('[data-tid="channelTitle-text"]'),
    pane.querySelector('[id^="chat-header-"]') ||
      document.querySelector('[id^="chat-header-"]'),
    pane,
  ].filter(Boolean);

  let teamId = null;
  for (const el of els) {
    const key = Object.keys(el).find((k) => k.startsWith("__reactFiber$"));
    let fiber = key ? el[key] : null;
    for (let i = 0; i < 30 && fiber; i++, fiber = fiber.return) {
      const p = fiber.memoizedProps;
      if (!p) continue;
      const nameCandidate =
        p.teamDisplayName ||
        p.owningTeamName ||
        p.teamName ||
        p.team?.displayName ||
        p.team?.name ||
        p.team?.spaceThreadTopic ||
        p.channel?.owningTeam?.displayName ||
        p.channel?.teamName ||
        p.chat?.teamName;
      if (typeof nameCandidate === "string" && nameCandidate.trim()) {
        return normalizeChannelName(nameCandidate);
      }
      if (!teamId) {
        const idCandidate =
          p.teamId ||
          p.team?.id ||
          p.channel?.owningTeam?.id ||
          p.channel?.teamId ||
          p.chat?.teamId ||
          p.chat?.spaceId ||
          p.threadProperties?.spaceId ||
          extractParentSpaceId(p.threadProperties?.parentSpaces);
        if (isCanonicalChannelId(idCandidate)) {
          teamId = idCandidate;
        }
      }
    }
  }

  const existing = channelId ? selectChannelInfo.get(channelId) : null;
  if (existing?.team_name) return existing.team_name;
  if (teamId) {
    const teamRow = selectChannelInfo.get(teamId);
    if (teamRow?.team_name) return teamRow.team_name;
    if (teamId !== channelId && teamRow?.name) return teamRow.name;
  }
  return null;
}

function getThreadId(msg) {
  const chainEl =
    msg.closest("[data-reply-chain-id]") ||
    msg.querySelector("[data-reply-chain-id]");
  const chainId = chainEl?.getAttribute("data-reply-chain-id");
  if (chainId && /^\d{13}$/.test(chainId)) return Number(chainId);
  const summary = msg.closest('[id^="reply-chain-summary-"]');
  const summaryId = summary?.id.slice("reply-chain-summary-".length);
  if (summaryId && /^\d{13}$/.test(summaryId)) return Number(summaryId);
  return null;
}

function getFiberMessage(el) {
  const key = Object.keys(el).find((k) => k.startsWith("__reactFiber$"));
  let fiber = key ? el[key] : null;
  for (let i = 0; i < 20 && fiber; i++, fiber = fiber.return) {
    const p = fiber.memoizedProps;
    const m = p?.message || p?.item;
    if (m && (m.from || m.imDisplayName || m.fromUser)) {
      return m;
    }
  }
  return null;
}

function getPeerIdFromDirectChat(channelId, myAuthorId) {
  const m = channelId?.match(
    /^19:([0-9a-f-]{36})_([0-9a-f-]{36})@unq\.gbl\.spaces$/i
  );
  if (!m) return null;
  const id1 = normalizeAuthorId(m[1]);
  const id2 = normalizeAuthorId(m[2]);
  if (myAuthorId && id1 === myAuthorId) return id2;
  if (myAuthorId && id2 === myAuthorId) return id1;
  return null;
}

function getDomAuthorInfo(msg, mid) {
  const fiberMsg = getFiberMessage(msg);
  let id = normalizeAuthorId(fiberMsg?.from || fiberMsg?.fromUser?.id);
  let name =
    fiberMsg?.imDisplayName?.trim() ||
    fiberMsg?.fromUser?.displayName?.trim() ||
    null;

  const direct = document.getElementById(`author-${mid}`);
  if (direct?.innerText?.trim()) {
    name = direct.innerText.trim();
  }
  const item =
    document.getElementById(`message-body-${mid}`) ||
    msg.closest(`[id="message-body-${mid}"]`) ||
    msg.closest('[data-tid="chat-pane-item"]') ||
    msg;
  if (!name) {
    const authorEl = item?.querySelector(
      '[data-tid="message-author-name"], [id^="author-"]'
    );
    if (authorEl?.innerText?.trim()) {
      name = authorEl.innerText.trim();
    }
  }
  if (!id) {
    const mriEl = item?.querySelector(
      '[data-tid*="avatar"][data-person-mri], [data-person-mri]:not([data-mention-type])'
    );
    if (mriEl) {
      id = normalizeAuthorId(mriEl.getAttribute("data-person-mri"));
    }
  }
  const img = item?.querySelector('img[src*="8:orgid:"]');
  const src = img?.getAttribute("src") || "";
  if (!id) {
    const match = src.match(/8:orgid:[0-9a-f-]{36}/i);
    if (match) id = match[0].toLowerCase();
  }
  if (!name && src.includes("displayname=")) {
    try {
      const displayName = new URL(src).searchParams.get("displayname")?.trim();
      if (displayName) name = displayName;
    } catch {
      // ignore malformed URL
    }
  }
  return { id, name };
}

function computeGroupChatNameFromDb(channelId, me = null) {
  if (!isCanonicalChannelId(channelId)) return null;
  pendingGroupNameRequests.set(channelId, { channelId, me });
  scheduleDbFlush();
  return channelCache.get(channelId)?.name || null;
}

const seenStateByMid = new Map();
let pendingOptimisticRetryTimer = null;

function syncPane(pane) {
  const channelName = getChannelName(pane);
  const channelId = getChannelId(pane, channelName);
  const channelType = getChannelType(pane, channelId);
  const teamName = getTeamName(pane, channelId, channelType);
  const me = getCurrentUser();
  const peerId =
    channelType === "direct chat"
      ? getPeerIdFromDirectChat(channelId, me.id)
      : null;
  const lastAuthorByThread = new Map();
  let newestDomMid = 0;
  let updatedAnyMessage = false;
  let hasOptimisticMessage = false;

  for (const msg of pane.querySelectorAll(MESSAGE_SELECTOR)) {
    const mid = msg.dataset.mid;
    if (!isReadyMessage(msg)) {
      if (mid && !/^\d{13}$/.test(mid)) {
        hasOptimisticMessage = true;
      }
      continue;
    }

    const id = Number(mid);
    if (id > newestDomMid) newestDomMid = id;
    const threadId = getThreadId(msg);
    const threadKey = threadId ?? "main";

    let direction = "received";
    if (isEventMessage(msg, mid)) {
      direction = "event";
    } else if (document.getElementById(`read-status-icon-${mid}`)) {
      direction = "sent";
    }

    let authorId = null;
    let authorName = null;
    if (direction !== "event") {
      const domAuthor = getDomAuthorInfo(msg, mid);
      const prev = lastAuthorByThread.get(threadKey);

      if (direction === "sent") {
        authorId = domAuthor.id || me.id || prev?.id || "me";
        authorName = domAuthor.name || me.name || prev?.name || "me";
      } else {
        authorName =
          domAuthor.name ||
          prev?.name ||
          (channelType === "direct chat" ? channelName : null);
        authorId =
          domAuthor.id ||
          (!domAuthor.name && prev?.id ? prev.id : null) ||
          (channelType === "direct chat" ? peerId : null) ||
          authorName;
      }

      if (authorId || authorName) {
        lastAuthorByThread.set(threadKey, { id: authorId, name: authorName });
      }
    }

    const contentEl = getContentEl(mid);
    const subject = getSubjectText(msg, mid);
    let body = extractDomTextWithEmojis(contentEl);
    if (!body) {
      const fiberMsg = getFiberMessage(msg);
      if (typeof fiberMsg?.content === "string" && fiberMsg.content.trim()) {
        body = htmlToText(fiberMsg.content);
      }
    }
    if (direction === "event" || /^(?:callStarted|callEnded)\b/.test(body)) {
      if (!subject) {
        direction = "event";
        authorId = null;
        authorName = null;
      }
      body = formatEventContent(body);
    }
    const content =
      subject && body && body !== subject
        ? `${subject}\n${body}`
        : subject || body;
    const reactions = extractDomReactions(msg, mid);
    const mentions = extractDomMentions(contentEl);
    const mentionKey = mentions.map((m) => m.id).join(",");
    const stateKey = `${authorId ?? ""}|${content}|${formatReactionsSummary(reactions) ?? ""}|${mentionKey}`;
    if (seenStateByMid.get(mid) === stateKey) continue;

    storeRecord({
      id,
      threadId,
      channelId,
      channelName,
      teamName,
      channelType,
      direction,
      authorId,
      authorName,
      content,
      reactions,
      mentions,
    });
    seenStateByMid.set(mid, stateKey);
    updatedAnyMessage = true;
  }
  if (channelId && updatedAnyMessage) {
    let refreshedName = getChannelName(pane);
    const hasCustomTopic = Boolean(
      pane.querySelector("#custom-chat-topic-renderer") ||
        document.getElementById("custom-chat-topic-renderer")
    );
    if (channelType === "group chat" && !hasCustomTopic) {
      const dbGroupName = computeGroupChatNameFromDb(channelId, me);
      if (dbGroupName) refreshedName = dbGroupName;
    }
    if ((refreshedName && refreshedName !== channelName) || teamName) {
      upsertChannel.run(channelId, refreshedName || channelName, channelType, teamName);
    }
    if (isCanonicalChannelId(channelId) && newestDomMid > 0) {
      const prevSyncMs =
        selectChannelLastSync.get(channelId)?.last_sync_ms || 0;
      if (newestDomMid > prevSyncMs + 1000) {
        queueChannelForBackgroundSync(channelId, refreshedName || channelName);
      }
    }
  }
  if (hasOptimisticMessage && !pendingOptimisticRetryTimer) {
    pendingOptimisticRetryTimer = setTimeout(() => {
      pendingOptimisticRetryTimer = null;
      runIdle(() => syncAll());
    }, 500);
  }
}

let lastContextChannelId = null;
let lastContextChannelName = null;
let lastContextMenuTriggerAt = 0;

function resolveActiveChannel() {
  const pane = document.querySelector(PANE_SELECTOR);
  if (!pane) return { id: lastContextChannelId, name: lastContextChannelName };
  const name = getChannelName(pane);
  const id = getChannelId(pane, name);
  return {
    id: isCanonicalChannelId(id) ? id : lastContextChannelId,
    name: name || lastContextChannelName,
  };
}

function injectContextMenuSyncItem() {
  if (
    !lastContextMenuTriggerAt ||
    Date.now() - lastContextMenuTriggerAt > 1000 ||
    !isCanonicalChannelId(lastContextChannelId)
  ) {
    return;
  }

  for (const menu of document.querySelectorAll('[role="menu"]')) {
    if (menu.querySelector("[data-tfl-sync-channel-item]")) continue;
    if (menu.closest?.(PANE_SELECTOR)) continue;
    const items = [...menu.querySelectorAll(':scope > [role="menuitem"]')];
    if (items.length < 3) continue;
    // Skip icon-only toolbars/popovers (e.g., reaction bars or preview popovers)
    if (!items.some((el) => (el.textContent || "").trim().length > 2)) continue;

    const target = {
      id: lastContextChannelId,
      name: lastContextChannelName,
    };

    const template = items[items.length - 1];
    const item = template.cloneNode(false);
    item.removeAttribute("id");
    item.removeAttribute("aria-describedby");
    item.removeAttribute("aria-controls");
    item.removeAttribute("aria-expanded");
    item.removeAttribute("aria-haspopup");
    item.setAttribute("data-tfl-sync-channel-item", "true");
    item.textContent = "Sync channel archive";
    item.style.cursor = "pointer";

    item.addEventListener(
      "click",
      (e) => {
        e.preventDefault();
        e.stopPropagation();
        document.body.dispatchEvent(
          new KeyboardEvent("keydown", { key: "Escape", bubbles: true })
        );
        runFullArchiveSync(false, target.id, target.name);
      },
      true
    );

    menu.appendChild(item);
    lastContextMenuTriggerAt = 0;
  }
}

function syncAll() {
  for (const pane of document.querySelectorAll(PANE_SELECTOR)) {
    try {
      syncPane(pane);
    } catch (err) {
      console.error("[message-logger] syncPane error:", err);
    }
  }
  try {
    scanDomSidebar();
  } catch (err) {
    console.error("[message-logger] scanDomSidebar error:", err);
  }
  try {
    injectContextMenuSyncItem();
  } catch (err) {
    console.error("[message-logger] injectContextMenuSyncItem error:", err);
  }
}

// --- Full Archive Sync (triggered via menu item) ---

let syncInProgress = false;
let syncPaused = false;
let syncCancelled = false;
let resumeWaiter = null;
let lastStatusText = "";
let hideTimer = null;

const ENTITIES = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: " ",
};

const SKYPE_EMOJI_MAP = {
  like: "👍",
  heart: "❤️",
  laugh: "😆",
  surprised: "😮",
  sad: "😢",
  angry: "😠",
  ok: "👌",
  think: "🤔",
  yes: "👍",
};

function emojiFromItemId(itemId) {
  if (!itemId) return "";
  if (SKYPE_EMOJI_MAP[itemId]) return SKYPE_EMOJI_MAP[itemId];
  const hexMatch = itemId.match(/^([0-9a-f]{4,6})_/i);
  if (hexMatch) {
    const cp = parseInt(hexMatch[1], 16);
    if (cp > 0) return String.fromCodePoint(cp);
  }
  return `:${itemId}:`;
}

function decodeEntities(str) {
  return str.replace(/&(#x?[0-9a-f]+|[a-z]+);/gi, (match, ent) => {
    const lower = ent.toLowerCase();
    if (lower in ENTITIES) return ENTITIES[lower];
    if (lower.startsWith("#x")) {
      return String.fromCodePoint(parseInt(lower.slice(2), 16) || 0);
    }
    if (lower.startsWith("#")) {
      return String.fromCodePoint(parseInt(lower.slice(1), 10) || 0);
    }
    return match;
  });
}

function htmlToText(html, codePlaceholders = []) {
  if (!html) return "";
  const stashCode = (text) => {
    const token = `\x00CODE_${codePlaceholders.length}\x00`;
    codePlaceholders.push(text);
    return token;
  };

  const processed = String(html)
    .replace(/\r\n?/g, "\n")
    .replace(/<pre\b([^>]*)>([\s\S]*?)<\/pre>/gi, (_m, attrs, inner) => {
      const langMatch = attrs.match(/\blanguage-([a-z0-9_+-]+)/i);
      const lang = normalizeCodeLang(langMatch?.[1]);
      const code = decodeEntities(
        inner
          .replace(/<br\s*\/?>/gi, "\n")
          .replace(/<\/(p|div|li)>/gi, "\n")
          .replace(/<[^>]+>/g, "")
      )
        .replace(/\u00a0/g, " ")
        .replace(/\r\n?/g, "\n")
        .replace(/\n+$/, "");
      return `\n${stashCode(`\`\`\`${lang}\n${code}\n\`\`\``)}\n`;
    })
    .replace(/<code\b[^>]*>([\s\S]*?)<\/code>/gi, (_m, inner) => {
      const code = decodeEntities(
        inner.replace(/<br\s*\/?>/gi, "\n").replace(/<[^>]+>/g, "")
      )
        .replace(/\u00a0/g, " ")
        .replace(/\r\n?/g, "\n");
      return stashCode(`\`${code}\``);
    })
    .replace(/<blockquote\b[^>]*>([\s\S]*?)<\/blockquote>/gi, (_m, inner) => {
      const authorMatch = inner.match(
        /<strong\b[^>]*itemprop="mri"[^>]*>([\s\S]*?)<\/strong>/i
      );
      const timeMatch = inner.match(
        /<span\b[^>]*itemprop="time"[^>]*>([\s\S]*?)<\/span>/i
      );
      const previewMatch = inner.match(
        /<p\b[^>]*itemprop="preview"[^>]*>([\s\S]*?)<\/p>/i
      );
      if (authorMatch || previewMatch) {
        const author = htmlToText(authorMatch?.[1] || "", codePlaceholders);
        const time = htmlToText(timeMatch?.[1] || "", codePlaceholders);
        const preview = htmlToText(previewMatch?.[1] || "", codePlaceholders);
        return `\n${formatQuoteBlock(author, time, preview)}`;
      }
      const body = htmlToText(inner, codePlaceholders);
      return `\n${formatQuoteBlock("", "", body)}`;
    })
    .replace(/<(?:img|emoji)\b[^>]*>/gi, (tag) => {
      const altMatch = tag.match(/\balt=(?:"([^"]*)"|'([^']*)')/i);
      if (altMatch) return altMatch[1] ?? altMatch[2] ?? "";
      const idMatch = tag.match(/\b(?:itemid|id)=(?:"([^"]*)"|'([^']*)')/i);
      if (idMatch) return emojiFromItemId(idMatch[1] ?? idMatch[2]);
      return "";
    })
    .replace(
      /<readonly\b[^>]*itemtype="http:\/\/schema\.skype\.com\/Mention"[^>]*>([\s\S]*?)<\/readonly>/gi,
      "$1 "
    )
    .replace(
      /<span\b[^>]*itemtype="http:\/\/schema\.skype\.com\/Mention"[^>]*>([\s\S]*?)<\/span>/gi,
      "$1 "
    )
    .replace(/<br\s*\/?>/gi, "\n")
    .replace(/<\/(p|div|li|tr|h[1-6]|blockquote|pre)>/gi, "\n")
    .replace(/<[^>]+>/g, "");

  return decodeEntities(processed)
    .replace(/\u00a0/g, " ")
    .replace(/\x00CODE_(\d+)\x00/g, (_m, idx) => codePlaceholders[Number(idx)] ?? "")
    .replace(/\r\n?/g, "\n")
    .replace(/[ \t]+\n/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

function extractChatSvcReactions(m) {
  let raw = m.properties?.emotions || m.emotions;
  if (!raw) return [];
  if (typeof raw === "string") {
    try {
      raw = JSON.parse(raw);
    } catch {
      return [];
    }
  }
  if (!Array.isArray(raw)) return [];
  const reactions = [];
  for (const entry of raw) {
    const key = entry?.key;
    const count = Array.isArray(entry?.users)
      ? entry.users.length
      : Number(entry?.count) || 0;
    if (key && count > 0) {
      reactions.push({ key, emoji: emojiFromItemId(key), count });
    }
  }
  return reactions;
}

function extractChatSvcMentions(m) {
  let raw = m.properties?.mentions || m.mentions;
  if (!raw) return [];
  if (typeof raw === "string") {
    try {
      raw = JSON.parse(raw);
    } catch {
      return [];
    }
  }
  if (!Array.isArray(raw)) return [];
  const tokens = [];
  for (const entry of raw) {
    const id = normalizeAuthorId(entry?.mri || entry?.id || entry?.userId);
    if (!id) continue;
    const name = entry?.displayName?.trim() || entry?.name?.trim() || null;
    tokens.push({ id, name });
  }
  return mergeMentionTokens(tokens);
}

function togglePauseSync() {
  if (!syncInProgress) return;
  syncPaused = !syncPaused;
  if (!syncPaused && resumeWaiter) {
    const r = resumeWaiter;
    resumeWaiter = null;
    r();
  }
  showSyncStatus(lastStatusText);
}

function cancelSync() {
  if (!syncInProgress) return;
  syncCancelled = true;
  syncPaused = false;
  if (resumeWaiter) {
    const r = resumeWaiter;
    resumeWaiter = null;
    r();
  }
}

async function waitIfPausedOrCancelled() {
  while (syncPaused && !syncCancelled) {
    showSyncStatus(lastStatusText);
    await new Promise((resolve) => {
      resumeWaiter = resolve;
    });
  }
  if (syncCancelled) {
    const err = new Error("Cancelled");
    err.cancelled = true;
    throw err;
  }
}

function showSyncStatus(text, autohideMs = 0) {
  lastStatusText = text;
  if (hideTimer) {
    clearTimeout(hideTimer);
    hideTimer = null;
  }
  let badge = document.getElementById("tfl-archive-sync-status");
  let label;
  let pauseBtn;
  let controls;
  if (!badge) {
    badge = document.createElement("div");
    badge.id = "tfl-archive-sync-status";
    badge.style.cssText =
      "position: fixed; bottom: 16px; right: 72px; z-index: 2147483647; " +
      "background: #6264a7; color: #fff; padding: 8px 14px; border-radius: 6px; " +
      "font: 600 13px/1.4 sans-serif; box-shadow: 0 4px 12px rgba(0,0,0,0.35); " +
      "display: flex; align-items: center; gap: 10px;";

    label = document.createElement("span");
    label.id = "tfl-archive-sync-label";
    badge.appendChild(label);

    controls = document.createElement("span");
    controls.id = "tfl-archive-sync-controls";
    controls.style.cssText = "display: inline-flex; gap: 6px;";

    const btnStyle =
      "background: rgba(255,255,255,0.2); color: #fff; border: 1px solid rgba(255,255,255,0.4); " +
      "border-radius: 4px; padding: 2px 8px; font: 600 12px/1.2 sans-serif; cursor: pointer;";

    pauseBtn = document.createElement("button");
    pauseBtn.id = "tfl-archive-sync-pause";
    pauseBtn.type = "button";
    pauseBtn.style.cssText = btnStyle;
    pauseBtn.addEventListener("click", () => togglePauseSync());

    const cancelBtn = document.createElement("button");
    cancelBtn.type = "button";
    cancelBtn.textContent = "Cancel";
    cancelBtn.style.cssText = btnStyle;
    cancelBtn.addEventListener("click", () => cancelSync());

    controls.appendChild(pauseBtn);
    controls.appendChild(cancelBtn);
    badge.appendChild(controls);
    document.body.appendChild(badge);
  } else {
    label = document.getElementById("tfl-archive-sync-label");
    pauseBtn = document.getElementById("tfl-archive-sync-pause");
    controls = document.getElementById("tfl-archive-sync-controls");
  }

  if (label) {
    label.textContent = syncPaused ? `[Paused] ${text}` : text;
  }
  if (pauseBtn) {
    pauseBtn.textContent = syncPaused ? "Resume" : "Pause";
  }
  if (controls) {
    controls.style.display = autohideMs > 0 ? "none" : "inline-flex";
  }
  if (autohideMs > 0) {
    hideTimer = setTimeout(() => {
      badge.remove();
      hideTimer = null;
    }, autohideMs);
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const nativeFetch = globalThis.fetch?.bind(globalThis) || fetch;

async function buildChatSvcHeaders(coreServices, url) {
  if (coreServices?.authenticationResources?.skypeAuthorizeAuthResource) {
    try {
      const res =
        await coreServices.authenticationResources.skypeAuthorizeAuthResource({
          url,
          method: "GET",
          headers: {},
          authOptions: { source: "mfeService" },
        });
      if (res?.request?.headers) return res.request.headers;
    } catch {
      // Fall back to direct token acquisition
    }
  }
  const handler = globalThis.window?.teamsForLinuxReactHandler;
  const tokenRes = await handler?.acquireToken?.("https://ic3.teams.office.com");
  if (!tokenRes?.token) {
    throw new Error("Could not acquire IC3 token");
  }
  return {
    Authorization: `Bearer ${tokenRes.token}`,
    "x-ms-migration": "True",
  };
}

async function chatSvcFetchJson(coreServices, url) {
  for (let attempt = 0; attempt < 4; attempt++) {
    const headers = await buildChatSvcHeaders(coreServices, url);
    const res = await nativeFetch(url, { headers });
    if (res.status === 429) {
      const retryAfter = Number(res.headers.get("retry-after")) || 5;
      await sleep(retryAfter * 1000);
      continue;
    }
    if (!res.ok) {
      throw new Error(`HTTP ${res.status} for ${url}`);
    }
    return await res.json();
  }
  throw new Error(`Rate limit exceeded for ${url}`);
}

async function resolveUnknownMris(coreServices, mris) {
  const toResolve = [];
  for (const raw of mris) {
    const id = normalizeAuthorId(raw);
    if (!id || !id.startsWith("8:orgid:")) continue;
    const existing = selectAuthorName.get(id)?.name;
    if (!existing || existing.startsWith("8:") || !existing.includes(" ")) {
      if (!toResolve.includes(id)) toResolve.push(id);
    }
  }
  if (toResolve.length === 0) return;

  try {
    const mtBase = (
      (await coreServices.discover?.regionGtm?.("middleTier")) ||
      "https://teams.cloud.microsoft/api/mt/part/emea-03"
    ).replace(/\/+$/, "");
    const url = `${mtBase}/beta/users/fetchShortProfile?isMailAddress=false&enableGuest=true&skypeTeamsInfo=true&canBeSmtpAddress=false&includeIBBarredUsers=true&includeDisabledAccounts=true`;

    for (let offset = 0; offset < toResolve.length; offset += 200) {
      const chunk = toResolve.slice(offset, offset + 200);
      let headers = {
        "Content-Type": "application/json;charset=UTF-8",
        "x-ms-client-caller": "standAlone",
      };
      if (coreServices?.authenticationResources?.mtAuthWithSkypeXTokenResource) {
        try {
          const authRes =
            await coreServices.authenticationResources.mtAuthWithSkypeXTokenResource({
              url,
              method: "POST",
              headers,
              authOptions: { source: "peoplePhase2" },
            });
          if (authRes?.request?.headers) {
            headers = { ...headers, ...authRes.request.headers };
          }
        } catch {
          // ignore auth helper error
        }
      }
      if (!headers.Authorization && !headers.authorization) {
        const handler = globalThis.window?.teamsForLinuxReactHandler;
        const tokenRes = await handler?.acquireToken?.(
          "https://api.spaces.skype.com"
        );
        if (tokenRes?.token) {
          headers.Authorization = `Bearer ${tokenRes.token}`;
        }
      }
      const res = await nativeFetch(url, {
        method: "POST",
        headers,
        body: JSON.stringify(chunk),
      });
      if (!res.ok) continue;
      const data = await res.json();
      for (const u of data?.value || []) {
        const id = normalizeAuthorId(u?.mri || u?.id || u?.objectId);
        const name = u?.displayName?.trim();
        if (id && name && isPersonMri(id)) {
          upsertAuthor.run(id, name);
        }
      }
    }
  } catch {
    // Profile resolution is best-effort
  }
}

function harvestAuthorsFromMessages(convMessages, convMembers = []) {
  const seenMris = new Set();
  const trackMri = (raw) => {
    const id = normalizeAuthorId(raw);
    if (isPersonMri(id)) seenMris.add(id);
    return id;
  };

  if (Array.isArray(convMembers)) {
    for (const mem of convMembers) {
      const id = trackMri(mem?.id || mem?.mri || mem?.userId);
      const disp =
        mem?.displayName?.trim() || mem?.friendlyname?.trim() || null;
      if (id && disp) upsertAuthor.run(id, disp);
    }
  }

  let latestTopicUpdate = null;

  for (const m of convMessages) {
    const fromId = trackMri(m.from);
    const disp = m.imdisplayname?.trim() || null;
    if (fromId && disp) {
      upsertAuthor.run(fromId, disp);
    }
    for (const men of extractChatSvcMentions(m)) {
      if (isPersonMri(men.id)) {
        seenMris.add(men.id);
        if (men.name) upsertAuthor.run(men.id, men.name);
      }
    }

    const text = String(m.content || "").trim();
    if (!text) continue;

    if (text.startsWith("<")) {
      if (/<partlist\b/i.test(text)) {
        for (const pm of text.matchAll(/<part\b[^>]*>([\s\S]*?)<\/part>/gi)) {
          const inner = pm[1];
          const pId = trackMri(inner.match(/<name>([^<]+)<\/name>/i)?.[1]);
          const pDisp = inner.match(/<displayName>([^<]+)<\/displayName>/i)?.[1]?.trim();
          if (pId && pDisp) upsertAuthor.run(pId, pDisp);
        }
      }
      if (/<(?:addmember|deletemember|roleupdate)\b/i.test(text)) {
        trackMri(text.match(/<initiator>([^<]+)<\/initiator>/i)?.[1]);
        for (const tm of text.matchAll(/<target>([^<]+)<\/target>/gi)) {
          trackMri(tm[1]);
        }
      }
      if (/<topicupdate\b/i.test(text)) {
        const val = htmlToText(text.match(/<value>([\s\S]*?)<\/value>/i)?.[1] || "");
        latestTopicUpdate = normalizeChannelName(val);
      }
    } else if (text.startsWith("{")) {
      try {
        const obj = JSON.parse(text);
        if (obj.initiator) trackMri(obj.initiator);
        if (Array.isArray(obj.members)) {
          for (const mem of obj.members) {
            const mId = trackMri(mem?.id);
            const fName = mem?.friendlyname?.trim() || null;
            if (mId && fName) upsertAuthor.run(mId, fName);
          }
        }
        if (typeof obj.topic === "string") {
          latestTopicUpdate = normalizeChannelName(obj.topic);
        }
      } catch {
        // ignore invalid json
      }
    } else {
      PARTLIST_ENTRY_RE.lastIndex = 0;
      for (const pm of text.matchAll(PARTLIST_ENTRY_RE)) {
        const pId = trackMri(pm[1]);
        const pDisp = pm[2]?.trim() || null;
        if (pId && pDisp) upsertAuthor.run(pId, pDisp);
      }
      for (const im of text.matchAll(/8:orgid:[0-9a-f-]{36}/gi)) {
        trackMri(im[0]);
      }
    }
  }

  return { seenMris: [...seenMris], latestTopicUpdate };
}

function computeGroupChatNameFromMessages(convMessages, convMembers = [], me = null) {
  const activeIds = new Set();
  const allIds = new Set();
  const removedIds = new Set();
  const fallbackNames = [];

  const addParticipantId = (rawId, forceAdd = false) => {
    const id = normalizeAuthorId(rawId);
    if (!isPersonMri(id)) return;
    if (forceAdd) removedIds.delete(id);
    allIds.add(id);
    if (!removedIds.has(id)) activeIds.add(id);
  };

  const removeParticipantId = (rawId) => {
    const id = normalizeAuthorId(rawId);
    if (!isPersonMri(id)) return;
    activeIds.delete(id);
    removedIds.add(id);
  };

  for (const m of convMessages) {
    const msgType = m.messagetype || "";
    const text = String(m.content || "").trim();
    const fromId = normalizeAuthorId(m.from);
    const disp = m.imdisplayname?.trim() || null;

    if (fromId && isPersonMri(fromId)) {
      addParticipantId(fromId, false);
    } else if (disp && (msgType.startsWith("RichText") || msgType === "Text")) {
      fallbackNames.push(disp);
    }

    if (!text) continue;

    if (/<(?:addmember|deletemember|roleupdate)\b/i.test(text)) {
      const isDelete =
        /<deletemember\b/i.test(text) ||
        msgType === "ThreadActivity/DeleteMember";
      const initiatorId = normalizeAuthorId(
        text.match(/<initiator>([^<]+)<\/initiator>/i)?.[1]
      );
      const targets = [...text.matchAll(/<target>([^<]+)<\/target>/gi)]
        .map((tm) => normalizeAuthorId(tm[1]))
        .filter(Boolean);
      if (isDelete) {
        for (const t of targets) removeParticipantId(t);
        if (initiatorId && !targets.includes(initiatorId)) {
          addParticipantId(initiatorId, false);
        }
      } else {
        if (initiatorId) addParticipantId(initiatorId, true);
        for (const t of targets) addParticipantId(t, true);
      }
    } else if (/<partlist\b/i.test(text)) {
      for (const pm of text.matchAll(/<part\b[^>]*>([\s\S]*?)<\/part>/gi)) {
        const inner = pm[1];
        const pId = normalizeAuthorId(inner.match(/<name>([^<]+)<\/name>/i)?.[1]);
        const pDisp = inner.match(/<displayName>([^<]+)<\/displayName>/i)?.[1]?.trim();
        if (pId && isPersonMri(pId)) {
          addParticipantId(pId, false);
        } else if (pDisp) {
          fallbackNames.push(pDisp);
        }
      }
    } else if (text.startsWith("{")) {
      try {
        const obj = JSON.parse(text);
        if (Array.isArray(obj.members)) {
          if (obj.initiator) addParticipantId(obj.initiator, true);
          for (const mem of obj.members) {
            if (mem?.id) addParticipantId(mem.id, true);
            else if (mem?.friendlyname) fallbackNames.push(mem.friendlyname);
          }
        }
      } catch {
        // ignore invalid json
      }
    } else if (/\d{13}/.test(text) && !text.includes("callEnded")) {
      const withoutTimestamps = text.replace(/\d{13}/g, " ");
      const idMatches = [
        ...withoutTimestamps.matchAll(
          /(8:orgid:[0-9a-f-]{36}|8:teamsvisitor:[0-9a-f]{32})/gi
        ),
      ]
        .map((x) => normalizeAuthorId(x[1]))
        .filter(Boolean);
      if (
        msgType === "ThreadActivity/DeleteMember" ||
        (idMatches.length === 2 && idMatches[0] === idMatches[1])
      ) {
        removeParticipantId(idMatches[idMatches.length - 1]);
      } else {
        for (const id of idMatches) addParticipantId(id, true);
      }
    }
  }

  if (Array.isArray(convMembers)) {
    for (const mem of convMembers) {
      const mid = normalizeAuthorId(mem?.id || mem?.mri || mem?.userId);
      if (mid) addParticipantId(mid, false);
    }
  }

  if (me?.id) {
    activeIds.delete(me.id);
    allIds.delete(me.id);
  }

  const chosenIds = activeIds.size > 0 ? activeIds : allIds;
  const rawNames = [...chosenIds]
    .map((id) => resolvePersonName(id))
    .concat(fallbackNames);
  const names = dedupeParticipantNames(rawNames, me);
  return names.length > 0 ? names.join(", ") : null;
}

function getConvLastMessageMs(conv) {
  const lastMsg = conv?.lastMessage;
  if (!lastMsg) return 0;
  return Math.max(
    Number(lastMsg.id || lastMsg.sequenceId) || 0,
    Number(lastMsg.version) || 0,
    Number(lastMsg.properties?.edittime) || 0,
    lastMsg.originalarrivaltime ? Date.parse(lastMsg.originalarrivaltime) || 0 : 0,
    lastMsg.composetime ? Date.parse(lastMsg.composetime) || 0 : 0
  );
}

function getConvHorizonMs(conv) {
  const raw = conv?.properties?.consumptionhorizon;
  if (!raw || typeof raw !== "string") return 0;
  const first = Number(raw.split(";")[0]);
  return Number.isFinite(first) && first > 0 ? first : 0;
}

function isConvUnread(conv, domUnreadIds = null) {
  if (!conv?.id) return false;
  if (domUnreadIds?.has(conv.id)) return true;
  const isReadProp = conv.properties?.isRead ?? conv.properties?.isread;
  if (isReadProp === false || isReadProp === "false") return true;
  const lastMs = getConvLastMessageMs(conv);
  const horizonMs = getConvHorizonMs(conv);
  return lastMs > 0 && horizonMs > 0 && lastMs > horizonMs;
}

async function syncSingleConversation(
  coreServices,
  baseUrl,
  conv,
  me,
  {
    resyncOnly = false,
    stopAtMs = null,
    maxPages = Infinity,
    pageSize = 200,
    teamNameById = new Map(),
    teamNameByGroupId = new Map(),
    targetChannelName = null,
    checkPause = false,
    onBatch = null,
  } = {}
) {
  const channelId = conv.id;
  if (!isCanonicalChannelId(channelId)) return 0;

  const channelType = classifyChannelType(
    channelId,
    false,
    false,
    conv.threadProperties
  );
  const explicitTopic = normalizeChannelName(
    conv.threadProperties?.topic ||
      conv.threadProperties?.spaceThreadTopic ||
      null
  );
  let channelName = explicitTopic;
  let teamName = null;
  if (channelType === "channel") {
    const info = getConvTeamInfo(conv);
    teamName =
      info.teamName ||
      (info.teamId ? teamNameById.get(info.teamId) : null) ||
      (info.groupId ? teamNameByGroupId.get(info.groupId) : null) ||
      null;
    if (!teamName && info.teamId) {
      const existingTeam = selectChannelInfo.get(info.teamId);
      teamName =
        existingTeam?.team_name ||
        (info.teamId !== channelId ? existingTeam?.name : null) ||
        null;
    }
    if (!teamName) {
      teamName = selectChannelInfo.get(channelId)?.team_name || null;
    }
    if (!teamName && info.teamId && info.teamId !== channelId) {
      try {
        const teamConv = await chatSvcFetchJson(
          coreServices,
          `${baseUrl}/v1/users/ME/conversations/${encodeURIComponent(
            info.teamId
          )}?view=msnp24Equivalent`
        );
        const teamInfo = getConvTeamInfo(teamConv);
        if (teamInfo.teamName) {
          teamName = teamInfo.teamName;
          teamNameById.set(info.teamId, teamName);
          if (teamInfo.groupId) {
            teamNameByGroupId.set(teamInfo.groupId, teamName);
          }
        }
      } catch {
        // Team metadata lookup is best-effort
      }
    }
  }

  const prevSyncMs = selectChannelLastSync.get(channelId)?.last_sync_ms || null;
  const effectiveStopMs =
    stopAtMs !== null ? stopAtMs : resyncOnly && prevSyncMs ? prevSyncMs : null;

  if (resyncOnly && prevSyncMs) {
    const convLastMs = getConvLastMessageMs(conv);
    if (
      (convLastMs === 0 || convLastMs <= prevSyncMs + 1000) &&
      (!effectiveStopMs || effectiveStopMs >= prevSyncMs)
    ) {
      upsertChannel.run(channelId, explicitTopic, channelType, teamName);
      return 0;
    }
  }

  let msgUrl = `${baseUrl}/v1/users/ME/conversations/${encodeURIComponent(
    channelId
  )}/messages?view=msnp24Equivalent|supportsMessageProperties&pageSize=${pageSize}&startTime=1`;

  const convMessages = [];
  let reachedSyncTarget = false;
  let pagesFetched = 0;
  while (msgUrl && pagesFetched < maxPages) {
    if (checkPause) await waitIfPausedOrCancelled();
    let page;
    try {
      page = await chatSvcFetchJson(coreServices, msgUrl);
    } catch (err) {
      console.warn(`[message-logger] Skipping page in ${channelId}:`, err.message);
      break;
    }
    pagesFetched++;
    const batch = page.messages || [];
    if (batch.length === 0) {
      reachedSyncTarget = true;
      break;
    }
    convMessages.push(...batch);
    if (onBatch) onBatch(convMessages.length);

    if (effectiveStopMs) {
      const oldestInBatch = batch.reduce((min, m) => {
        const t =
          Number(m.id || m.sequenceId) ||
          (m.originalarrivaltime
            ? Date.parse(m.originalarrivaltime)
            : Infinity);
        return t > 0 && t < min ? t : min;
      }, Infinity);
      if (oldestInBatch <= effectiveStopMs + 1000) {
        reachedSyncTarget = true;
        break;
      }
    }

    msgUrl = page._metadata?.backwardLink || null;
    if (!msgUrl) {
      reachedSyncTarget = true;
      break;
    }
    await sleep(150);
  }

  // Process oldest-first so membership events and sender inheritance work in chronological order
  convMessages.sort(
    (a, b) =>
      (Number(a.id || a.sequenceId) || 0) -
      (Number(b.id || b.sequenceId) || 0)
  );

  const { seenMris, latestTopicUpdate } = harvestAuthorsFromMessages(
    convMessages,
    conv.members
  );
  if (!channelName && latestTopicUpdate) {
    channelName = latestTopicUpdate;
  }

  const peerId =
    channelType === "direct chat"
      ? getPeerIdFromDirectChat(channelId, me.id)
      : null;

  if (!channelName && channelType === "direct chat") {
    if (peerId) {
      await resolveUnknownMris(coreServices, [peerId]);
    }
    const peerMsg = convMessages.find((m) => {
      const fromId = normalizeAuthorId(m.from);
      if (!m.imdisplayname?.trim() || !fromId) return false;
      if (peerId) return fromId === peerId;
      return Boolean(me.id && fromId !== me.id);
    });
    channelName = normalizeChannelName(
      peerMsg?.imdisplayname?.trim() ||
        (peerId ? selectAuthorName.get(peerId)?.name : null) ||
        null
    );
  }

  if (!channelName && channelType === "group chat") {
    await resolveUnknownMris(coreServices, seenMris);
    channelName =
      computeGroupChatNameFromMessages(convMessages, conv.members, me) ||
      computeGroupChatNameFromDb(channelId, me);
  }

  if (!channelName && targetChannelName) {
    channelName = normalizeChannelName(targetChannelName);
  }

  upsertChannel.run(channelId, channelName, channelType, teamName);

  let lastAuthor = null;
  let newestMsgMs = 0;
  let savedCount = 0;

  db.exec("BEGIN");
  try {
    for (const m of convMessages) {
      const rawId =
        m.id ||
        m.sequenceId ||
        (m.originalarrivaltime
          ? String(Date.parse(m.originalarrivaltime))
          : null);
      if (!rawId || !/^\d{13}$/.test(String(rawId))) continue;
      const id = Number(rawId);
      const mVersion = Math.max(
        id,
        Number(m.version) || 0,
        Number(m.properties?.edittime) || 0
      );
      if (mVersion > newestMsgMs) newestMsgMs = mVersion;

      const linkMatch = m.conversationLink?.match(/;messageid=(\d{13})/);
      const rawThreadId =
        m.rootMessageId ||
        m.properties?.parentMessageId ||
        linkMatch?.[1];
      const threadId =
        rawThreadId && /^\d{13}$/.test(String(rawThreadId))
          ? Number(rawThreadId)
          : null;

      const subject = String(
        m.properties?.title || m.properties?.subject || m.subject || ""
      )
        .replace(/\r\n?/g, "\n")
        .trim();
      const msgType = m.messagetype || "";
      const isRichOrText =
        msgType.startsWith("RichText") || msgType === "Text";
      const isUserMsg = isRichOrText || Boolean(subject);
      const fromId = normalizeAuthorId(m.from);

      let direction = "received";
      if (!isUserMsg) {
        direction = "event";
      } else if (me.id && fromId === me.id) {
        direction = "sent";
      }

      let authorId = null;
      let authorName = null;
      if (direction !== "event") {
        authorName =
          m.imdisplayname?.trim() ||
          (direction === "sent" ? me.name : null) ||
          lastAuthor?.name ||
          (direction === "received" && channelType === "direct chat"
            ? channelName
            : null);
        authorId =
          fromId ||
          (direction === "sent" ? me.id : null) ||
          lastAuthor?.id ||
          (direction === "received" && channelType === "direct chat"
            ? peerId
            : null) ||
          authorName;
        if (authorId || authorName) {
          lastAuthor = { id: authorId, name: authorName };
        }
      }

      const body = !isRichOrText
        ? formatEventContent(m.content, msgType)
        : htmlToText(m.content);
      const content =
        subject && body && body !== subject
          ? `${subject}\n${body}`
          : subject || body;
      const reactions = extractChatSvcReactions(m);
      const mentions = extractChatSvcMentions(m);
      storeRecord({
        id,
        threadId,
        channelId,
        channelName,
        teamName,
        channelType,
        direction,
        authorId,
        authorName,
        content,
        reactions,
        mentions,
      });
      savedCount++;
    }
    if (
      reachedSyncTarget &&
      (!maxPages ||
        !Number.isFinite(maxPages) ||
        prevSyncMs ||
        pagesFetched < maxPages)
    ) {
      const syncTs =
        Math.max(newestMsgMs, getConvLastMessageMs(conv), prevSyncMs || 0) ||
        Date.now();
      updateChannelLastSync.run(syncTs, channelId, syncTs);
    }
    db.exec("COMMIT");
  } catch (err) {
    db.exec("ROLLBACK");
    throw err;
  }

  return savedCount;
}

async function runFullArchiveSync(
  resyncOnly = false,
  targetChannelId = null,
  targetChannelName = null
) {
  if (syncInProgress) {
    if (syncPaused) togglePauseSync();
    return;
  }
  const handler = globalThis.window?.teamsForLinuxReactHandler;
  const coreServices = handler?._getTeams2CoreServices?.();
  if (!coreServices) return;

  syncInProgress = true;
  syncPaused = false;
  syncCancelled = false;
  let totalMessages = 0;
  const modeLabel = targetChannelId
    ? `Channel sync${targetChannelName ? ` (${targetChannelName})` : ""}`
    : resyncOnly
      ? "Re-sync"
      : "Archive sync";
  const progressInterval = setInterval(() => {
    if (syncInProgress && lastStatusText) {
      showSyncStatus(lastStatusText);
    }
  }, 5000);
  try {
    showSyncStatus(`${modeLabel}: discovering conversations...`);

    const baseUrl = (
      (await coreServices.discover?.regionGtm?.("chatService")) ||
      "https://teams.cloud.microsoft/api/chatsvc/emea"
    ).replace(/\/+$/, "");

    const conversations = [];
    if (targetChannelId) {
      try {
        const single = await chatSvcFetchJson(
          coreServices,
          `${baseUrl}/v1/users/ME/conversations/${encodeURIComponent(
            targetChannelId
          )}?view=msnp24Equivalent`
        );
        conversations.push(
          single?.id
            ? single
            : {
                id: targetChannelId,
                threadProperties: { topic: targetChannelName },
              }
        );
      } catch {
        conversations.push({
          id: targetChannelId,
          threadProperties: { topic: targetChannelName },
        });
      }
    } else {
      let convUrl = `${baseUrl}/v1/users/ME/conversations?view=msnp24Equivalent&pageSize=200&startTime=1`;
      while (convUrl) {
        await waitIfPausedOrCancelled();
        const data = await chatSvcFetchJson(coreServices, convUrl);
        for (const conv of data.conversations || []) {
          if (conv.id?.startsWith("19:") || conv.id === "48:notes") {
            conversations.push(conv);
          }
        }
        convUrl = data._metadata?.backwardLink || null;
        if (convUrl) await sleep(150);
      }
    }

    const me = getCurrentUser(conversations.map((c) => c.id));

    const teamNameById = new Map();
    const teamNameByGroupId = new Map();
    for (const conv of conversations) {
      const info = getConvTeamInfo(conv);
      if (info.teamName && conv.id) {
        teamNameById.set(conv.id, info.teamName);
        if (info.groupId) {
          teamNameByGroupId.set(info.groupId, info.teamName);
        }
      }
    }

    for (let i = 0; i < conversations.length; i++) {
      await waitIfPausedOrCancelled();
      const conv = conversations[i];
      lastStatusText = `${modeLabel}: ${i + 1}/${conversations.length} conversations (${totalMessages} msgs)...`;
      const saved = await syncSingleConversation(
        coreServices,
        baseUrl,
        conv,
        me,
        {
          resyncOnly,
          teamNameById,
          teamNameByGroupId,
          targetChannelName,
          checkPause: true,
          onBatch: (inConv) => {
            lastStatusText = `${modeLabel}: ${i + 1}/${conversations.length} conversations (${totalMessages + inConv} msgs)...`;
          },
        }
      );
      totalMessages += saved;
    }

    showSyncStatus(
      `${modeLabel} complete: ${totalMessages} messages across ${conversations.length} conversations`,
      8000
    );
  } catch (err) {
    if (err?.cancelled) {
      showSyncStatus(
        `${modeLabel} cancelled (${totalMessages} messages saved)`,
        5000
      );
    } else {
      console.error("[message-logger] Archive sync failed:", err);
      showSyncStatus(`${modeLabel} failed: ${err.message}`, 8000);
    }
  } finally {
    clearInterval(progressInterval);
    syncInProgress = false;
    syncPaused = false;
    syncCancelled = false;
    resumeWaiter = null;
    if (
      pendingBackgroundChannels.size > 0 ||
      backgroundSweepNeeded ||
      !backgroundStartupSweepDone
    ) {
      scheduleBackgroundWorker(1000);
    }
  }
}

// --- Background Sync (startup unread/delta & real-time message arrival) ---

const pendingBackgroundChannels = new Map();
const lastBackgroundSyncByChannel = new Map();
const sidebarStateByChannel = new Map();
let backgroundWorkerTimer = null;
let backgroundSyncRunning = false;
let backgroundStartupSweepDone = false;
let backgroundSweepNeeded = false;
let commandStreamSubscribed = false;
let lastObservedTitleUnreadCount = 0;

function extractDomSidebarItemState(el) {
  if (!el) return null;
  const channelId = extractConvIdFromFiber(el);
  if (!isCanonicalChannelId(channelId)) return null;

  let isUnread = Boolean(
    el.querySelector?.(
      '[data-tid*="unread"], [class*="unread"], [aria-label*="unread" i], [aria-label*="ongelezen" i]'
    )
  );
  let lastMsgId = 0;
  let itemName = null;

  const key = Object.keys(el).find((k) => k.startsWith("__reactFiber$"));
  let fiber = key ? el[key] : null;
  for (let i = 0; i < 25 && fiber; i++, fiber = fiber.return) {
    const p = fiber.memoizedProps;
    if (!p) continue;
    const obj = p.chat || p.channel || p.item || p;
    if (
      p.isRead === false ||
      p.isUnread === true ||
      p.unread === true ||
      (typeof p.unreadCount === "number" && p.unreadCount > 0) ||
      obj?.isRead === false ||
      obj?.isUnread === true ||
      obj?.unread === true ||
      (typeof obj?.unreadCount === "number" && obj.unreadCount > 0)
    ) {
      isUnread = true;
    }
    const candidateLastId = Number(
      obj?.lastMessage?.id ||
        obj?.lastMessage?.sequenceId ||
        p.lastMessage?.id ||
        obj?.lastMessageTime ||
        0
    );
    if (candidateLastId > lastMsgId) {
      lastMsgId = candidateLastId;
    }
    if (!itemName) {
      const candidateName =
        obj?.title ||
        obj?.displayName ||
        obj?.name ||
        obj?.topic ||
        p.title ||
        p.displayName;
      if (typeof candidateName === "string" && candidateName.trim()) {
        itemName = normalizeChannelName(candidateName);
      }
    }
  }

  if (!itemName) {
    const labelEl = el.querySelector?.(
      '[data-tid="chat-list-item-title"], [data-tid="channel-list-item-text"], ' +
        TITLE_SELECTOR
    );
    itemName = normalizeChannelName(labelEl?.textContent);
  }

  const previewText = (el.textContent || "")
    .replace(/\s+/g, " ")
    .trim()
    .slice(0, 200);
  const signature = `${isUnread ? 1 : 0}|${lastMsgId || ""}|${previewText}`;

  return { channelId, isUnread, lastMsgId, itemName, signature };
}

function collectDomUnreadChannels() {
  const unreadMap = new Map();
  const items = document.querySelectorAll(
    '[role="treeitem"], [data-tid*="list-item"]'
  );
  for (const el of items) {
    const state = extractDomSidebarItemState(el);
    if (state?.isUnread) {
      unreadMap.set(state.channelId, state.itemName || null);
    }
  }
  return unreadMap;
}

let lastSidebarScanMs = 0;

function scanDomSidebar() {
  const now = Date.now();
  if (now - lastSidebarScanMs < 3000) return;
  lastSidebarScanMs = now;

  const items = document.querySelectorAll(
    '[role="treeitem"], [data-tid*="list-item"]'
  );
  for (const el of items) {
    const state = extractDomSidebarItemState(el);
    if (!state) continue;
    const { channelId, isUnread, lastMsgId, itemName, signature } = state;
    const prevSig = sidebarStateByChannel.get(channelId);
    sidebarStateByChannel.set(channelId, signature);

    if (prevSig === undefined) {
      if (isUnread) {
        queueChannelForBackgroundSync(channelId, itemName);
      } else if (lastMsgId > 0) {
        const prevSyncMs =
          selectChannelLastSync.get(channelId)?.last_sync_ms || 0;
        if (lastMsgId > prevSyncMs + 1000) {
          queueChannelForBackgroundSync(channelId, itemName);
        }
      }
    } else if (prevSig !== signature) {
      const prevParts = prevSig.split("|");
      const prevPreview = prevParts.slice(2).join("|");
      const currPreview = signature.split("|").slice(2).join("|");
      if (isUnread || currPreview !== prevPreview) {
        queueChannelForBackgroundSync(channelId, itemName);
      }
    }
  }

  const titleMatch = /^\((\d+)\)/.exec(document.title || "");
  const titleUnread = titleMatch ? Number.parseInt(titleMatch[1], 10) : 0;
  if (titleUnread > lastObservedTitleUnreadCount) {
    scheduleIncrementalSweep(2000);
  }
  lastObservedTitleUnreadCount = titleUnread;
}

function scheduleBackgroundWorker(delayMs = 1500) {
  if (backgroundWorkerTimer) return;
  backgroundWorkerTimer = setTimeout(() => {
    backgroundWorkerTimer = null;
    void processBackgroundSync();
  }, delayMs);
}

function queueChannelForBackgroundSync(
  channelId,
  hintName = null,
  delayMs = 1500
) {
  if (!isCanonicalChannelId(channelId)) return;
  const existing = pendingBackgroundChannels.get(channelId);
  pendingBackgroundChannels.set(channelId, {
    hintName: hintName || existing?.hintName || null,
  });
  const lastRun = lastBackgroundSyncByChannel.get(channelId) || 0;
  const cooldownRemaining = Math.max(0, 3000 - (Date.now() - lastRun));
  scheduleBackgroundWorker(Math.max(delayMs, cooldownRemaining));
}

function scheduleIncrementalSweep(delayMs = 2000) {
  backgroundSweepNeeded = true;
  scheduleBackgroundWorker(delayMs);
}

async function syncUnreadAndUpdatedConversations(
  coreServices,
  baseUrl,
  { fullScan = false } = {}
) {
  const domUnreadMap = collectDomUnreadChannels();
  const conversations = [];
  let convUrl = `${baseUrl}/v1/users/ME/conversations?view=msnp24Equivalent&pageSize=200&startTime=1`;
  let pages = 0;
  const maxPages = fullScan ? Infinity : 1;

  while (convUrl && pages < maxPages) {
    if (syncInProgress) return;
    const data = await chatSvcFetchJson(coreServices, convUrl);
    pages++;
    for (const conv of data.conversations || []) {
      if (isCanonicalChannelId(conv.id)) {
        conversations.push(conv);
      }
    }
    convUrl = data._metadata?.backwardLink || null;
    if (convUrl && pages < maxPages) await sleep(150);
  }

  const me = getCurrentUser(conversations.map((c) => c.id));
  const teamNameById = new Map();
  const teamNameByGroupId = new Map();
  const seenConvIds = new Set();

  for (const conv of conversations) {
    seenConvIds.add(conv.id);
    const info = getConvTeamInfo(conv);
    if (info.teamName && conv.id) {
      teamNameById.set(conv.id, info.teamName);
      if (info.groupId) {
        teamNameByGroupId.set(info.groupId, info.teamName);
      }
    }
  }

  for (const [unreadId, unreadName] of domUnreadMap) {
    if (!seenConvIds.has(unreadId)) {
      pendingBackgroundChannels.set(unreadId, { hintName: unreadName });
    }
  }

  for (const conv of conversations) {
    if (syncInProgress) return;
    const channelId = conv.id;
    const convLastMs = getConvLastMessageMs(conv);
    const horizonMs = getConvHorizonMs(conv);
    const unread = isConvUnread(conv, domUnreadMap);
    const prevSyncMs = selectChannelLastSync.get(channelId)?.last_sync_ms || 0;
    const maxMsgId = selectChannelMaxMsgId.get(channelId)?.max_id || 0;

    // Skip root team space containers with no messages
    if (conv.threadProperties?.threadType === "space" && convLastMs === 0) {
      continue;
    }

    const hasNewSinceSync = prevSyncMs > 0 && convLastMs > prevSyncMs + 1000;
    const hasUnloggedUnread =
      unread &&
      (prevSyncMs === 0 ||
        convLastMs === 0 ||
        convLastMs > prevSyncMs + 1000 ||
        (convLastMs > 0 && maxMsgId < convLastMs));

    if (!hasNewSinceSync && !hasUnloggedUnread) {
      continue;
    }

    let stopAtMs = prevSyncMs;
    if (unread && horizonMs > 0) {
      stopAtMs = prevSyncMs > 0 ? Math.min(prevSyncMs, horizonMs) : horizonMs;
    }

    try {
      lastBackgroundSyncByChannel.set(channelId, Date.now());
      pendingBackgroundChannels.delete(channelId);
      await syncSingleConversation(coreServices, baseUrl, conv, me, {
        resyncOnly: false,
        stopAtMs: stopAtMs > 0 ? stopAtMs : null,
        maxPages: prevSyncMs > 0 ? 10 : 2,
        pageSize: prevSyncMs > 0 ? 100 : 200,
        teamNameById,
        teamNameByGroupId,
        targetChannelName: domUnreadMap.get(channelId) || null,
      });
    } catch (err) {
      console.warn(
        `[message-logger] Background sync failed for ${channelId}:`,
        err.message
      );
    }
  }
}

function trySubscribeCommandStream(coreServices) {
  if (commandStreamSubscribed) return;
  const reportingService = coreServices?.commandChangeReportingService;
  if (typeof reportingService?.observeChanges !== "function") return;
  try {
    const observable = reportingService.observeChanges();
    if (typeof observable?.subscribe !== "function") return;
    observable.subscribe((e) => {
      try {
        if (!e) return;
        const serialized = JSON.stringify(e);
        const ids = extractCanonicalChannelIdsFromText(serialized);
        if (ids.length > 0) {
          for (const id of ids) {
            queueChannelForBackgroundSync(id);
          }
        } else if (
          e.context?.entityCommand?.entityOptions &&
          !e.context.entityCommand.entityOptions.isIncomingCall
        ) {
          scheduleIncrementalSweep(1500);
        }
      } catch {
        // ignore command stream parse errors
      }
    });
    commandStreamSubscribed = true;
  } catch {
    // ignore subscription failure
  }
}

async function processBackgroundSync() {
  if (syncInProgress || backgroundSyncRunning) {
    scheduleBackgroundWorker(2000);
    return;
  }

  const handler = globalThis.window?.teamsForLinuxReactHandler;
  const coreServices = handler?._getTeams2CoreServices?.();
  if (!coreServices) {
    scheduleBackgroundWorker(3000);
    return;
  }

  trySubscribeCommandStream(coreServices);

  backgroundSyncRunning = true;
  try {
    const baseUrl = (
      (await coreServices.discover?.regionGtm?.("chatService")) ||
      "https://teams.cloud.microsoft/api/chatsvc/emea"
    ).replace(/\/+$/, "");

    if (!backgroundStartupSweepDone) {
      backgroundStartupSweepDone = true;
      backgroundSweepNeeded = false;
      await syncUnreadAndUpdatedConversations(coreServices, baseUrl, {
        fullScan: true,
      });
    } else if (backgroundSweepNeeded) {
      backgroundSweepNeeded = false;
      await syncUnreadAndUpdatedConversations(coreServices, baseUrl, {
        fullScan: false,
      });
    }

    while (pendingBackgroundChannels.size > 0 && !syncInProgress) {
      const [channelId, info] = pendingBackgroundChannels.entries().next().value;
      pendingBackgroundChannels.delete(channelId);

      const lastRun = lastBackgroundSyncByChannel.get(channelId) || 0;
      const elapsed = Date.now() - lastRun;
      if (elapsed < 1500) {
        await sleep(1500 - elapsed);
      }

      lastBackgroundSyncByChannel.set(channelId, Date.now());
      let conv = null;
      try {
        const single = await chatSvcFetchJson(
          coreServices,
          `${baseUrl}/v1/users/ME/conversations/${encodeURIComponent(
            channelId
          )}?view=msnp24Equivalent`
        );
        if (single?.id) conv = single;
      } catch {
        // Fall back to minimal conversation descriptor
      }
      if (!conv) {
        conv = {
          id: channelId,
          threadProperties: { topic: info?.hintName || null },
        };
      }

      const me = getCurrentUser([channelId]);
      const prevSyncMs =
        selectChannelLastSync.get(channelId)?.last_sync_ms || 0;
      const horizonMs = getConvHorizonMs(conv);
      const stopAtMs =
        prevSyncMs > 0
          ? horizonMs > 0 && isConvUnread(conv)
            ? Math.min(prevSyncMs, horizonMs)
            : prevSyncMs
          : horizonMs > 0
            ? horizonMs
            : null;

      try {
        await syncSingleConversation(coreServices, baseUrl, conv, me, {
          resyncOnly: false,
          stopAtMs,
          maxPages: prevSyncMs > 0 ? 10 : 1,
          pageSize: prevSyncMs > 0 ? 50 : 200,
          targetChannelName: info?.hintName || null,
        });
      } catch (err) {
        console.warn(
          `[message-logger] Channel arrival sync failed for ${channelId}:`,
          err.message
        );
      }
    }
  } catch (err) {
    console.warn("[message-logger] Background sync error:", err.message);
  } finally {
    backgroundSyncRunning = false;
    if (pendingBackgroundChannels.size > 0 || backgroundSweepNeeded) {
      scheduleBackgroundWorker(1500);
    }
  }
}

const CANONICAL_CHANNEL_GLOBAL_RE =
  /(?:19:[A-Za-z0-9_.:-]+@(?:thread\.(?:tacv2|v2|skype)|(?:fed\.)?unq\.gbl\.spaces)|48:notes)/g;

function extractCanonicalChannelIdsFromText(raw) {
  if (!raw || typeof raw !== "string") return [];
  const found = new Set();
  const scan = (str) => {
    for (const m of str.matchAll(CANONICAL_CHANNEL_GLOBAL_RE)) {
      if (isCanonicalChannelId(m[0])) found.add(m[0]);
    }
  };
  scan(raw);
  if (raw.includes("19%3A") || raw.includes("19%3a") || raw.includes("48%3A")) {
    try {
      scan(decodeURIComponent(raw));
    } catch {
      // ignore malformed URI sequences
    }
  }
  return [...found];
}

function inspectRealtimePayload(data) {
  if (typeof data !== "string" || data.length === 0) return;
  if (
    (data.includes("Control/Typing") || data.includes("Control/ClearTyping")) &&
    !data.includes("RichText") &&
    !data.includes('"Text"') &&
    !data.includes("ThreadActivity")
  ) {
    return;
  }
  const ids = extractCanonicalChannelIdsFromText(data);
  for (const id of ids) {
    queueChannelForBackgroundSync(id);
  }
}

let realtimeHooksInstalled = false;

function installRealtimeHooks() {
  if (realtimeHooksInstalled) return;
  realtimeHooksInstalled = true;

  const NativeWebSocket = globalThis.WebSocket;
  if (typeof NativeWebSocket === "function") {
    try {
      globalThis.WebSocket = new Proxy(NativeWebSocket, {
        construct(Target, args, newTarget) {
          const ws = Reflect.construct(Target, args, newTarget);
          ws.addEventListener("message", (event) => {
            try {
              inspectRealtimePayload(event?.data);
            } catch {
              // ignore WebSocket inspection errors
            }
          });
          return ws;
        },
      });
    } catch {
      // ignore if WebSocket cannot be proxied
    }
  }

  if (typeof globalThis.fetch === "function") {
    const origFetch = globalThis.fetch;
    globalThis.fetch = function patchedFetch(input, init) {
      let matchedIds = [];
      try {
        const url =
          typeof input === "string"
            ? input
            : typeof input?.url === "string"
              ? input.url
              : "";
        if (url && (url.includes("/conversations/") || url.includes("chatsvc"))) {
          matchedIds = extractCanonicalChannelIdsFromText(url);
          for (const id of matchedIds) {
            queueChannelForBackgroundSync(id, null, 2000);
          }
        }
      } catch {
        // ignore URL inspection errors
      }
      const resPromise = Reflect.apply(origFetch, this, [input, init]);
      if (matchedIds.length > 0 && typeof resPromise?.finally === "function") {
        resPromise.finally(() => {
          for (const id of matchedIds) {
            queueChannelForBackgroundSync(id, null, 1000);
          }
          runIdle(() => syncAll());
        });
      }
      return resPromise;
    };
  }

  if (typeof globalThis.XMLHttpRequest === "function") {
    const origOpen = globalThis.XMLHttpRequest.prototype.open;
    globalThis.XMLHttpRequest.prototype.open = function patchedOpen(
      method,
      url,
      ...rest
    ) {
      try {
        if (
          typeof url === "string" &&
          (url.includes("/conversations/") || url.includes("chatsvc"))
        ) {
          const ids = extractCanonicalChannelIdsFromText(url);
          if (ids.length > 0) {
            this.addEventListener(
              "loadend",
              () => {
                for (const id of ids) {
                  queueChannelForBackgroundSync(id, null, 1000);
                }
                runIdle(() => syncAll());
              },
              { once: true }
            );
          }
        }
      } catch {
        // ignore XHR inspection errors
      }
      return Reflect.apply(origOpen, this, [method, url, ...rest]);
    };
  }

  globalThis.addEventListener("unread-count", (event) => {
    const count = Number(event?.detail?.number) || 0;
    if (count > lastObservedTitleUnreadCount) {
      scheduleIncrementalSweep(1500);
    }
    lastObservedTitleUnreadCount = count;
  });
}

ipcRenderer.on("full-archive-sync", (_event, action = "start") => {
  if (action === "pause") {
    togglePauseSync();
  } else if (action === "cancel") {
    cancelSync();
  } else if (action === "sync-current") {
    const active = resolveActiveChannel();
    if (active.id) {
      runFullArchiveSync(false, active.id, active.name);
    } else {
      showSyncStatus("Channel sync: open a conversation first", 4000);
    }
  } else if (action === "resync") {
    runFullArchiveSync(true);
  } else {
    runFullArchiveSync(false);
  }
});

function captureContextChannel(event) {
  const target = event.target;
  if (!target || target.closest?.('[role="menu"]')) return;
  const itemEl = target.closest?.(
    '[role="treeitem"], [data-tid*="list-item"], [id^="chat-header-"]'
  );
  if (!itemEl) return;

  const isContextMenu = event.type === "contextmenu";
  const isMoreButton = Boolean(
    target.closest?.(
      '[aria-haspopup="menu"], [data-tid*="more"], [data-tid*="options"]'
    )
  );
  if (!isContextMenu && !isMoreButton) return;

  const fiberId = extractConvIdFromFiber(target) || extractConvIdFromFiber(itemEl);
  if (isCanonicalChannelId(fiberId)) {
    lastContextChannelId = fiberId;
    const labelEl = itemEl.querySelector?.(
      '[data-tid="chat-list-item-title"], [data-tid="channel-list-item-text"], ' +
        TITLE_SELECTOR
    );
    lastContextChannelName = normalizeChannelName(labelEl?.textContent);
    lastContextMenuTriggerAt = Date.now();
    setTimeout(() => injectContextMenuSyncItem(), 50);
    setTimeout(() => injectContextMenuSyncItem(), 200);
  }
}

function isComposeOrIgnoredMutation(m) {
  const t = m.target;
  const el = t?.nodeType === 1 ? t : t?.parentElement;
  if (!el) return true;
  if (el.isContentEditable || el.id === "tfl-archive-sync-status") return true;
  const tag = el.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA") return true;
  return Boolean(
    el.closest?.(
      '[contenteditable="true"], [role="textbox"], #tfl-archive-sync-status'
    )
  );
}

function runIdle(fn) {
  if (typeof globalThis.requestIdleCallback === "function") {
    globalThis.requestIdleCallback(fn, { timeout: 1000 });
  } else {
    setTimeout(fn, 50);
  }
}

function handlePossibleSendAction(event) {
  const t = event.target;
  const el = t?.nodeType === 1 ? t : t?.parentElement;
  if (!el) return;

  if (event.type === "keydown") {
    if (
      event.key !== "Enter" ||
      event.shiftKey ||
      event.altKey ||
      event.isComposing
    ) {
      return;
    }
    if (
      !el.isContentEditable &&
      !el.closest?.('[contenteditable="true"], [role="textbox"]')
    ) {
      return;
    }
  } else if (event.type === "click") {
    if (
      !el.closest?.(
        '[data-tid*="send"], [data-tid="newMessageCommands-send"], [data-tid*="edit-complete"], [data-tid*="save"], button[type="submit"]'
      )
    ) {
      return;
    }
  } else {
    return;
  }

  const active = resolveActiveChannel();
  if (isCanonicalChannelId(active?.id)) {
    queueChannelForBackgroundSync(active.id, active.name, 1500);
  }
  setTimeout(() => runIdle(() => syncAll()), 500);
  setTimeout(() => runIdle(() => syncAll()), 1500);
}

function start() {
  initDbBridge();
  installRealtimeHooks();
  document.addEventListener("pointerdown", captureContextChannel, true);
  document.addEventListener("contextmenu", captureContextChannel, true);
  document.addEventListener("keydown", handlePossibleSendAction, true);
  document.addEventListener("click", handlePossibleSendAction, true);
  runIdle(() => syncAll());
  scheduleBackgroundWorker(3000);
  let scheduled = false;
  const observer = new MutationObserver((mutations) => {
    let hasRelevant = false;
    for (let i = 0; i < mutations.length; i++) {
      if (!isComposeOrIgnoredMutation(mutations[i])) {
        hasRelevant = true;
        break;
      }
    }
    if (!hasRelevant || scheduled) return;
    scheduled = true;
    setTimeout(() => {
      runIdle(() => {
        scheduled = false;
        syncAll();
      });
    }, 350);
  });
  observer.observe(document.body || document.documentElement, {
    childList: true,
    subtree: true,
    characterData: true,
    attributes: true,
    attributeFilter: ["data-mid", "aria-labelledby", "id"],
  });
}

installRealtimeHooks();

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", start, { once: true });
} else {
  start();
}
