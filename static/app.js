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
let saveTimer = null;
let rendering = false;
let dirty = false;

function setStatus(text) {
  stateLabel.textContent = text;
}

async function loadState() {
  const response = await fetch("/api/state", { cache: "no-store" });
  if (!response.ok) throw new Error(await response.text());
  state = await response.json();
  render();
  setStatus("Saved");
}

async function loadFiles() {
  const response = await fetch("/api/files", { cache: "no-store" });
  if (!response.ok) throw new Error(await response.text());
  sharedFiles = await response.json();
  renderFiles();
}

function render() {
  rendering = true;
  serverSummary.textContent = summaryText();
  instructionRoot.replaceChildren(...state.instructions.map(renderInstruction));
  scriptRoot.replaceChildren(...state.scripts.map(renderScript));
  resizeInstructionTextareas();
  rendering = false;
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
  title.value = item.title;
  content.value = item.content;
  code.textContent = item.content;

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
  filename.value = item.filename;
  content.value = item.content;
  runLine.textContent = runCommand(item.filename);

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
  return `curl -fsSL ${base}/${safeName} | bash`;
}

function scriptBaseURL() {
  const host = window.location.hostname;
  const openedRemote = host && host !== "127.0.0.1" && host !== "localhost" && host !== "::1";
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
    render();
    setStatus("Saved");
  } catch (error) {
    dirty = true;
    setStatus(`Save failed: ${error.message.trim()}`);
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
    sharedFiles = [];
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
  const previous = button.textContent;
  button.textContent = text;
  window.setTimeout(() => {
    button.textContent = previous;
  }, 900);
}

function connectEvents() {
  const events = new EventSource("/api/events");
  events.addEventListener("update", () => {
    if (!dirty) loadState().catch((error) => setStatus(`Reload failed: ${error.message}`));
    loadFiles().catch((error) => {
      uploadStatus.textContent = `Reload failed: ${error.message}`;
    });
  });
  events.onerror = () => {
    setStatus("Live reload disconnected");
  };
}

window.addEventListener("resize", resizeInstructionTextareas);

loadState()
  .then(loadFiles)
  .then(connectEvents)
  .catch((error) => setStatus(`Load failed: ${error.message}`));
