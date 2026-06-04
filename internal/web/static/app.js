// Mounts xterm into #terminal and streams logs over WebSocket.
window.mountLogTerminal = function (wsPath) {
  const el = document.getElementById("terminal");
  if (!el || el.dataset.mounted) return;
  el.dataset.mounted = "1";

  const term = new Terminal({ convertEol: true, fontSize: 12, theme: { background: "#0f1115" } });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();
  window.addEventListener("resize", () => fit.fit());

  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}${wsPath}`);
  ws.onmessage = (e) => term.writeln(e.data);
  ws.onclose = () => term.writeln("\x1b[90m[log stream closed]\x1b[0m");
  ws.onerror = () => term.writeln("\x1b[31m[log stream error]\x1b[0m");
};

// Toggles the source fields in the application creation form.
window.krillToggleSource = function (val) {
  document.querySelectorAll('[data-src]').forEach(function (el) {
    el.style.display = (el.getAttribute('data-src') === val) ? '' : 'none';
  });
};

// Open/close the native <dialog> modal by id.
window.krillOpenModal = function (id) {
  const d = document.getElementById(id);
  if (d && typeof d.showModal === "function") d.showModal();
};
window.krillCloseModal = function (id) {
  const d = document.getElementById(id);
  if (d && typeof d.close === "function") d.close();
};
// Clicking the backdrop (outside the content) closes the modal.
// Guarded so hx-boost re-running this script does not stack duplicate listeners.
if (!window.__krillClickInit) {
  window.__krillClickInit = true;
  document.addEventListener("click", function (e) {
    if (e.target && e.target.tagName === "DIALOG" && e.target.classList.contains("k-modal")) {
      e.target.close();
    }
  });
}

// Copy text to the clipboard and show a confirmation toast.
window.krillCopyToClip = function (text, label) {
  const done = function () { krillShowToast((label || "Copied") + " to clipboard", "ok"); };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done).catch(function () { krillFallbackCopy(text, done); });
  } else {
    krillFallbackCopy(text, done);
  }
};
function krillFallbackCopy(text, done) {
  const ta = document.createElement("textarea");
  ta.value = text; ta.style.position = "fixed"; ta.style.opacity = "0";
  document.body.appendChild(ta); ta.focus(); ta.select();
  try { document.execCommand("copy"); done(); } catch (e) { /* ignore */ }
  document.body.removeChild(ta);
}

// Show a transient toast. kind: "ok" | "err".
window.krillShowToast = function (msg, kind) {
  let host = document.getElementById("k-toast-host");
  if (!host) {
    host = document.createElement("div");
    host.id = "k-toast-host";
    host.className = "k-toast-host";
    document.body.appendChild(host);
  }
  const el = document.createElement("div");
  el.className = "k-toast k-toast-" + (kind === "err" ? "err" : "ok") + " k-toast-pop";
  el.textContent = msg;
  el.addEventListener("click", function () { krillDismissToast(el); });
  host.appendChild(el);
  setTimeout(function () { krillDismissToast(el); }, 4000);
};
window.krillDismissToast = function (el) {
  if (!el || el.dataset.gone) return;
  el.dataset.gone = "1";
  el.classList.add("k-toast-out");
  setTimeout(function () { if (el.parentNode) el.parentNode.removeChild(el); }, 250);
};

// Confirm DB deletion, reflecting whether the data volume will be destroyed.
window.krillConfirmDbDelete = function (form) {
  const name = form.dataset.dbName || "this database";
  const destroy = !!(form.querySelector('[name="destroy_data"]') || {}).checked;
  const msg = destroy
    ? 'Delete "' + name + '" AND permanently destroy its data volume? This cannot be undone.'
    : 'Delete "' + name + '"? The data volume will be kept.';
  return confirm(msg);
};

// Disable a submit button on form submit to prevent double-submits and signal activity.
window.krillBusy = function (btn, label) {
  if (!btn) return;
  btn.disabled = true;
  btn.dataset.label = btn.textContent;
  btn.textContent = label || "Working…";
};
