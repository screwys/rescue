const stateLabel = document.querySelector("#save-state");
const serverSummary = document.querySelector("#server-summary");
const instructionRoot = document.querySelector("#instructions");
const scriptRoot = document.querySelector("#scripts");
const resetAll = document.querySelector("#reset-all");
const uploadForm = document.querySelector("#file-upload");
const uploadInput = document.querySelector("#shared-files");
const uploadStatus = document.querySelector("#upload-status");
const sharedFilesRoot = document.querySelector("#shared-files-list");
const downloadAll = document.querySelector("#download-all");
const downloadZip = document.querySelector("#download-zip");
const instructionTemplate = document.querySelector("#instruction-template");
const scriptTemplate = document.querySelector("#script-template");
const instructionAutoRows = 10;

let state = null;
let sharedFiles = [];
let hiddenFiles = new Set();
let stateFingerprint = "";
let filesFingerprint = "";
let saveTimer = null;
let rendering = false;
let dirty = false;
let saving = false;
let pendingRemote = false;
let syncTimer = null;
let syncingRemote = false;
let loadingLogs = false;
const logNodes = new Map();

async function loadLogs() {
  if (loadingLogs) return;
  loadingLogs = true;
  try {
    const response = await fetch("/api/logs", { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    const logs = await response.json();
    const root = document.querySelector("#logs");
    const remaining = new Set(logNodes.keys());
    let previous = null;
    for (const log of logs) {
      let node = logNodes.get(log.id);
      if (!node) {
        node = createLog(log);
        logNodes.set(log.id, node);
      }
      remaining.delete(log.id);
      const position = previous ? previous.nextElementSibling : root.firstElementChild;
      if (node !== position) root.insertBefore(node, position);
      previous = node;
      node.querySelector(".log-name").textContent = log.name;
      const status = log.finished ? `Exit ${log.exitCode ?? "?"}` : "Running";
      node.querySelector(".log-meta").textContent = [log.device, new Date(log.started).toLocaleString(), status].filter(Boolean).join(" · ");
      if (log.size > node.offset) await appendLogOutput(node, log);
    }
    for (const id of remaining) {
      logNodes.get(id).remove();
      logNodes.delete(id);
    }
    document.querySelector("#logs-status").textContent = "";
  } finally {
    loadingLogs = false;
  }
}

function createLog(log) {
  const node = document.createElement("details");
  node.className = "block log";
  node.offset = 0;
  node.decoder = new TextDecoder();
  const summary = document.createElement("summary");
  const name = document.createElement("strong");
  name.className = "log-name";
  const meta = document.createElement("span");
  meta.className = "log-meta";
  summary.append(name, meta);
  const actions = document.createElement("div");
  actions.className = "log-actions";
  const download = document.createElement("a");
  download.className = "icon-button";
  download.href = `/api/logs/${encodeURIComponent(log.id)}/download`;
  download.download = "";
  download.title = "Download log";
  download.setAttribute("aria-label", "Download log");
  download.innerHTML = downloadIcon();
  const copy = document.createElement("button");
  copy.className = "icon-button copy-snippet";
  copy.type = "button";
  copy.title = "Copy log";
  copy.setAttribute("aria-label", "Copy log");
  copy.innerHTML = copyIcon();
  const output = document.createElement("pre");
  output.className = "log-output";
  copy.addEventListener("click", () => copyText(output.textContent, copy));
  actions.append(copy, download);
  node.append(summary, actions, output);
  return node;
}

async function appendLogOutput(node, log) {
  const response = await fetch(`/api/logs/${encodeURIComponent(log.id)}/output?offset=${node.offset}`, { cache: "no-store" });
  if (!response.ok) throw new Error(await response.text());
  const bytes = await response.arrayBuffer();
  const nextOffset = response.headers.get("X-Log-Offset");
  node.offset = nextOffset === null ? node.offset + bytes.byteLength : Number(nextOffset);
  const output = node.querySelector(".log-output");
  const atBottom = output.scrollTop + output.clientHeight >= output.scrollHeight - 2;
  const text = node.decoder.decode(bytes, { stream: !log.finished || node.offset < log.size });
  if (text) output.append(document.createTextNode(text));
  if (atBottom) output.scrollTop = output.scrollHeight;
}

function setStatus(text) {
  stateLabel.textContent = text;
}

async function loadState(options = {}) {
  const response = await fetch("/api/state", { cache: "no-store" });
  if (!response.ok) throw new Error(await response.text());
  const next = await response.json();
  const fingerprint = JSON.stringify(next);
  if (!options.force && fingerprint === stateFingerprint) {
    return;
  }
  state = next;
  stateFingerprint = fingerprint;
  render();
  setStatus("Saved");
}

async function loadFiles() {
  const response = await fetch("/api/files", { cache: "no-store" });
  if (!response.ok) throw new Error(await response.text());
  const next = await response.json();
  const fingerprint = JSON.stringify(next);
  if (fingerprint === filesFingerprint) {
    return;
  }
  sharedFiles = next;
  filesFingerprint = fingerprint;
  renderFiles();
}

function render() {
  rendering = true;
  serverSummary.textContent = summaryText();
  updateBlocks(instructionRoot, state.instructions, renderInstruction);
  updateBlocks(scriptRoot, state.scripts, renderScript);
  rendering = false;
}

function updateBlocks(root, items, create) {
  const existing = new Map(Array.from(root.children, (node) => [node.dataset.id, node]));
  let previous = null;
  for (const item of items) {
    let node = existing.get(item.id);
    const added = !node;
    if (!node) node = create(item);
    node.dataset.id = item.id;
    node.updateItem(item);
    existing.delete(item.id);
    const position = previous ? previous.nextElementSibling : root.firstElementChild;
    if (node !== position) root.insertBefore(node, position);
    if (added && node.querySelector('.instruction-body')) {
      resizeInstructionTextarea(node.querySelector('.instruction-body'));
    }
    previous = node;
  }
  for (const node of existing.values()) node.remove();
}

function renderFiles() {
  const files = visibleFiles();
  const hasFiles = files.length > 0;
  downloadAll.disabled = !hasFiles;
  downloadZip.classList.toggle("disabled", !hasFiles);
  downloadZip.setAttribute("aria-disabled", String(!hasFiles));
  downloadZip.href = zipURL(files);
  if (sharedFiles.length === 0) {
    const empty = document.createElement("li");
    empty.className = "empty-files";
    empty.textContent = "No shared files yet.";
    sharedFilesRoot.replaceChildren(empty);
    return;
  }
  if (files.length === 0) {
    const empty = document.createElement("li");
    empty.className = "empty-files";
    empty.textContent = "No shared files selected.";
    sharedFilesRoot.replaceChildren(empty);
    return;
  }
  sharedFilesRoot.replaceChildren(...files.map(renderFile));
}

function renderFile(file) {
  const item = document.createElement("li");
  item.className = "shared-file";

  const href = `/files/${encodeURIComponent(file.name)}`;
  const link = document.createElement("a");
  link.className = "shared-link";
  link.href = href;
  link.download = file.name;

  const name = document.createElement("span");
  name.className = "shared-name";
  name.textContent = file.name;

  const meta = document.createElement("span");
  meta.className = "shared-meta";
  meta.textContent = formatBytes(file.size);

  link.append(name, meta);

  const download = document.createElement("a");
  download.className = "download-file";
  download.href = href;
  download.download = file.name;
  download.setAttribute("aria-label", `Download ${file.name}`);
  download.title = "Download";
  download.innerHTML = downloadIcon();

  const hide = document.createElement("button");
  hide.className = "hide-file";
  hide.type = "button";
  hide.setAttribute("aria-label", `Remove ${file.name} from list`);
  hide.title = "Remove from list";
  hide.innerHTML = closeIcon();
  hide.addEventListener("click", () => {
    hiddenFiles.add(file.name);
    renderFiles();
  });

  item.append(link, download, hide);
  return item;
}

function summaryText() {
  const urls = [state.server.localUrl, ...state.server.lanUrls].filter(Boolean);
  return `${urls.join("  ")} | ${state.server.file}`;
}

function renderInstruction(item) {
  const node = instructionTemplate.content.firstElementChild.cloneNode(true);
  const title = node.querySelector(".title-input");
  const content = node.querySelector(".content-input");
  const code = node.querySelector("code");
  const pre = node.querySelector("pre");
  const copy = node.querySelector(".copy-snippet");
  copy.innerHTML = copyIcon();
  title.value = item.title;
  content.value = item.content;
  code.textContent = item.content;
  node.updateItem = (next) => {
    item = next;
    if (title.value !== item.title) title.value = item.title;
    if (content.value !== item.content) content.value = item.content;
    if (code.textContent !== item.content) code.textContent = item.content;
  };

  title.addEventListener("input", () => {
    item.title = title.value;
    scheduleSave();
  });
  content.addEventListener("input", () => {
    item.content = content.value;
    code.textContent = item.content;
    resizeInstructionTextarea(content);
    scheduleSave();
  });
  pre.addEventListener("click", () => copyText(item.content, copy));
  pre.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      copyText(item.content, copy);
    }
  });
  copy.addEventListener("click", () => copyText(item.content, copy));
  node.querySelector(".remove").addEventListener("click", () => {
    state.instructions = state.instructions.filter((entry) => entry.id !== item.id);
    render();
    scheduleSave();
  });
  return node;
}

function renderScript(item) {
  const node = scriptTemplate.content.firstElementChild.cloneNode(true);
  const filename = node.querySelector(".filename-input");
  const content = node.querySelector(".content-input");
  const runLine = node.querySelector(".run-line");
  const copy = node.querySelector(".copy-snippet");
  copy.innerHTML = copyIcon();
  filename.value = item.filename;
  content.value = item.content;
  runLine.textContent = runCommand(item.filename);
  node.updateItem = (next) => {
    item = next;
    if (filename.value !== item.filename) filename.value = item.filename;
    if (content.value !== item.content) content.value = item.content;
    const command = runCommand(item.filename);
    if (runLine.textContent !== command) runLine.textContent = command;
  };

  filename.addEventListener("input", () => {
    item.filename = filename.value;
    runLine.textContent = runCommand(item.filename);
    scheduleSave();
  });
  content.addEventListener("input", () => {
    item.content = content.value;
    scheduleSave();
  });
  runLine.addEventListener("click", () => copyText(runLine.textContent, copy));
  runLine.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      copyText(runLine.textContent, copy);
    }
  });
  copy.addEventListener("click", () => copyText(runLine.textContent, copy));
  node.querySelector(".remove").addEventListener("click", () => {
    state.scripts = state.scripts.filter((entry) => entry.id !== item.id);
    render();
    scheduleSave();
  });
  return node;
}

function runCommand(filename) {
  const base = scriptBaseURL();
  const safeName = encodeURIComponent(filename || "install.sh");
  const windows = /Windows/i.test(navigator.userAgent);
  const url = `${base}/run/${safeName}?platform=${windows ? "windows" : "unix"}`;
  if (windows) return `Invoke-RestMethod '${url.replaceAll("'", "''")}' | Invoke-Expression`;
  return `curl -fsSL '${url.replaceAll("'", "'\\''")}' | bash`;
}

function scriptBaseURL() {
  const host = window.location.hostname;
  const openedRemote = host && !["127.0.0.1", "localhost", "::1", "[::1]"].includes(host);
  if (openedRemote) return window.location.origin;
  return state.server.lanUrls[0] || window.location.origin || state.server.localUrl;
}

function scheduleSave() {
  if (rendering) return;
  dirty = true;
  setStatus("Saving...");
  clearTimeout(saveTimer);
  saveTimer = setTimeout(saveNow, 450);
}

async function saveNow() {
  if (!dirty) return;
  dirty = false;
  saving = true;
  let saved = false;
  const payload = {
    version: state.version,
    instructions: state.instructions,
    scripts: state.scripts,
  };
  try {
    const response = await fetch("/api/state", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!response.ok) throw new Error(await response.text());
    state = await response.json();
    stateFingerprint = JSON.stringify(state);
    render();
    setStatus("Saved");
    saved = true;
  } catch (error) {
    dirty = true;
    setStatus(`Save failed: ${error.message.trim()}`);
  } finally {
    saving = false;
  }
  if (saved && pendingRemote) {
    pendingRemote = false;
    await syncRemote();
  }
}

document.querySelector("#add-instruction").addEventListener("click", () => {
  const next = nextNumber(state.instructions, "instruction");
  state.instructions.push({
    id: `instruction-${next}`,
    title: `Instruction ${next}`,
    content: "",
  });
  render();
  scheduleSave();
});

document.querySelector("#add-script").addEventListener("click", () => {
  const next = nextNumber(state.scripts, "script");
  state.scripts.push({
    id: `script-${next}`,
    filename: `script-${next}.sh`,
    content: "",
  });
  render();
  scheduleSave();
});

resetAll.addEventListener("click", async () => {
  const confirmed = window.confirm(
    "Reset instructions, scripts, and shared files? This cannot be undone.",
  );
  if (!confirmed) return;

  clearTimeout(saveTimer);
  dirty = false;
  setStatus("Resetting...");
  uploadStatus.textContent = "";
  try {
    const response = await fetch("/api/reset", { method: "POST" });
    if (!response.ok) throw new Error(await response.text());
    state = await response.json();
    stateFingerprint = JSON.stringify(state);
    sharedFiles = [];
    filesFingerprint = JSON.stringify(sharedFiles);
    hiddenFiles = new Set();
    uploadInput.value = "";
    render();
    renderFiles();
    setStatus("Reset");
  } catch (error) {
    setStatus(`Reset failed: ${error.message.trim()}`);
  }
});

uploadForm.addEventListener("submit", (event) => {
  event.preventDefault();
});

uploadInput.addEventListener("change", uploadSelectedFiles);
downloadAll.addEventListener("click", downloadAllFiles);
downloadZip.addEventListener("click", (event) => {
  if (visibleFiles().length === 0) {
    event.preventDefault();
  }
});
downloadAll.innerHTML = downloadIcon();
downloadZip.innerHTML = zipIcon();

async function uploadSelectedFiles() {
  if (!uploadInput.files.length) {
    return;
  }
  const names = Array.from(uploadInput.files, (file) => file.name);
  const body = new FormData();
  for (const file of uploadInput.files) {
    body.append("files", file);
  }
  uploadStatus.textContent = "Uploading...";
  try {
    const response = await fetch("/api/files", { method: "POST", body });
    if (!response.ok) throw new Error(await response.text());
    names.forEach((name) => hiddenFiles.delete(name));
    uploadInput.value = "";
    uploadStatus.textContent = "Uploaded";
    await loadFiles();
  } catch (error) {
    uploadStatus.textContent = `Upload failed: ${error.message.trim()}`;
  }
}

function downloadAllFiles() {
  const files = visibleFiles();
  if (files.length === 0) return;
  files.forEach((file, index) => {
    window.setTimeout(() => {
      const link = document.createElement("a");
      link.href = `/files/${encodeURIComponent(file.name)}`;
      link.download = file.name;
      link.style.display = "none";
      document.body.append(link);
      link.click();
      link.remove();
    }, index * 120);
  });
}

function visibleFiles() {
  return sharedFiles.filter((file) => !hiddenFiles.has(file.name));
}

function zipURL(files) {
  if (files.length === 0) return "/files.zip";
  const params = new URLSearchParams();
  files.forEach((file) => params.append("name", file.name));
  return `/files.zip?${params.toString()}`;
}

function nextNumber(items, prefix) {
  const used = new Set(
    items
      .map((item) => item.id || "")
      .map((id) => id.match(new RegExp(`^${prefix}-(\\d+)$`)))
      .filter(Boolean)
      .map((match) => Number(match[1])),
  );
  let next = 1;
  while (used.has(next)) next += 1;
  return next;
}

function formatBytes(size) {
  if (size < 1024) return `${size} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = size / 1024;
  let unit = units.shift();
  while (value >= 1024 && units.length > 0) {
    value /= 1024;
    unit = units.shift();
  }
  return `${value.toFixed(value >= 10 ? 0 : 1)} ${unit}`;
}

function downloadIcon() {
  return `
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M12 3v12"></path>
      <path d="m7 10 5 5 5-5"></path>
      <path d="M5 21h14"></path>
    </svg>
  `;
}

function zipIcon() {
  return `
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M6 3h8l4 4v14H6z"></path>
      <path d="M14 3v5h5"></path>
      <path d="M9 12h2"></path>
      <path d="M9 16h6"></path>
      <path d="M13 12h2"></path>
    </svg>
  `;
}

function closeIcon() {
  return `
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M18 6 6 18"></path>
      <path d="m6 6 12 12"></path>
    </svg>
  `;
}

function copyIcon() {
  return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="8" y="8" width="12" height="13" rx="2"/><path d="M16 8V5a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h3"/></svg>';
}

function checkIcon() {
  return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m5 12 4 4L19 6"/></svg>';
}

function resizeInstructionTextareas() {
  document.querySelectorAll(".instruction-body").forEach(resizeInstructionTextarea);
}

function resizeInstructionTextarea(textarea) {
  const style = window.getComputedStyle(textarea);
  const fontSize = parseFloat(style.fontSize) || 14;
  const lineHeight = parseFloat(style.lineHeight) || fontSize * 1.45;
  const padding =
    (parseFloat(style.paddingTop) || 0) + (parseFloat(style.paddingBottom) || 0);
  const border =
    (parseFloat(style.borderTopWidth) || 0) + (parseFloat(style.borderBottomWidth) || 0);
  const minHeight = parseFloat(style.minHeight) || 0;
  const maxAutoHeight = lineHeight * instructionAutoRows + padding + border;

  textarea.style.height = "auto";
  textarea.style.height = `${Math.max(
    minHeight,
    Math.min(textarea.scrollHeight + border, maxAutoHeight),
  )}px`;
}

async function copyText(text, button) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
    } else {
      const textarea = document.createElement("textarea");
      textarea.value = text;
      textarea.style.position = "fixed";
      textarea.style.opacity = "0";
      document.body.append(textarea);
      textarea.select();
      document.execCommand("copy");
      textarea.remove();
    }
    flashCopy(button, "Copied");
  } catch {
    flashCopy(button, "Copy failed");
  }
}

function flashCopy(button, text) {
  clearTimeout(button.copyTimer);
  button.innerHTML = text === "Copied" ? checkIcon() : closeIcon();
  button.title = text;
  button.copyTimer = window.setTimeout(() => {
    button.innerHTML = copyIcon();
    button.title = "Copy";
  }, 900);
}

async function syncRemote() {
  if (syncingRemote) return;
  syncingRemote = true;
  try {
    const requests = [
      loadFiles().catch((error) => {
        uploadStatus.textContent = `Reload failed: ${error.message}`;
      }),
      loadTargets().catch((error) => {
        document.querySelector("#pairing-status").textContent = error.message;
      }),
      loadLogs().catch((error) => {
        document.querySelector("#logs-status").textContent = error.message.trim();
      }),
    ];
    if (dirty || saving) {
      pendingRemote = true;
    } else {
      requests.push(loadState().catch((error) => setStatus(`Reload failed: ${error.message}`)));
    }
    await Promise.all(requests);
  } finally {
    syncingRemote = false;
  }
}

function connectEvents() {
  const events = new EventSource("/api/events");
  events.addEventListener("update", () => {
    syncRemote();
  });
  events.addEventListener("ready", () => {
    setStatus("Saved");
  });
  events.onerror = () => {
    setStatus("Live reload disconnected");
  };
}

function startSyncFallback() {
  clearInterval(syncTimer);
  syncTimer = setInterval(syncRemote, 2500);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) syncRemote();
  });
  window.addEventListener("focus", syncRemote);
}

window.addEventListener("resize", resizeInstructionTextareas);

loadState({ force: true })
  .then(loadFiles)
  .then(() => {
    generatePairing();
    syncRemote();
    connectEvents();
    startSyncFallback();
  })
  .catch((error) => setStatus(`Load failed: ${error.message}`));

const pairingForm = document.querySelector("#pairing-form");
const pairingCode = document.querySelector("#pairing-command code");
const pairingCopy = document.querySelector("#copy-pairing");
pairingCopy.innerHTML = copyIcon();
const pairingStatus = document.querySelector("#pairing-status");
let pairingRevision = 0;
let pairingTimer;
let targetsFingerprint = "";

async function generatePairing() {
  const revision = ++pairingRevision;
  pairingCopy.disabled = true;
  pairingStatus.textContent = "";
  try {
    const response = await fetch("/api/pairing", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        platform: /Windows/i.test(navigator.userAgent) ? "windows" : "unix",
        name: document.querySelector("#target-name").value,
        root: document.querySelector('input[name="target-access"]:checked').value === "root",
        persistent: document.querySelector("#target-persist").checked,
        url: scriptBaseURL(),
      }),
    });
    if (!response.ok) throw new Error(await response.text());
    const result = await response.json();
    if (revision !== pairingRevision) return;
    pairingCode.textContent = result.command;
    pairingCopy.disabled = false;
  } catch (error) {
    if (revision === pairingRevision) pairingStatus.textContent = error.message.trim();
  }
}

pairingForm.addEventListener("submit", (event) => {
  event.preventDefault();
  clearTimeout(pairingTimer);
  generatePairing();
});
pairingForm.addEventListener("input", (event) => {
  if (!event.target.matches("input, select")) return;
  ++pairingRevision;
  pairingCopy.disabled = true;
  clearTimeout(pairingTimer);
  pairingTimer = setTimeout(generatePairing, 300);
});
pairingCopy.addEventListener("click", () => copyText(pairingCode.textContent, pairingCopy));
const pairingPreview = document.querySelector("#pairing-command");
pairingPreview.addEventListener("click", () => {
  if (!pairingCopy.disabled) copyText(pairingCode.textContent, pairingCopy);
});
pairingPreview.addEventListener("keydown", (event) => {
  if ((event.key === "Enter" || event.key === " ") && !pairingCopy.disabled) {
    event.preventDefault();
    copyText(pairingCode.textContent, pairingCopy);
  }
});

async function loadTargets() {
  const response = await fetch("/api/targets", { cache: "no-store" });
  if (!response.ok) throw new Error(await response.text());
  const targets = await response.json();
  const fingerprint = JSON.stringify(targets);
  if (fingerprint === targetsFingerprint) return;
  targetsFingerprint = fingerprint;
  updateBlocks(document.querySelector("#targets"), targets, (target) => {
    const row = document.createElement("li");
    const name = document.createElement("strong");
    const details = document.createElement("span");
    row.append(name, details);
    const revoke = document.createElement("button");
    revoke.type = "button";
    revoke.className = "remove-target";
    revoke.title = "Remove device";
    revoke.innerHTML = closeIcon();
    revoke.addEventListener("click", async () => {
      try {
        const response = await fetch(`/api/targets/${encodeURIComponent(target.id)}`, { method: "DELETE" });
        if (!response.ok) throw new Error(await response.text());
        await loadTargets();
      } catch (error) { pairingStatus.textContent = error.message.trim(); }
    });
    name.after(revoke);
    row.updateItem = (next) => {
      target = next;
      name.textContent = target.name;
      details.textContent = `${target.user} · ${target.os}/${target.arch} · ${target.persistent ? "Start at boot" : "Temporary"} · ${target.connected ? "Connected" : "Offline"}`;
      revoke.setAttribute("aria-label", `Remove ${target.name}`);
    };
    return row;
  });
}
