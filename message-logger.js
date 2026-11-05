const fs = require("node:fs");
const path = require("node:path");

if (
  process.env.TEAMS_LOGGER_SCRIPT &&
  path.resolve(process.env.TEAMS_LOGGER_SCRIPT) !== path.resolve(__filename)
) {
  const target = path.resolve(process.env.TEAMS_LOGGER_SCRIPT);
  delete require.cache[target];
  module.exports = require(target);
  return;
}

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

// --- Electron Main Process (DB worker supervisor + Archive Sync menu) ---
if (process.type === "browser") {
  const { app, ipcMain, webContents } = require("electron");
  const { spawn } = require("node:child_process");
  const readline = require("node:readline");
  const candidateAppDir = path.join(app.getAppPath(), "app");
  const appRoot = fs.existsSync(
    path.join(candidateAppDir, "menus", "appMenu.orig.js")
  )
    ? candidateAppDir
    : path.resolve(__dirname, "..");
  const origAppMenu = require(path.join(appRoot, "menus", "appMenu.orig.js"));
  try {
    const { allowedChannels } = require(
      path.join(appRoot, "security", "ipcValidator")
    );
    allowedChannels.add("teams-logger-db");
  } catch {
    // ignore if ipcValidator is absent
  }

  let dbWorker = null;
  let lastInitMsg = null;

  function ensureDbWorker() {
    if (dbWorker && dbWorker.stdin && !dbWorker.stdin.destroyed) {
      return dbWorker;
    }
    try {
      const workerBin = process.env.TEAMS_LOGGER_WORKER || "teams-worker";
      dbWorker = spawn(workerBin, [], {
        stdio: ["pipe", "pipe", "inherit"],
      });
      const rl = readline.createInterface({ input: dbWorker.stdout });
      rl.on("line", (line) => {
        if (!line.trim()) return;
        let msg;
        try {
          msg = JSON.parse(line);
        } catch {
          return;
        }
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
      console.error("[message-logger] Main failed to spawn teams-worker:", err);
    }
    return dbWorker;
  }

  function sendToWorker(payload) {
    const worker = ensureDbWorker();
    if (worker && worker.stdin && !worker.stdin.destroyed) {
      try {
        worker.stdin.write(`${JSON.stringify(payload)}\n`);
      } catch {
        // ignore broken pipe
      }
    }
  }

  ensureDbWorker();

  ipcMain.on("teams-logger-db", (event, payload) => {
    if (!payload || typeof payload !== "object") return;
    if (payload.type === "request-init") {
      if (lastInitMsg) {
        try {
          event.sender.send("teams-logger-db", lastInitMsg);
        } catch {
          // ignore
        }
      } else {
        sendToWorker({ type: "request-init" });
      }
      return;
    }
    sendToWorker(payload);
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
const archivedAttachmentsByMid = new Map();

let pendingAuthors = new Map();
let pendingChannels = new Map();
let pendingRecords = [];
let pendingAttachments = new Map();
let pendingLastSyncs = new Map();
let pendingGroupNameRequests = new Map();
let pendingTokens = null;
let drainWaiter = null;
let remainingWorkerAttachments = 0;
let flushTimer = null;
let dbBridgeInitialized = false;

function recordArchivedAttachmentInCache(att) {
  if (!att?.message_id || !att?.checksum) return;
  let list = archivedAttachmentsByMid.get(att.message_id);
  if (!list) {
    list = [];
    archivedAttachmentsByMid.set(att.message_id, list);
  }
  if (!list.some((x) => x.checksum === att.checksum)) {
    list.push({
      filename: att.filename || "",
      checksum: att.checksum,
      extension: att.extension || "",
    });
  }
}

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
      for (const att of msg.attachments || []) {
        recordArchivedAttachmentInCache(att);
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
    } else if (msg.type === "attachment-saved") {
      recordArchivedAttachmentInCache(msg.attachment);
    } else if (msg.type === "attachment-progress") {
      remainingWorkerAttachments = Number(msg.remaining) || 0;
    } else if (msg.type === "attachments-drained") {
      remainingWorkerAttachments = 0;
      if (drainWaiter) {
        const resolve = drainWaiter;
        drainWaiter = null;
        resolve();
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
    !pendingTokens &&
    pendingAuthors.size === 0 &&
    pendingChannels.size === 0 &&
    pendingRecords.length === 0 &&
    pendingAttachments.size === 0 &&
    pendingLastSyncs.size === 0 &&
    pendingGroupNameRequests.size === 0
  ) {
    return;
  }
  initDbBridge();

  const payload = {
    type: "flush",
    tokens: pendingTokens,
    authors: [...pendingAuthors.values()],
    channels: [...pendingChannels.values()],
    records: pendingRecords,
    attachments: [...pendingAttachments.values()],
    lastSyncs: [...pendingLastSyncs.values()],
    groupNameRequests: [...pendingGroupNameRequests.values()],
  };
  pendingTokens = null;
  pendingAuthors = new Map();
  pendingChannels = new Map();
  pendingRecords = [];
  pendingAttachments = new Map();
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
  rawContent,
  subject,
  messageType,
  rawEmotions,
  rawMentions,
  rawFiles,
  reactions,
  mentions,
  attachmentTasks,
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
    rawContent: rawContent ?? "",
    subject: subject ?? "",
    messageType: messageType ?? "",
    rawEmotions: rawEmotions ?? null,
    rawMentions: rawMentions ?? null,
    rawFiles: rawFiles ?? null,
    reactionsSummary,
    reactions: reactions && reactions.length > 0 ? reactions : null,
    mentions: mentions && mentions.length > 0 ? mentions : null,
    attachmentTasks:
      attachmentTasks && attachmentTasks.length > 0 ? attachmentTasks : null,
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
      if (isDomContentImage(node)) return;
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
    getContentEl(mid) ||
      document.getElementById(`subject-line-${mid}`) ||
      msg.querySelector?.('[data-tid*="file"], [data-tid*="attachment"], img')
  );
}

function isAmsOrMediaUrl(url) {
  if (!url || typeof url !== "string") return false;
  return (
    /\/v1\/objects\/[^/?#]+/i.test(url) ||
    url.includes("asm.skype.com") ||
    url.includes("asyncgw.teams.microsoft.com")
  );
}

function extractAmsObjectId(url, fallbackId = null) {
  if (typeof url === "string") {
    const m = url.match(/\/v1\/objects\/([^/?#]+)/i);
    if (m?.[1]) return m[1];
  }
  if (typeof fallbackId === "string" && /^0-[a-z0-9-]+$/i.test(fallbackId)) {
    return fallbackId;
  }
  return null;
}

function isEmojiOrAvatarUrl(url) {
  if (!url || typeof url !== "string") return false;
  const lower = url.toLowerCase();
  return (
    lower.includes("/emoticons/") ||
    lower.includes("/pes/") ||
    lower.includes("personal-expressions") ||
    lower.includes("statics.teams.cdn.office.net") ||
    lower.includes("profilepicture") ||
    lower.includes("8:orgid:")
  );
}

function isDomContentImage(img) {
  if (!img || img.tagName !== "IMG") return false;
  const itemtype = img.getAttribute("itemtype") || "";
  if (itemtype === "http://schema.skype.com/Emoji") return false;
  const cls = typeof img.className === "string" ? img.className : "";
  if (/\b(?:emoticon|emoji)\b/i.test(cls)) return false;
  if (
    img.closest?.(
      '[data-tid="diverse-reaction-pill-button"], [data-tid*="avatar"], [data-tid*="emoticon"]'
    )
  ) {
    return false;
  }
  const origSrc = img.getAttribute("data-orig-src") || "";
  const src = img.currentSrc || img.getAttribute("src") || "";
  if (isEmojiOrAvatarUrl(src) || isEmojiOrAvatarUrl(origSrc)) return false;
  if (
    itemtype === "http://schema.skype.com/AMSImage" ||
    img.getAttribute("data-tid") === "rich-text-ima-image" ||
    isAmsOrMediaUrl(origSrc) ||
    isAmsOrMediaUrl(src)
  ) {
    return true;
  }
  const itemid = img.getAttribute("itemid") || "";
  if (itemid && !/^0-[a-z0-9-]+$/i.test(itemid)) return false;
  return src.startsWith("blob:") || src.startsWith("data:image/");
}

const capturedDomBlobs = new Set();

function captureDomBlobImages(messageId, contentEl) {
  if (!contentEl || !messageId) return [];
  const tasks = [];
  for (const img of contentEl.querySelectorAll("img")) {
    if (!isDomContentImage(img)) continue;
    const origSrc = img.getAttribute("data-orig-src") || "";
    const src = img.currentSrc || img.getAttribute("src") || "";
    const amsId =
      extractAmsObjectId(
        origSrc,
        img.getAttribute("id") || img.getAttribute("itemid")
      ) ||
      extractAmsObjectId(
        src,
        img.getAttribute("id") || img.getAttribute("itemid")
      );
    const filename =
      img.getAttribute("data-file-name") ||
      img.getAttribute("title") ||
      img.getAttribute("alt") ||
      "";
    const key = amsId ? `ams:${amsId}` : `img:${origSrc || src}`;

    if (src.startsWith("blob:")) {
      const blobKey = `${messageId}:${key}`;
      if (!capturedDomBlobs.has(blobKey)) {
        capturedDomBlobs.add(blobKey);
        void nativeFetch(src)
          .then((r) => (r.ok ? r.arrayBuffer() : null))
          .then((ab) => {
            if (!ab || ab.byteLength === 0) return;
            pendingAttachments.set(blobKey, {
              messageId,
              filename,
              dataBase64: Buffer.from(ab).toString("base64"),
            });
            scheduleDbFlush();
          })
          .catch(() => {});
      }
    }

    if (origSrc || !src.startsWith("blob:")) {
      tasks.push({
        key,
        url: origSrc || src,
        filename,
        isInlineImage: true,
      });
    }
  }
  return tasks;
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
    const fiberMsg = getFiberMessage(msg);
    let body = extractDomTextWithEmojis(contentEl);
    if (direction === "event" || /^(?:callStarted|callEnded)\b/.test(body)) {
      if (!subject) {
        direction = "event";
        authorId = null;
        authorName = null;
      }
    }
    const content =
      subject && body && body !== subject
        ? `${subject}\n${body}`
        : subject || body;
    const reactions = extractDomReactions(msg, mid);
    const mentions = extractDomMentions(contentEl);
    const attachmentTasks = captureDomBlobImages(id, contentEl);
    const rawContent =
      typeof fiberMsg?.content === "string" ? fiberMsg.content : "";
    const rawFiles = fiberMsg?.properties?.files ?? fiberMsg?.files ?? null;
    const mentionKey = mentions.map((m) => m.id).join(",");
    const attKey = `${attachmentTasks.length}:${rawFiles ? 1 : 0}`;
    const stateKey = `${authorId ?? ""}|${content}|${rawContent.length}|${formatReactionsSummary(reactions) ?? ""}|${mentionKey}|${attKey}`;
    if (seenStateByMid.get(mid) === stateKey) continue;

    void refreshAuthTokensForWorker(coreServicesFromWindow(), [rawFiles]);
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
      rawContent,
      subject,
      messageType: fiberMsg?.messagetype || fiberMsg?.messageType || "",
      rawEmotions: fiberMsg?.properties?.emotions ?? fiberMsg?.emotions ?? null,
      rawMentions: fiberMsg?.properties?.mentions ?? fiberMsg?.mentions ?? null,
      rawFiles,
      reactions,
      mentions,
      attachmentTasks,
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

function coreServicesFromWindow() {
  return globalThis.window?.teamsForLinuxReactHandler?._getTeams2CoreServices?.();
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

async function waitForWorkerAttachments(modeLabel) {
  let done = false;
  const drainPromise = new Promise((resolve) => {
    drainWaiter = () => {
      done = true;
      resolve();
    };
  });
  try {
    ipcRenderer.send("teams-logger-db", { type: "drain-attachments" });
  } catch {
    return;
  }
  while (!done && !syncCancelled) {
    if (remainingWorkerAttachments > 0) {
      showSyncStatus(
        `${modeLabel}: downloading attachments (${remainingWorkerAttachments} remaining)...`
      );
    }
    await Promise.race([drainPromise, sleep(250)]);
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

let lastTokenRefreshMs = 0;
const cachedSharePointTokens = new Map();

function collectSharePointOrigins(rawFilesList) {
  const origins = new Set();
  for (let raw of rawFilesList) {
    if (!raw) continue;
    if (typeof raw === "string") {
      try {
        raw = JSON.parse(raw);
      } catch {
        continue;
      }
    }
    if (!Array.isArray(raw)) continue;
    for (const f of raw) {
      const fi = f?.fileInfo;
      for (const u of [
        fi?.fileUrl,
        f?.fileUrl,
        f?.objectUrl,
        fi?.shareUrl,
        f?.shareUrl,
        fi?.siteUrl,
        f?.baseUrl,
      ]) {
        if (!u || typeof u !== "string") continue;
        try {
          const origin = new URL(u).origin;
          if (origin.includes(".sharepoint.com")) origins.add(origin);
        } catch {
          // ignore invalid URL
        }
      }
    }
  }
  return [...origins];
}

async function refreshAuthTokensForWorker(coreServices, rawFilesList = []) {
  const handler = globalThis.window?.teamsForLinuxReactHandler;
  if (!handler?.acquireToken) return;
  const origins = collectSharePointOrigins(rawFilesList);
  const newOrigins = origins.filter((o) => !cachedSharePointTokens.has(o));
  const now = Date.now();
  if (now - lastTokenRefreshMs < 120000 && newOrigins.length === 0) {
    return;
  }
  lastTokenRefreshMs = now;

  try {
    const [ic3, spaces, graph] = await Promise.all([
      handler.acquireToken("https://ic3.teams.office.com").catch(() => null),
      handler.acquireToken("https://api.spaces.skype.com").catch(() => null),
      handler.acquireToken("https://graph.microsoft.com").catch(() => null),
    ]);
    for (const origin of origins) {
      const sp = await handler.acquireToken(origin).catch(() => null);
      if (sp?.token) cachedSharePointTokens.set(origin, sp.token);
    }
    pendingTokens = {
      ic3Token: ic3?.token || "",
      spacesToken: spaces?.token || "",
      skypeToken: coreServices?.authenticationUser?.skypeToken || "",
      graphToken: graph?.token || "",
      sharePointTokens: Object.fromEntries(cachedSharePointTokens),
    };
    scheduleDbFlush();
  } catch {
    // best-effort token refresh
  }
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
    let rawMentions = m.properties?.mentions || m.mentions;
    if (typeof rawMentions === "string") {
      try {
        rawMentions = JSON.parse(rawMentions);
      } catch {
        rawMentions = null;
      }
    }
    if (Array.isArray(rawMentions)) {
      for (const entry of rawMentions) {
        const menId = trackMri(entry?.mri || entry?.id || entry?.userId);
        const menName =
          entry?.displayName?.trim() || entry?.name?.trim() || null;
        if (menId && menName) upsertAuthor.run(menId, menName);
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
        const val = (text.match(/<value>([\s\S]*?)<\/value>/i)?.[1] || "")
          .replace(/<[^>]+>/g, "")
          .trim();
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

  await refreshAuthTokensForWorker(
    coreServices,
    convMessages.map((m) => m.properties?.files ?? m.files)
  );

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
      const rawFiles = m.properties?.files ?? m.files ?? null;
      const hasAttachment = Boolean(
        rawFiles || /<(?:img|URIObject)\b/i.test(m.content || "")
      );
      const isUserMsg = isRichOrText || Boolean(subject) || hasAttachment;
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
        content: "",
        rawContent: typeof m.content === "string" ? m.content : "",
        subject,
        messageType: msgType,
        rawEmotions: m.properties?.emotions ?? m.emotions ?? null,
        rawMentions: m.properties?.mentions ?? m.mentions ?? null,
        rawFiles,
        reactions: null,
        mentions: null,
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

    flushToDbWorker();
    await waitForWorkerAttachments(modeLabel);

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
