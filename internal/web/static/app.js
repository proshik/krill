// Mounts an xterm log viewer into a `.k-term[data-ws]` element and streams logs
// over WebSocket. Mounting is driven by krillEnhance (DOMContentLoaded +
// htmx:afterSettle), so any log view added by a page swap mounts automatically;
// the data-mounted guard makes it idempotent and the singleton id is gone.
function mountTerminalEl(el) {
  if (!el || el.dataset.mounted) return;
  const wsPath = el.dataset.ws;
  if (!wsPath) return;
  // After an hx-boost swap the external xterm.js / addon-fit.js scripts may
  // still be loading; wait for them before building, and only mark mounted on
  // an actual build.
  if (typeof Terminal === "undefined" || typeof FitAddon === "undefined" || !FitAddon.FitAddon) {
    setTimeout(function () { mountTerminalEl(el); }, 50);
    return;
  }
  el.dataset.mounted = "1";

  const term = new Terminal({ convertEol: true, fontSize: 12, theme: { background: "#0f1115" } });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(el);
  // Defer the first fit to after layout: right after a swap the container has no
  // measured height yet, so an immediate fit() computes 0 rows. rAF + a
  // ResizeObserver re-fit once height exists.
  const refit = () => { try { fit.fit(); } catch (_) { /* not laid out yet */ } };
  requestAnimationFrame(refit);
  window.addEventListener("resize", refit);
  if (window.ResizeObserver) { new ResizeObserver(refit).observe(el); }

  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}${wsPath}`);
  let gotData = false;
  ws.onmessage = (e) => { gotData = true; term.writeln(e.data); };
  ws.onclose = () => term.writeln(gotData
    ? "\x1b[90m[log stream closed]\x1b[0m"
    : "\x1b[90m[no logs yet — start or redeploy the service to stream logs]\x1b[0m");
  ws.onerror = () => term.writeln("\x1b[31m[log stream error]\x1b[0m");
}

// krillEnhance wires up content present on the page or just swapped in: it mounts
// any unmounted log terminals and initializes the env editor. Idempotent.
window.krillEnhance = function () {
  document.querySelectorAll(".k-term[data-ws]:not([data-mounted])").forEach(mountTerminalEl);
  if (document.getElementById("env-form")) {
    let mode = "kv";
    try { mode = localStorage.getItem("krillEnvMode") || "kv"; } catch (_) { /* private mode */ }
    krillEnvMode(mode);
  }
  // Stop any previous status-refresh timer (the page/element may have swapped),
  // then restore the toggle from localStorage on the current page.
  if (window.__krillAutoTimer) { clearInterval(window.__krillAutoTimer); window.__krillAutoTimer = null; }
  const arcb = document.querySelector('input[data-target="status-refresh"]');
  if (arcb) {
    let on = "0";
    try { on = localStorage.getItem("krillAutoRefresh") || "0"; } catch (_) { /* private mode */ }
    if (on === "1") { arcb.checked = true; krillAutoRefresh(arcb); }
  }
};

// Toggle periodic status refresh: when checked, click the refresh button every
// 5s (it hx-get's the OOB status fragment). A single global timer, cleared by
// krillEnhance on navigation so it never leaks across page swaps.
window.krillAutoRefresh = function (cb) {
  if (window.__krillAutoTimer) { clearInterval(window.__krillAutoTimer); window.__krillAutoTimer = null; }
  try { localStorage.setItem("krillAutoRefresh", cb.checked ? "1" : "0"); } catch (_) { /* private mode */ }
  if (cb.checked) {
    window.__krillAutoTimer = setInterval(function () {
      const b = document.getElementById(cb.dataset.target);
      if (b) b.click();
    }, 5000);
  }
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
  if (d && typeof d.showModal === "function") {
    d.showModal();
    const f = d.querySelector("input, select, textarea");
    if (f) { try { f.focus(); } catch (_) { /* not focusable yet */ } }
  }
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

// Styled confirmation dialog (replaces the native confirm()). Used from a
// form's onsubmit: `return krillConfirm(this, 'message', 'Delete')`. Returns
// false to block the triggering submit, opens #k-confirm, and only submits the
// form natively when the user clicks the confirm button. The form must carry
// hx-boost="false" so HTMX does not fire its own request on the submit event.
window.krillConfirm = function (form, message, confirmLabel, kind) {
  const dlg = document.getElementById("k-confirm");
  if (!dlg || typeof dlg.showModal !== "function") return confirm(message);
  const msgEl = dlg.querySelector("#k-confirm-msg");
  const okEl = dlg.querySelector("#k-confirm-ok");
  if (msgEl) msgEl.textContent = message;
  if (okEl) {
    okEl.textContent = confirmLabel || "Delete";
    okEl.className = "k-btn " + (kind === "primary" ? "k-btn-primary" : "k-btn-danger");
  }
  window.__krillConfirmAction = function () { form.submit(); };
  dlg.showModal();
  return false;
};
// Invoked by the confirm dialog's confirm button.
window.krillConfirmRun = function () {
  const action = window.__krillConfirmAction;
  window.__krillConfirmAction = null;
  krillCloseModal("k-confirm");
  if (action) action();
};
// Clear the pending action when the dialog is dismissed (Esc/backdrop), so a
// cancel never runs it. 'close' does not bubble, so listen in the capture phase.
if (!window.__krillConfirmInit) {
  window.__krillConfirmInit = true;
  document.addEventListener("close", function (e) {
    if (e.target && e.target.id === "k-confirm") window.__krillConfirmAction = null;
  }, true);
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
// The two i18n'd messages are rendered server-side into data attributes.
window.krillConfirmDbDelete = function (form) {
  const destroy = !!(form.querySelector('[name="destroy_data"]') || {}).checked;
  const msg = destroy ? form.dataset.confirmDestroy : form.dataset.confirmKeep;
  return krillConfirm(form, msg, "Delete");
};

// Disable a submit button on form submit to prevent double-submits and signal activity.
window.krillBusy = function (btn, label) {
  if (!btn) return;
  btn.disabled = true;
  btn.dataset.label = btn.textContent;
  btn.textContent = label || "Working…";
};

// ── Environment editor: switch between a raw KEY=VALUE textarea and key/value rows.
// The hidden textarea (name="env") is always the submitted source of truth; in
// key/value mode the rows are synced into it on every edit.
window.krillEnvSyncToTextarea = function () {
  const ta = document.getElementById("env-textarea");
  if (!ta) return;
  const lines = [];
  document.querySelectorAll("#env-rows .env-row").forEach(function (r) {
    const k = r.querySelector(".env-key").value.trim();
    const v = r.querySelector(".env-val").value; // keep value as-typed (spaces are significant)
    if (k) lines.push(k + "=" + v);
  });
  ta.value = lines.join("\n");
};
window.krillEnvAddRow = function (key, val) {
  const rows = document.getElementById("env-rows");
  if (!rows) return;
  const row = document.createElement("div");
  row.className = "env-row flex items-center gap-2 mb-2";
  const k = document.createElement("input");
  k.className = "k-input env-key"; k.placeholder = "KEY"; k.value = key || "";
  const v = document.createElement("input");
  v.className = "k-input env-val"; v.placeholder = "value"; v.value = val || "";
  const del = document.createElement("button");
  del.type = "button"; del.className = "k-btn k-btn-secondary"; del.textContent = "✕";
  del.setAttribute("aria-label", "Remove variable");
  del.addEventListener("click", function () { row.remove(); krillEnvSyncToTextarea(); });
  k.addEventListener("input", krillEnvSyncToTextarea);
  v.addEventListener("input", krillEnvSyncToTextarea);
  row.appendChild(k); row.appendChild(v); row.appendChild(del);
  rows.appendChild(row);
};
window.krillEnvBuildRows = function (text) {
  const rows = document.getElementById("env-rows");
  if (!rows) return;
  rows.innerHTML = "";
  (text || "").split("\n").forEach(function (line) {
    line = line.trim();
    if (!line) return;
    const i = line.indexOf("=");
    if (i < 0) return;
    krillEnvAddRow(line.slice(0, i).trim(), line.slice(i + 1).trim());
  });
  if (!rows.children.length) krillEnvAddRow("", "");
};
window.krillEnvMode = function (mode) {
  const form = document.getElementById("env-form");
  if (!form) return;
  const raw = document.getElementById("env-raw");
  const kv = document.getElementById("env-kv");
  if (mode === "kv") {
    krillEnvBuildRows(document.getElementById("env-textarea").value);
    raw.style.display = "none";
    kv.style.display = "";
  } else {
    // only sync rows→textarea if KV rows exist, so we never wipe the textarea
    if (document.querySelectorAll("#env-rows .env-row").length) krillEnvSyncToTextarea();
    kv.style.display = "none";
    raw.style.display = "";
  }
  form.dataset.mode = mode;
  try { localStorage.setItem("krillEnvMode", mode); } catch (_) { /* private mode */ }
  document.querySelectorAll("#env-seg [data-env-mode]").forEach(function (b) {
    b.classList.toggle("k-seg-active", b.dataset.envMode === mode);
  });
};

// Run enhancements on first load and after every hx-boost swap. Guarded so a
// re-executed app.js does not stack duplicate listeners.
if (!window.__krillEnhanceInit) {
  window.__krillEnhanceInit = true;
  document.addEventListener("DOMContentLoaded", window.krillEnhance);
  document.addEventListener("htmx:afterSettle", window.krillEnhance);
}
window.krillEnhance();
