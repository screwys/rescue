const stateLabel = document.querySelector("#save-state");
const serverSummary = document.querySelector("#server-summary");
const instructionRoot = document.querySelector("#instructions");
const scriptRoot = document.querySelector("#scripts");
const instructionTemplate = document.querySelector("#instruction-template");
const scriptTemplate = document.querySelector("#script-template");

let state = null;
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

function render() {
  rendering = true;
  serverSummary.textContent = summaryText();
  instructionRoot.replaceChildren(...state.instructions.map(renderInstruction));
  scriptRoot.replaceChildren(...state.scripts.map(renderScript));
  rendering = false;
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
  });
  events.onerror = () => {
    setStatus("Live reload disconnected");
  };
}

loadState()
  .then(connectEvents)
  .catch((error) => setStatus(`Load failed: ${error.message}`));
