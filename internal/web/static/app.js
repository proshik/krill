// Mounts an xterm log viewer into a `.term[data-term-log]` element and streams logs
// over WebSocket. Mounting is driven by krillEnhance (DOMContentLoaded +
// htmx:afterSettle), so any log view added by a page swap mounts automatically;
// the data-mounted guard makes it idempotent and the singleton id is gone.
const KRILL_TERM_THEME = { background: "#1c1d20", foreground: "#d6d7da", cursor: "#a9a6d8", selectionBackground: "#3a3950" };
function mountTerminalEl(el) {
  if (!el || el.dataset.mounted) return;
  const wsPath = el.dataset.termLog;
  if (!wsPath) return;
  // After an hx-boost swap the external xterm.js / addon-fit.js scripts may
  // still be loading; wait for them before building, and only mark mounted on
  // an actual build.
  if (typeof Terminal === "undefined" || typeof FitAddon === "undefined" || !FitAddon.FitAddon) {
    setTimeout(function () { mountTerminalEl(el); }, 50);
    return;
  }
  el.dataset.mounted = "1";

  const term = new Terminal({ convertEol: true, fontSize: 12, theme: KRILL_TERM_THEME });
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

// Mounts an INTERACTIVE exec terminal into `.term[data-term-ws]`: xterm with
// stdin (binary frames), resize (text JSON frames), and binary output. Status
// text is hardcoded English to match the log viewer.
function mountExecTerminal(el) {
  if (!el || el.dataset.mounted) return;
  if (typeof Terminal === "undefined" || typeof FitAddon === "undefined" || !FitAddon.FitAddon) {
    setTimeout(function () { mountExecTerminal(el); }, 50);
    return;
  }
  el.dataset.mounted = "1";
  const term = new Terminal({ fontSize: 12, cursorBlink: true, theme: KRILL_TERM_THEME });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(el);
  const refit = () => { try { fit.fit(); } catch (_) { /* not laid out */ } };
  requestAnimationFrame(refit);
  window.addEventListener("resize", refit);
  if (window.ResizeObserver) { new ResizeObserver(refit).observe(el); }

  const statusEl = document.getElementById("term-status");
  const setStatus = (s) => { if (statusEl) statusEl.textContent = s; };
  let ws = null;
  const sendResize = () => { if (ws && ws.readyState === 1) ws.send(JSON.stringify({ rows: term.rows, cols: term.cols })); };
  term.onData((d) => { if (ws && ws.readyState === 1) ws.send(new TextEncoder().encode(d)); });
  term.onResize(() => sendResize());

  function connect() {
    if (ws) { try { ws.close(); } catch (_) {} ws = null; }
    term.reset();
    const cmd = (document.getElementById("term-cmd") || {}).value || "/bin/sh";
    const proto = location.protocol === "https:" ? "wss" : "ws";
    setStatus("connecting…");
    ws = new WebSocket(`${proto}://${location.host}${el.dataset.termWs}?cmd=${encodeURIComponent(cmd)}`);
    ws.binaryType = "arraybuffer";
    ws.onopen = () => { setStatus(""); refit(); sendResize(); term.focus(); };
    ws.onmessage = (e) => term.write(new Uint8Array(e.data));
    ws.onclose = () => setStatus("session ended — press Connect to start a new one");
    ws.onerror = () => setStatus("connection error");
  }
  el._krillConnect = connect;
  connect();
}

// Mounts the structured log viewer into `[data-logs]`: streams JSON frames
// {t,lvl,msg}, renders time|level|message rows with per-level colors, and does
// client-side text search + level filtering. Status text is hardcoded English.
function mountLogViewer(el) {
  if (!el || el.dataset.mounted) return;
  el.dataset.mounted = "1";
  const MAX_ROWS = 2000;
  const body = el.querySelector("[data-logs-body]");
  const searchEl = el.querySelector("[data-logs-search]");
  let level = "all";
  let query = "";

  function rowVisible(row) {
    const lvlOK = level === "all" || row.dataset.level === level;
    const txtOK = query === "" || row.dataset.text.indexOf(query) !== -1;
    return lvlOK && txtOK;
  }
  function applyFilter() {
    body.querySelectorAll("li").forEach((r) => { r.hidden = !rowVisible(r); });
  }
  if (searchEl) {
    searchEl.addEventListener("input", () => { query = searchEl.value.toLowerCase(); applyFilter(); });
  }
  const chips = el.querySelectorAll("[data-level]");
  chips.forEach((chip) => {
    chip.addEventListener("click", () => {
      level = chip.dataset.level;
      chips.forEach((c) => c.setAttribute("aria-pressed", String(c === chip)));
      applyFilter();
    });
  });

  function fmtTime(t) {
    if (!t) return "";
    const d = new Date(t);
    if (isNaN(d.getTime())) return "";
    const p = (n) => String(n).padStart(2, "0");
    const today = new Date();
    const clock = `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
    return d.toDateString() === today.toDateString() ? clock : `${p(d.getMonth() + 1)}-${p(d.getDate())} ${clock}`;
  }
  function addRow(line) {
    const row = document.createElement("li");
    const lv = (line.lvl || "").toLowerCase();
    row.dataset.level = lv;
    row.dataset.text = (line.msg || "").toLowerCase();
    const t = document.createElement("time");
    t.textContent = fmtTime(line.t);
    if (line.t) t.dateTime = line.t;
    const lvl = document.createElement("span");
    lvl.className = "lvl" + (lv ? " lvl--" + lv : "");
    lvl.textContent = lv;
    const msg = document.createElement("code");
    msg.textContent = line.msg || "";
    row.append(t, lvl, msg);
    row.hidden = !rowVisible(row);
    const nearBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 40;
    body.appendChild(row);
    while (body.childElementCount > MAX_ROWS) body.removeChild(body.firstElementChild);
    if (nearBottom) body.scrollTop = body.scrollHeight;
  }

  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}${el.dataset.logs}`);
  let got = false;
  ws.onmessage = (e) => { got = true; try { addRow(JSON.parse(e.data)); } catch (_) { /* ignore */ } };
  ws.onclose = () => { if (!got) addRow({ msg: el.dataset.empty || "no logs yet" }); };
  ws.onerror = () => addRow({ lvl: "error", msg: el.dataset.error || "log stream error" });
  // A boosted navigation away replaces the body: close the stream with it.
  const stop = () => { try { ws.close(); } catch (_) { /* closed */ } document.removeEventListener("htmx:beforeSwap", onSwap); };
  const onSwap = (e) => { if (e.detail && e.detail.target === document.body) stop(); };
  document.addEventListener("htmx:beforeSwap", onSwap);
}

// krillExecConnect (re)connects the terminal using the current command field.
window.krillExecConnect = function () {
  const el = document.querySelector(".term[data-term-ws]");
  if (el && el._krillConnect) el._krillConnect();
};

// The monitoring page: CPU and memory per node as SVG lines (% of the node),
// and a column per node with its containers. Polls every 10 s; the timer is
// cleared by krillEnhance when the page is left.
function mountMonitoring(el) {
  if (!el || el.dataset.mounted) return;
  el.dataset.mounted = "1";
  const base = el.dataset.url;
  const t = el.dataset;
  let range = "24h", nodeFilter = "", last = null;
  const status = el.querySelector(".mon-status");
  const nodeSeg = el.querySelector("[data-mon-nodes]");
  el.querySelectorAll("[data-range]").forEach((b) => b.addEventListener("click", () => {
    range = b.dataset.range;
    el.querySelectorAll("[data-range]").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
    load();
  }));
  nodeSeg.addEventListener("click", (e) => {
    const b = e.target.closest("button[data-node]");
    if (!b) return;
    nodeFilter = b.dataset.node;
    nodeSeg.querySelectorAll("button").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
    if (last) build(last);
  });
  const esc = (s) => { const d = document.createElement("div"); d.textContent = s == null ? "" : String(s); return d.innerHTML; };
  const gib = (b) => b >= 1073741824 ? (b / 1073741824).toFixed(2) + " GiB" : Math.round(b / 1048576) + " MiB";
  const mib = (b) => Math.round(b / 1048576) + " MiB";
  const CLS = ["s1", "s2", "s3", "s4", "s5", "s6"];
  function nodeSeries(d) {
    // Per node, per bucket: sum of the containers' values, as % of the node.
    const cap = {};
    (d.nodes || []).forEach((n) => { cap[n.node] = n; });
    const out = {};
    const len = (d.x || []).length;
    (d.series || []).forEach((s) => {
      const n = cap[s.node] || {};
      if (!out[s.node]) out[s.node] = { cpu: new Array(len).fill(null), mem: new Array(len).fill(null) };
      for (let i = 0; i < len; i++) {
        const c = s.cpu[i], m = s.mem[i];
        if (c != null && n.ncpu) out[s.node].cpu[i] = (out[s.node].cpu[i] || 0) + c / n.ncpu;
        if (m != null && n.mem_total) out[s.node].mem[i] = (out[s.node].mem[i] || 0) + m * 1048576 * 100 / n.mem_total;
      }
    });
    return out;
  }
  function path(pts) {
    let d = "", pen = false;
    const n = pts.length;
    pts.forEach((v, i) => {
      if (v == null) { pen = false; return; }
      const x = (n > 1 ? i * 600 / (n - 1) : 0).toFixed(1);
      const y = (200 - Math.max(0, Math.min(100, v)) * 2).toFixed(1);
      d += (pen ? "L" : "M") + x + " " + y;
      pen = true;
    });
    return d;
  }
  function lastVal(pts) { for (let i = pts.length - 1; i >= 0; i--) if (pts[i] != null) return pts[i]; return null; }
  function chart(kind, series, names) {
    const sec = el.querySelector('[data-chart="' + kind + '"]');
    const svg = sec.querySelector("svg");
    let html = '<line x1="0" y1="0.5" x2="600" y2="0.5"/><line x1="0" y1="100" x2="600" y2="100"/><line x1="0" y1="199.5" x2="600" y2="199.5"/>';
    let ends = "";
    names.forEach((n, i) => {
      if (nodeFilter && n !== nodeFilter) return;
      const pts = (series[n] || {})[kind] || [];
      html += '<path class="' + CLS[i % CLS.length] + '" d="' + path(pts) + '"/>';
      const v = lastVal(pts);
      ends += '<li class="' + CLS[i % CLS.length] + '"><span class="swatch"></span>' + esc(n) + "<b>" + (v == null ? "—" : Math.round(v) + "%") + "</b></li>";
    });
    svg.innerHTML = html;
    sec.querySelector(".ends").innerHTML = ends;
  }
  function spark(pts) {
    return '<svg class="spark" viewBox="0 0 120 32" preserveAspectRatio="none" aria-hidden="true"><path d="' +
      path(pts).replace(/([ML])([\d.]+) ([\d.]+)/g, (m, c, x, y) => c + (x / 5).toFixed(1) + " " + (y * 0.16).toFixed(1)) + '"/></svg>';
  }
  function meter(pct, warn) {
    return '<span class="meter' + (warn ? " meter--warn" : "") + '" aria-hidden="true"><span style="width:' + Math.max(0, Math.min(100, pct)) + '%"></span></span>';
  }
  function build(d) {
    last = d;
    status.textContent = "";
    const nodes = (d.nodes || []).map((n) => n.node);
    // The node switch keeps the current choice when the node still exists.
    if (nodeFilter && nodes.indexOf(nodeFilter) < 0) nodeFilter = "";
    const all = nodeSeg.querySelector('[data-node=""]');
    nodeSeg.querySelectorAll("button:not([data-node=''])").forEach((b) => b.remove());
    nodes.forEach((n) => {
      const b = document.createElement("button");
      b.type = "button"; b.dataset.node = n; b.textContent = n;
      b.setAttribute("aria-pressed", String(n === nodeFilter));
      nodeSeg.appendChild(b);
    });
    all.setAttribute("aria-pressed", String(nodeFilter === ""));
    const series = nodeSeries(d);
    chart("cpu", series, nodes);
    chart("mem", series, nodes);
    const rows = d.rows || [];
    const ord = (g) => g === "control" ? 0 : g === "infra" ? 1 : g === "app" ? 2 : g === "db" ? 3 : 9;
    let cols = "";
    (d.nodes || []).forEach((n) => {
      if (nodeFilter && n.node !== nodeFilter) return;
      const memPct = n.mem_total ? Math.round(n.mem_used * 100 / n.mem_total) : 0;
      const warn = memPct >= 85;
      const st = n.stale ? '<span class="state state--idle"><svg class="sig sig--idle" aria-hidden="true"><use href="#s-idle"></use></svg>' + esc(t.i18nStale) + "</span>"
        : warn ? '<span class="state state--warn"><svg class="sig sig--warn" aria-hidden="true"><use href="#s-warn"></use></svg>' + esc((t.i18nPressure || "%d").replace("%d", memPct).replace("%%", "%")) + "</span>"
        : '<span class="state state--ok"><svg class="sig sig--ok" aria-hidden="true"><use href="#s-ok"></use></svg>' + esc(t.i18nHealthy) + "</span>";
      const comps = rows.filter((r) => r.node === n.node).sort((a, b) => ord(a.group) - ord(b.group) || (a.name || "").localeCompare(b.name || ""));
      const cpuPts = (series[n.node] || {}).cpu || [];
      cols += '<section class="nodecol' + (warn ? " nodecol--warn" : "") + '" aria-label="' + esc(n.node) + '">' +
        '<div class="nodecol-head"><h2>' + esc(n.node) + "</h2>" + st + "</div>" +
        '<div class="figs"><div class="fig"><b>' + Math.round(n.cpu_pct || 0) + "%</b><span>" + esc((t.i18nCores || "%d").replace("%d", n.ncpu || 0)) + "</span>" + spark(cpuPts) + "</div>" +
        '<div class="fig"><b>' + gib(n.mem_used || 0) + "<small> " + esc(t.i18nOf) + " " + gib(n.mem_total || 0) + "</small></b><span>" + esc(t.i18nMemory) + "</span>" + meter(memPct, warn) + "</div></div>" +
        '<h3 class="list-head">' + esc((t.i18nContainers || "%d").replace("%d", comps.length)) + "</h3><ul class=\"comps\">" +
        comps.map((r) => {
          const name = r.href ? '<a href="' + esc(r.href) + '">' + esc(r.name) + "</a>" : "<span>" + esc(r.name) + "</span>";
          const lim = r.mem_limit && r.mem_limit > 0 ? meter(Math.round(r.mem * 100 / r.mem_limit), r.mem * 100 / r.mem_limit >= 85) + mib(r.mem) + " / " + mib(r.mem_limit) : mib(r.mem);
          return '<li><span class="comp-name">' + name + "<em>" + esc(r.detail || r.group || "") + '</em></span><span class="comp-cpu">' + (r.cpu || 0).toFixed(1) + '%</span><span class="comp-mem">' + lim + "</span></li>";
        }).join("") + "</ul></section>";
    });
    el.querySelector(".nodecols").innerHTML = cols;
  }
  function load() {
    fetch(base + "?range=" + range).then((r) => { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(build).catch(() => { status.textContent = t.i18nError || "metrics unavailable"; });
  }
  load();
  if (window.__krillMonTimer) clearInterval(window.__krillMonTimer);
  window.__krillMonTimer = setInterval(load, 10000);
}

// krillEnhance wires up content present on the page or just swapped in: it mounts
// any unmounted log terminals and initializes the env editor. Idempotent.
window.krillEnhance = function () {
  krillGuardForms();
  document.querySelectorAll(".term[data-term-log]:not([data-mounted])").forEach(mountTerminalEl);
  document.querySelectorAll(".term[data-term-ws]:not([data-mounted])").forEach(mountExecTerminal);
  document.querySelectorAll("[data-logs]:not([data-mounted])").forEach(mountLogViewer);
  document.querySelectorAll(".mon:not([data-mounted])").forEach(mountMonitoring);
  const envForm = document.getElementById("env-form");
  if (envForm && !envForm.envLines) {
    let mode = "kv";
    try { mode = localStorage.getItem("krillEnvMode") || "kv"; } catch (_) { /* private mode */ }
    krillEnvMode(mode);
  }
  krillMountDBLink();
  // Stop a monitoring poll timer from a previous page (cleared on every nav/swap;
  // re-armed by mountMonitoring only when the monitoring page is present).
  if (!document.querySelector(".mon")) {
    if (window.__krillMonTimer) { clearInterval(window.__krillMonTimer); window.__krillMonTimer = null; }
  }
};

// Engine-aware Scheme + Field selects for the DB-link form: rebuilds the scheme
// options to only those valid for the currently selected database (Postgres ->
// postgresql/postgres, Redis/DragonFly -> redis) and reveals a hint explaining
// that postgres:// and postgresql:// are interchangeable aliases (the choice is
// about the consuming framework, not the database); rebuilds the Field options
// to only those the selected engine's driver exposes (e.g. MinIO ->
// endpoint/access_key/secret_key/region, Postgres adds user/dbname). Idempotent
// (guarded by data-mounted).
function krillMountDBLink() {
  const dbSel = document.getElementById("dblink-db");
  const schemeSel = document.getElementById("dblink-scheme");
  if (!dbSel || !schemeSel || schemeSel.dataset.mounted) return;
  schemeSel.dataset.mounted = "1";
  const hint = document.getElementById("dblink-scheme-hint");
  const fieldSel = document.getElementById("dblink-field");
  const schemeWrap = document.getElementById("dblink-scheme-wrap");
  const allSchemes = Array.from(schemeSel.options).map((o) => ({ value: o.value, label: o.textContent, engine: o.dataset.engine }));
  const allFields = fieldSel
    ? Array.from(fieldSel.options).map((o) => ({ value: o.value, label: o.textContent, engines: (o.dataset.engines || "").split(" ") }))
    : [];
  const engineOf = () => {
    const opt = dbSel.selectedOptions && dbSel.selectedOptions[0];
    return (opt && opt.dataset.engine) || dbSel.value.split(":")[0] || "";
  };
  const apply = () => {
    const engine = engineOf();
    const prevScheme = schemeSel.value;
    schemeSel.innerHTML = "";
    allSchemes.filter((o) => o.engine === engine).forEach((o) => {
      const opt = document.createElement("option");
      opt.value = o.value;
      opt.textContent = o.label;
      schemeSel.appendChild(opt);
    });
    if (Array.from(schemeSel.options).some((o) => o.value === prevScheme)) schemeSel.value = prevScheme;
    if (fieldSel) {
      const prevField = fieldSel.value;
      fieldSel.innerHTML = "";
      allFields.filter((o) => o.engines.includes(engine)).forEach((o) => {
        const opt = document.createElement("option");
        opt.value = o.value;
        opt.textContent = o.label;
        fieldSel.appendChild(opt);
      });
      // Falls back to the first visible option when the prior field isn't valid for this engine.
      if (Array.from(fieldSel.options).some((o) => o.value === prevField)) fieldSel.value = prevField;
    }
    // Scheme and its hint only apply to the "url" field of a postgres DB.
    const isUrl = !fieldSel || fieldSel.value === "url";
    if (schemeWrap) schemeWrap.style.display = isUrl ? "" : "none";
    if (hint) hint.style.display = (isUrl && engine === "pg") ? "" : "none";
  };
  dbSel.addEventListener("change", apply);
  if (fieldSel) fieldSel.addEventListener("change", apply);
  apply();
}

// The confirmation dialog (#confirm, in the page frame). Used from a form's
// onsubmit: `return krillConfirm(this, 'Delete the app api?')`, optionally
// with the confirm button's verb, its kind ("primary" for a non-destructive
// action) and a second line saying what is lost; the form's data-ok, data-kind
// and data-more attributes supply them when the arguments are left out.
// Returns false to block the triggering submit and submits the form natively
// once confirmed. The form must carry hx-boost="false" so htmx does not fire
// its own request on the submit event.
window.krillConfirm = function (form, message, confirmLabel, kind, more) {
  const dlg = document.getElementById("confirm");
  if (!dlg || typeof dlg.showModal !== "function") return confirm(message);
  const d = (form && form.dataset) || {};
  dlg.querySelector(".confirm-q").textContent = message || d.confirm || "";
  dlg.querySelector(".confirm-more").textContent = more || d.more || "";
  const okEl = dlg.querySelector("#confirm-ok");
  okEl.textContent = confirmLabel || d.ok || d.label || okEl.dataset.default || okEl.textContent;
  if (!okEl.dataset.default) okEl.dataset.default = okEl.textContent;
  okEl.className = (kind || d.kind) === "primary" ? "btn btn--primary" : "btn btn--danger";
  window.__krillConfirmAction = function () {
    // form.submit() fires no submit event, so the double-submit guard below
    // would not see it: mark the form busy here, and refuse a second run.
    if (krillFormBusy(form)) form.submit();
  };
  dlg.returnValue = "";
  dlg.showModal();
  okEl.focus();
  return false;
};
// The dialog's own form (method="dialog") closes it with the button's value:
// run the pending action on "ok" only. 'close' does not bubble, so listen in
// the capture phase.
if (!window.__krillConfirmInit) {
  window.__krillConfirmInit = true;
  document.addEventListener("close", function (e) {
    if (!e.target || e.target.id !== "confirm" || e.target.open) return;
    const action = window.__krillConfirmAction;
    window.__krillConfirmAction = null;
    if (e.target.returnValue === "ok" && action) action();
  }, true);
}

// ── Unsaved changes in a sibling form ───────────────────────────────────────
// A card such as a domain holds several small forms side by side (exposure
// and paths with their own Save, a TLS toggle, primary, delete). Submitting
// one reloads the page and silently drops what was changed but not saved in
// another — a ticked "Open to the internet" was lost this way by pressing
// "Toggle HTTPS". Within a [data-unsaved-scope], submitting one form while
// another has unsaved changes asks first.
function krillFormDirty(form) {
  for (const el of form.elements) {
    if (!el.name || el.disabled) continue;
    const t = (el.type || "").toLowerCase();
    if (t === "hidden" || t === "submit" || t === "button" || t === "reset" || t === "file") continue;
    if (t === "checkbox" || t === "radio") {
      if (el.checked !== el.defaultChecked) return true;
    } else if (el.tagName === "SELECT") {
      for (const o of el.options) if (o.selected !== o.defaultSelected) return true;
    } else if (el.value !== el.defaultValue) {
      return true;
    }
  }
  return false;
}
if (!window.__krillUnsavedInit) {
  window.__krillUnsavedInit = true;
  // Capture phase: runs before htmx and before a form's own onsubmit.
  document.addEventListener("submit", function (e) {
    const form = e.target;
    if (!(form instanceof HTMLFormElement) || form.__krillUnsavedOK) {
      if (form) form.__krillUnsavedOK = false;
      return;
    }
    const scope = form.closest("[data-unsaved-scope]");
    if (!scope) return;
    const dirty = Array.from(scope.querySelectorAll("form")).some(function (f) { return f !== form && krillFormDirty(f); });
    if (!dirty) return;
    e.preventDefault();
    e.stopImmediatePropagation();
    const submitter = e.submitter || null;
    const msg = scope.dataset.unsavedMsg || "Unsaved changes will be lost. Continue?";
    // Re-dispatch through the normal path (htmx, the form's own confirm),
    // skipping this check once.
    const resubmit = function () {
      form.__krillUnsavedOK = true;
      form.requestSubmit(submitter);
    };
    const dlg = document.getElementById("confirm");
    if (!dlg || typeof dlg.showModal !== "function") {
      if (confirm(msg)) resubmit();
      return;
    }
    krillConfirm(form, msg, scope.dataset.unsavedLabel || "Continue", "primary");
    window.__krillConfirmAction = resubmit;
  }, true);
}

// ── Double-submit guard ─────────────────────────────────────────────────────
// A slow POST (closing the panel's direct port, a lockdown, a deploy) used to
// leave its button live, and a second click sent the request again. Boosted
// forms get htmx's own guards, set per form by krillGuardForms: hx-sync drops a
// submit while the form's request is in flight (htmx's default would queue it
// and send it afterwards), and hx-disabled-elt disables the form's submit
// buttons — including ones outside it, tied by form= — until the response,
// error or not. Forms submitted natively (hx-boost="false", and the confirm
// dialog's form.submit()) navigate away, so they are marked busy once and
// stay that way; a page restored from the back/forward cache is reset.

// The form's submit buttons, wherever they sit in the document.
function krillSubmitButtons(form) {
  return Array.prototype.filter.call(form.elements, function (el) {
    return (el.tagName === "BUTTON" && el.type === "submit") || (el.tagName === "INPUT" && el.type === "submit");
  });
}

// Marks a natively submitted form busy and disables its submit buttons.
// Returns false when it was already busy: the caller must not submit again.
window.krillFormBusy = function (form) {
  if (form.hasAttribute("data-k-busy")) return false;
  form.setAttribute("data-k-busy", "");
  // Disabled after the event, not during it: a disabled submitter is left out
  // of the form data, and its name/value may be what the server reads.
  setTimeout(function () {
    krillSubmitButtons(form).forEach(function (b) {
      if (b.disabled) return;
      b.disabled = true;
      b.setAttribute("data-k-busy-btn", "");
    });
  }, 0);
  return true;
};

var krillFormSeq = 0;
function krillGuardForms() {
  document.querySelectorAll("form").forEach(function (form) {
    if (form.hasAttribute("data-k-guarded")) return;
    form.setAttribute("data-k-guarded", "");
    if (form.closest('[hx-boost="false"]')) return; // native: see the submit listener
    if (!form.hasAttribute("hx-sync")) form.setAttribute("hx-sync", "this:drop");
    // A form with no submit button has nothing to disable, and htmx would
    // log an error for a selector list that matches nothing.
    if (!form.hasAttribute("hx-disabled-elt") && krillSubmitButtons(form).length > 0) {
      // A plain selector: htmx's "find" would only reach the first button and
      // none of the ones tied to the form from outside it.
      // htmx splits the list on top-level commas, so none may sit inside a
      // selector (no :is(a, b)).
      if (!form.id) form.id = "k-form-" + (++krillFormSeq);
      const id = form.id;
      form.setAttribute("hx-disabled-elt", [
        "#" + id + ' button:not([type="button"]):not([type="reset"])',
        "#" + id + ' input[type="submit"]',
        'button[form="' + id + '"]:not([type="button"]):not([type="reset"])',
        'input[type="submit"][form="' + id + '"]',
      ].join(", "));
    }
  });
}

if (!window.__krillSubmitGuardInit) {
  window.__krillSubmitGuardInit = true;
  // Bubble phase: htmx (boosted forms) and krillConfirm (dialog forms) have
  // already called preventDefault on the submits they take over.
  document.addEventListener("submit", function (e) {
    if (e.defaultPrevented) return;
    if (!krillFormBusy(e.target)) e.preventDefault();
  });
  window.addEventListener("pageshow", function (e) {
    if (!e.persisted) return;
    krillApplyEnvReveal();
    document.querySelectorAll("form[data-k-busy]").forEach(function (f) { f.removeAttribute("data-k-busy"); });
    document.querySelectorAll("[data-k-busy-btn]").forEach(function (b) {
      b.disabled = false;
      b.removeAttribute("data-k-busy-btn");
    });
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

// Show a transient toast. kind: "ok" | "err" | "warn" | "idle".
window.krillShowToast = function (msg, kind) {
  const host = document.getElementById("toasts");
  if (!host) return;
  const k = ["ok", "err", "warn", "idle"].indexOf(kind) >= 0 ? kind : "ok";
  const el = document.createElement("div");
  el.className = "toast toast--" + k;
  el.setAttribute("role", "status");
  el.innerHTML = '<svg class="sig sig--' + k + '" aria-hidden="true"><use href="#s-' + k + '"></use></svg><span></span>';
  el.querySelector("span").textContent = msg;
  host.appendChild(el);
  krillToastLater(el);
};
function krillToastLater(el) {
  el.addEventListener("click", function () { krillDismissToast(el); });
  setTimeout(function () { krillDismissToast(el); }, el.classList.contains("toast--err") ? 8000 : 4000);
}
window.krillDismissToast = function (el) {
  if (!el || el.dataset.gone) return;
  el.dataset.gone = "1";
  if (el.parentNode) el.parentNode.removeChild(el);
};

// Toggle masking of a single input (e.g. a connection string) between password
// and text; flips the eye icon on the button that triggered it.
window.krillReveal = function (btn) {
  const inp = btn.dataset.reveal ? document.getElementById(btn.dataset.reveal) : btn.parentElement.querySelector("input");
  if (!inp) return;
  inp.type = inp.type === "password" ? "text" : "password";
  btn.setAttribute("aria-pressed", inp.type === "text" ? "true" : "false");
};

// Environment editor state belongs to this form, never to the previous app.
// Keep original lines (including comments and whitespace) until actually edited.
function envLineKey(line) {
  if (!line.trim() || line.trimStart().startsWith("#")) return "";
  const i = line.indexOf("=");
  return i < 0 ? "" : line.slice(0, i).trim();
}
function envEditor() {
  const form = document.getElementById("env-form");
  if (!form) return null;
  if (!form.envLines) {
    const ordinary = new Set(document.getElementById("env-public-keys").value.split("\n"));
    form.envLines = document.getElementById("env-textarea").value.split("\n").map(function (text) {
      const key = envLineKey(text);
      return { text: text, ordinary: !!key && ordinary.has(key), draft: /^\s*=/.test(text) };
    });
  }
  return form;
}
window.krillEnvSyncToTextarea = function () {
  const form = envEditor();
  if (!form) return;
  document.getElementById("env-textarea").value = form.envLines.map(l => l.text).join("\n");
  document.getElementById("env-public-keys").value = form.envLines
    .filter(l => l.ordinary && envLineKey(l.text)).map(l => envLineKey(l.text)).join("\n");
};

// A mode switch or navigation resets ALL reveals. Raw secrets are hidden with
// the native hidden attribute, rather than browser-specific text masking CSS.
window.krillApplyEnvReveal = function () {
  const form = document.getElementById("env-form");
  if (!form) return;
  // HTMX serializes innerHTML for history: keep textarea text in step with
  // the live value and the hidden classification input's reflected attribute.
  const source = document.getElementById("env-textarea");
  source.defaultValue = source.value;
  krillEnvReveal(form, false);
};
// krillEnvReveal shows or hides every secret value, in both editors.
function krillEnvReveal(form, on) {
  form.dataset.revealed = on ? "1" : "";
  form.querySelectorAll("#env-secret-rows .env-val").forEach(i => { i.type = on ? "text" : "password"; });
  document.getElementById("env-secrets-raw").hidden = !on;
  document.getElementById("env-secrets-hidden").hidden = on;
  const button = document.getElementById("env-raw-reveal");
  button.setAttribute("aria-pressed", String(on));
  button.querySelector("span").textContent = on ? form.dataset.hide : form.dataset.show;
}
window.krillEnvRevealToggle = function () {
  const form = envEditor();
  if (!form) return;
  krillEnvReveal(form, form.dataset.revealed !== "1");
};
window.krillEnvAddRow = function (ordinary) {
  const form = envEditor();
  if (!form) return;
  const line = { text: "", ordinary: ordinary, draft: true };
  form.envLines.push(line);
  const row = envRenderRow(form, line);
  row.querySelector(".env-key").focus();
};
function envIcon(id) {
  return '<svg class="ico" aria-hidden="true"><use href="#' + id + '"></use></svg>';
}
function envRenderRow(form, line) {
  const row = document.createElement("li");
  row.className = "kv env-row";
  const k = document.createElement("input");
  k.className = "input input--mono env-key"; k.placeholder = form.dataset.key;
  k.setAttribute("aria-label", form.dataset.key);
  k.value = envLineKey(line.text);
  const v = document.createElement("input");
  v.className = "input input--mono env-val"; v.placeholder = form.dataset.value;
  v.setAttribute("aria-label", form.dataset.value);
  v.value = line.text.includes("=") ? line.text.slice(line.text.indexOf("=") + 1) : "";
  v.type = line.ordinary || form.dataset.revealed === "1" ? "text" : "password";
  k.autocomplete = "off"; v.autocomplete = "off";
  k.spellcheck = false; v.spellcheck = false;
  const move = document.createElement("button");
  move.type = "button"; move.className = "pill pill--icon env-move";
  move.innerHTML = envIcon(line.ordinary ? "i-lock" : "i-unlock");
  const moveLabel = line.ordinary ? form.dataset.toSecret : form.dataset.toVariable;
  move.setAttribute("aria-label", moveLabel); move.title = moveLabel;
  move.addEventListener("click", function () {
    line.ordinary = !line.ordinary;
    row.remove();
    const replacement = envRenderRow(form, line);
    krillEnvSyncToTextarea();
    replacement.querySelector(".env-move").focus();
  });
  const del = document.createElement("button");
  del.type = "button"; del.className = "pill pill--icon"; del.innerHTML = envIcon("i-trash");
  del.setAttribute("aria-label", form.dataset.remove); del.title = form.dataset.remove;
  del.addEventListener("click", function () {
    form.envLines.splice(form.envLines.indexOf(line), 1);
    row.remove();
    krillEnvSyncToTextarea();
  });
  function edit() {
    const key = k.value.trim();
    // Keep spacing before '=' if only the value changed.
    const prefix = key === envLineKey(line.text) && line.text.includes("=")
      ? line.text.slice(0, line.text.indexOf("=") + 1) : key + "=";
    line.text = prefix + v.value;
    line.draft = !key;
    krillEnvSyncToTextarea();
  }
  k.addEventListener("input", edit); v.addEventListener("input", edit);
  row.append(k, v, move, del);
  document.getElementById(line.ordinary ? "env-variable-rows" : "env-secret-rows").appendChild(row);
  return row;
}
window.krillEnvBuildRows = function () {
  const form = envEditor();
  if (!form) return;
  document.getElementById("env-variable-rows").replaceChildren();
  document.getElementById("env-secret-rows").replaceChildren();
  form.envLines.forEach(function (line) {
    if (envLineKey(line.text) || line.draft) envRenderRow(form, line);
  });
};
window.krillEnvRawInput = function (ordinary) {
  const form = envEditor();
  if (!form) return;
  const text = document.getElementById(ordinary ? "env-variables-raw" : "env-secrets-raw").value;
  const lines = text === "" ? [] : text.split("\n").map(text => ({ text: text, ordinary: ordinary, draft: /^\s*=/.test(text) }));
  // Replace this section's slots in place. Unedited lines in the other section
  // keep their position; merely switching modes never rebuilds the raw source.
  let index = 0;
  const next = [];
  form.envLines.forEach(function (line) {
    if (line.ordinary !== ordinary) next.push(line);
    else if (index < lines.length) next.push(lines[index++]);
  });
  form.envLines = next.concat(lines.slice(index));
  krillEnvSyncToTextarea();
};
window.krillEnvMode = function (mode) {
  const form = envEditor();
  if (!form) return;
  mode = mode === "raw" ? "raw" : "kv";
  krillEnvBuildRows();
  document.getElementById("env-variables-raw").value = form.envLines.filter(l => l.ordinary).map(l => l.text).join("\n");
  document.getElementById("env-secrets-raw").value = form.envLines.filter(l => !l.ordinary).map(l => l.text).join("\n");
  document.getElementById("env-raw").style.display = mode === "raw" ? "" : "none";
  document.getElementById("env-kv").style.display = mode === "kv" ? "" : "none";
  form.dataset.mode = mode;
  try { localStorage.setItem("krillEnvMode", mode); } catch (_) { /* private mode */ }
  document.querySelectorAll("#env-seg [data-env-mode]").forEach(function (b) {
    b.setAttribute("aria-pressed", String(b.dataset.envMode === mode));
  });
  krillApplyEnvReveal();
};

// Run enhancements on first load and after every hx-boost swap. Guarded so a
// re-executed app.js does not stack duplicate listeners.
if (!window.__krillEnhanceInit) {
  window.__krillEnhanceInit = true;
  document.addEventListener("DOMContentLoaded", window.krillEnhance);
  document.addEventListener("htmx:afterSettle", window.krillEnhance);
  document.addEventListener("htmx:beforeHistorySave", window.krillApplyEnvReveal);
  document.addEventListener("htmx:historyRestore", window.krillEnhance);
}
window.krillEnhance();

// ── The page frame: theme, bar menu, pickers, copy, reveal, conditional fields ──
// Delegated on document, so boosted swaps need no re-binding. Guarded so a
// re-executed app.js does not stack listeners.
if (!window.__krillShellInit) {
  window.__krillShellInit = true;
  const root = document.documentElement;

  document.addEventListener("click", function (e) {
    const t = e.target;
    // Theme: remember the explicit choice.
    const mode = t.closest && t.closest("#mode");
    if (mode) {
      root.dataset.mode = root.dataset.mode === "dark" ? "light" : "dark";
      try { localStorage.setItem("krill-mode", root.dataset.mode); } catch (_) { /* private mode */ }
      krillModeLabel();
      return;
    }
    // The bar's menu on narrow screens.
    const menu = t.closest && t.closest(".pill--menu");
    if (menu) {
      const open = document.querySelector(".bar").classList.toggle("is-open");
      menu.setAttribute("aria-expanded", String(open));
      return;
    }
    const copy = t.closest && t.closest("[data-copy]");
    if (copy) {
      const src = copy.dataset.copyFrom && document.getElementById(copy.dataset.copyFrom);
      krillCopyToClip(src ? src.value : copy.dataset.copy, copy.dataset.copyLabel || copy.getAttribute("aria-label") || "");
      return;
    }
    const reveal = t.closest && t.closest("[data-reveal]");
    if (reveal) { krillReveal(reveal); return; }
    const open = t.closest && t.closest("[data-palette-open]");
    if (open) { krillPalette(true); return; }
    if (t.id === "palette-scrim") { krillPalette(false); return; }
    // An open picker closes on a click outside it.
    document.querySelectorAll("details.envpick[open]").forEach(function (d) {
      if (!d.contains(t)) d.open = false;
    });
  });

  document.addEventListener("keydown", function (e) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k" && document.getElementById("palette")) {
      e.preventDefault();
      krillPalette(document.getElementById("palette").hidden);
      return;
    }
    if (e.key !== "Escape") return;
    const pal = document.getElementById("palette");
    if (pal && !pal.hidden) { krillPalette(false); return; }
    const picker = document.querySelector("details.envpick[open]");
    if (picker) { picker.open = false; picker.querySelector("summary").focus(); return; }
    const bar = document.querySelector(".bar.is-open");
    if (bar) { bar.classList.remove("is-open"); bar.querySelector(".pill--menu").setAttribute("aria-expanded", "false"); return; }
    // A sheet over the canvas closes with Escape unless focus is in a field.
    const close = document.querySelector(".sheet [data-sheet-close]");
    const typing = e.target.closest && e.target.closest("input, textarea, select, [contenteditable], .xterm");
    if (close && !typing && !document.querySelector("dialog[open]")) close.click();
  });

  // Fields shown only for one value of a radio group or select: data-when="name=value".
  document.addEventListener("change", function (e) {
    const form = e.target.form;
    if (form) krillSyncWhen(form);
  });
}

function krillModeLabel() {
  const b = document.getElementById("mode");
  if (!b) return;
  b.textContent = document.documentElement.dataset.mode === "dark" ? b.dataset.light : b.dataset.dark;
}

function krillSyncWhen(form) {
  form.querySelectorAll("[data-when]").forEach(function (g) {
    const parts = g.dataset.when.split("=");
    const el = form.querySelector('[name="' + parts[0] + '"]:checked') || form.querySelector('select[name="' + parts[0] + '"]');
    const values = (parts[1] || "").split("|");
    g.hidden = !el || values.indexOf(el.value) < 0;
  });
}

// The command palette: an overlay whose results come from the search route.
window.krillPalette = function (open) {
  const pal = document.getElementById("palette");
  const scrim = document.getElementById("palette-scrim");
  if (!pal) return;
  pal.hidden = !open;
  if (scrim) scrim.hidden = !open;
  document.body.classList.toggle("has-palette", open);
  const input = pal.querySelector("input");
  if (open) {
    input.value = "";
    input.focus();
    if (window.htmx) htmx.trigger(input, "palette-open");
  } else if (window.__krillPaletteFrom) {
    try { window.__krillPaletteFrom.focus(); } catch (_) { /* gone */ }
  }
  window.__krillPaletteFrom = open ? document.activeElement : null;
};
// Arrow keys move through the results, Enter opens the selected one.
document.addEventListener("keydown", function (e) {
  const pal = document.getElementById("palette");
  if (!pal || pal.hidden || (e.key !== "ArrowDown" && e.key !== "ArrowUp" && e.key !== "Enter")) return;
  const items = Array.from(pal.querySelectorAll(".palette-groups a, .palette-groups .palette-item"));
  if (!items.length) return;
  let i = items.findIndex(function (a) { return a.getAttribute("aria-selected") === "true"; });
  if (e.key === "Enter") {
    e.preventDefault();
    const a = items[i < 0 ? 0 : i];
    krillPalette(false);
    a.click();
    return;
  }
  e.preventDefault();
  i = e.key === "ArrowDown" ? Math.min(items.length - 1, i + 1) : Math.max(0, i - 1);
  items.forEach(function (a, j) { a.setAttribute("aria-selected", String(j === i)); });
  items[i].scrollIntoView({ block: "nearest" });
});
document.addEventListener("htmx:afterSwap", function (e) {
  if (e.target && e.target.id === "palette-results") {
    const first = e.target.querySelector("a, .palette-item");
    if (first) first.setAttribute("aria-selected", "true");
  }
});

// Per page: the theme button's label, server toasts' timers, conditional fields.
function krillShellEnhance() {
  krillModeLabel();
  document.querySelectorAll("[data-toast-auto]").forEach(function (el) {
    el.removeAttribute("data-toast-auto");
    krillToastLater(el);
  });
  document.querySelectorAll("form").forEach(function (f) { if (f.querySelector("[data-when]")) krillSyncWhen(f); });
  const pal = document.getElementById("palette");
  if (pal && pal.hidden) document.body.classList.remove("has-palette");
}
document.addEventListener("DOMContentLoaded", krillShellEnhance);
document.addEventListener("htmx:afterSettle", krillShellEnhance);
krillShellEnhance();

// ── Wired maps: the environment canvas, the nodes lanes and the topology ──
// Cards carry data-to (and data-detected) with the element ids they link to;
// the map draws a curve per link into its .wires svg. Pointing at a card
// lights it and its links; a node chip (data-node) lights every card placed
// on that node and pins the filter when clicked. The canvas also zooms
// (buttons, ctrl+wheel) and pans (drag on the background).
function mountWiredMap(map) {
  map.setAttribute("data-mounted", "");
  const svg = map.querySelector(".wires");
  const cards = Array.from(map.querySelectorAll(".svc"));
  const links = [];
  cards.forEach(function (c) {
    (c.dataset.to || "").split(" ").filter(Boolean).forEach(function (id) {
      const to = document.getElementById(id);
      if (to && map.contains(to)) links.push({ from: c, to: to });
    });
    (c.dataset.detected || "").split(" ").filter(Boolean).forEach(function (id) {
      const to = document.getElementById(id);
      if (!to) return;
      links.forEach(function (l) { if (l.from === c && l.to === to) l.detected = true; });
    });
  });
  const draw = function () {
    if (!svg) return;
    svg.replaceChildren();
    if (getComputedStyle(svg).display === "none") return;
    const host = map.getBoundingClientRect();
    const k = host.width / map.offsetWidth || 1;
    links.forEach(function (l) {
      l.path = null;
      if (!l.from.offsetParent || !l.to.offsetParent || l.from.hidden || l.to.hidden) return;
      const a = l.from.getBoundingClientRect(), b = l.to.getBoundingClientRect();
      const dir = b.left >= a.right ? 1 : b.right <= a.left ? -1 : 0;
      const ya = (a.top + a.height / 2 - host.top) / k, yb = (b.top + b.height / 2 - host.top) / k;
      const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
      if (dir === 0) {
        const x = (Math.max(a.right, b.right) - host.left) / k;
        const xa = (a.right - host.left) / k, xb = (b.right - host.left) / k;
        p.setAttribute("d", "M" + xa + " " + ya + "C" + (x + 48) + " " + ya + " " + (x + 48) + " " + yb + " " + xb + " " + yb);
      } else {
        const x1 = ((dir > 0 ? a.right : a.left) - host.left) / k;
        const x2 = ((dir > 0 ? b.left : b.right) - host.left) / k;
        const dx = Math.max(40, Math.abs(x2 - x1) / 2) * dir;
        p.setAttribute("d", "M" + x1 + " " + ya + "C" + (x1 + dx) + " " + ya + " " + (x2 - dx) + " " + yb + " " + x2 + " " + yb);
      }
      if (l.from.hasAttribute("data-gateway")) p.classList.add("w-ingress");
      else if (l.detected) p.classList.add("w-detected");
      else if (l.from.closest(".lane") && l.from.closest(".lane") !== l.to.closest(".lane")) p.classList.add("w-cross");
      else p.classList.add("w-link");
      l.path = p;
      svg.append(p);
    });
  };
  const scope = map.closest("main") || document;
  const chips = Array.from(scope.querySelectorAll(".chip[data-node]"));
  let pinned = null;
  const light = function (hovered) {
    const node = hovered && hovered.dataset.node ? hovered.dataset.node : (!hovered && pinned) || null;
    chips.forEach(function (c) {
      c.classList.toggle("is-lit", c.dataset.node === node);
      c.setAttribute("aria-pressed", String(c.dataset.node === pinned));
    });
    if (node) {
      map.classList.add("is-focus");
      links.forEach(function (l) { if (l.path) l.path.classList.remove("is-lit"); });
      cards.forEach(function (c) { c.classList.toggle("is-lit", (c.dataset.nodes || "").split(" ").indexOf(node) >= 0); });
      return;
    }
    const card = hovered && hovered.classList.contains("svc") ? hovered : map.querySelector('.svc[aria-current="true"]');
    map.classList.toggle("is-focus", !!card);
    cards.forEach(function (c) { c.classList.remove("is-lit"); });
    links.forEach(function (l) {
      const hit = card && (l.from === card || l.to === card);
      if (l.path) l.path.classList.toggle("is-lit", !!hit);
      if (hit) { l.from.classList.add("is-lit"); l.to.classList.add("is-lit"); }
    });
    if (card) card.classList.add("is-lit");
  };
  cards.forEach(function (c) {
    c.addEventListener("pointerenter", function () { light(c); });
    c.addEventListener("pointerleave", function () { light(null); });
    c.addEventListener("focus", function () { light(c); });
    c.addEventListener("blur", function () { light(null); });
  });
  chips.forEach(function (c) {
    c.addEventListener("pointerenter", function () { light(c); });
    c.addEventListener("pointerleave", function () { light(null); });
    c.addEventListener("click", function () { pinned = pinned === c.dataset.node ? null : c.dataset.node; light(null); });
  });
  map.__krillRedraw = function () { draw(); light(null); };

  // Zoom and pan: the canvas only.
  const canvas = map.closest(".canvas");
  if (canvas && map.classList.contains("plane")) {
    const label = canvas.querySelector("[data-zoom-label]");
    let zoom = 1, px = 0, py = 0;
    const apply = function () {
      map.style.setProperty("--zoom", String(zoom));
      map.style.setProperty("--pan-x", px + "px");
      map.style.setProperty("--pan-y", py + "px");
      if (label) label.textContent = Math.round(zoom * 100) + "%";
      requestAnimationFrame(map.__krillRedraw);
    };
    const fit = function () {
      if (getComputedStyle(svg).display === "none") return;
      map.setAttribute("data-zoomed", "");
      const sheet = document.querySelector(".sheet");
      const cw = canvas.clientWidth - (sheet ? sheet.offsetWidth + 24 : 0);
      const top = 84, ch = canvas.clientHeight - top - 24;
      zoom = Math.max(0.3, Math.min(1, (cw - 48) / map.offsetWidth, ch / map.offsetHeight));
      px = Math.max(24, (cw - map.offsetWidth * zoom) / 2);
      py = top;
      apply();
    };
    map.__krillFit = fit;
    canvas.querySelectorAll("[data-zoom]").forEach(function (b) {
      b.addEventListener("click", function () {
        const z = b.dataset.zoom;
        if (z === "fit") { fit(); return; }
        const before = zoom;
        zoom = Math.max(0.3, Math.min(2, Math.round((zoom + (z === "in" ? 0.1 : -0.1)) * 10) / 10));
        const cx = canvas.clientWidth / 2, cy = canvas.clientHeight / 2;
        px = cx - (cx - px) * zoom / before;
        py = cy - (cy - py) * zoom / before;
        apply();
      });
    });
    canvas.addEventListener("wheel", function (e) {
      if (!map.hasAttribute("data-zoomed")) return;
      e.preventDefault();
      if (e.ctrlKey || e.metaKey) {
        const before = zoom;
        zoom = Math.max(0.3, Math.min(2, zoom * (e.deltaY < 0 ? 1.08 : 1 / 1.08)));
        const r = canvas.getBoundingClientRect();
        const mx = e.clientX - r.left, my = e.clientY - r.top;
        px = mx - (mx - px) * zoom / before;
        py = my - (my - py) * zoom / before;
      } else {
        px -= e.deltaX;
        py -= e.deltaY;
      }
      apply();
    }, { passive: false });
    let drag = null;
    canvas.addEventListener("pointerdown", function (e) {
      if (!map.hasAttribute("data-zoomed") || e.button !== 0 || e.target.closest(".svc, .float, a, button")) return;
      drag = { x: e.clientX, y: e.clientY, px: px, py: py, id: e.pointerId };
    });
    canvas.addEventListener("pointermove", function (e) {
      if (!drag || e.pointerId !== drag.id) return;
      if (!canvas.classList.contains("is-panning") && Math.abs(e.clientX - drag.x) + Math.abs(e.clientY - drag.y) < 4) return;
      canvas.classList.add("is-panning");
      px = drag.px + e.clientX - drag.x;
      py = drag.py + e.clientY - drag.y;
      apply();
    });
    const end = function () { drag = null; canvas.classList.remove("is-panning"); };
    canvas.addEventListener("pointerup", end);
    canvas.addEventListener("pointercancel", end);
    fit();
  }
  map.__krillRedraw();
  if (document.fonts) document.fonts.ready.then(function () { if (map.isConnected) map.__krillRedraw(); });
}
function krillMountMaps() {
  document.querySelectorAll(".plane:not([data-mounted]), .lanemap:not([data-mounted]), .topomap:not([data-mounted])").forEach(mountWiredMap);
}
document.addEventListener("DOMContentLoaded", krillMountMaps);
document.addEventListener("htmx:afterSettle", krillMountMaps);
krillMountMaps();
window.addEventListener("resize", function () {
  document.querySelectorAll(".plane[data-mounted], .lanemap[data-mounted], .topomap[data-mounted]").forEach(function (m) {
    if (m.__krillFit) m.__krillFit(); else if (m.__krillRedraw) m.__krillRedraw();
  });
});
// The new-app sheet names its ghost card as the name is typed.
document.addEventListener("input", function (e) {
  if (!e.target.matches || !e.target.matches("[data-ghost-input]")) return;
  const g = document.querySelector("[data-ghost-name]");
  if (g) g.textContent = e.target.value.trim() || g.dataset.empty || g.textContent;
});

// A button with data-toggle shows or hides the element it names (link forms).
document.addEventListener("click", function (e) {
  const b = e.target.closest && e.target.closest("[data-toggle]");
  if (b) {
    const el = document.getElementById(b.dataset.toggle);
    if (!el) return;
    el.hidden = !el.hidden;
    b.setAttribute("aria-expanded", String(!el.hidden));
    if (!el.hidden) { const f = el.querySelector("input, select, textarea"); if (f) f.focus(); }
    return;
  }
  const u = e.target.closest && e.target.closest("[data-unmask]");
  if (u) {
    const t = document.getElementById(u.dataset.unmask);
    if (!t) return;
    t.classList.toggle("masked");
    u.setAttribute("aria-pressed", String(!t.classList.contains("masked")));
  }
});
// Placement: the node picker only matters when the app is pinned or global.
function krillSyncPlacement() {
  document.querySelectorAll(".nodepick").forEach(function (pick) {
    const form = pick.closest("form");
    const on = form && form.querySelector('input[name="placement_mode"]:checked');
    const ro = pick.closest("fieldset.bare[disabled]");
    if (on && !ro) pick.disabled = on.value === "any";
  });
}
document.addEventListener("change", function (e) { if (e.target.name === "placement_mode") krillSyncPlacement(); });
document.addEventListener("htmx:afterSettle", krillSyncPlacement);
document.addEventListener("DOMContentLoaded", krillSyncPlacement);
krillSyncPlacement();

// Small page behaviours: a select that submits its form on change, the theme
// back to following the system, a member's role save shown only on change,
// and the new-server form's image placeholder following the engine.
document.addEventListener("change", function (e) {
  const t = e.target;
  if (t.matches && t.matches("select[data-autosubmit]") && t.form) { t.form.requestSubmit(); return; }
  if (t.matches && t.matches("select[data-was]")) {
    const b = t.form && t.form.querySelector("[data-role-save]");
    if (b) b.hidden = t.value === t.dataset.was;
    return;
  }
  if (t.name === "engine" && t.dataset.defaultImage !== undefined) {
    const v = t.form && t.form.querySelector('[name="version"]');
    if (v) v.placeholder = t.dataset.defaultImage;
  }
});
document.addEventListener("click", function (e) {
  const b = e.target.closest && e.target.closest("[data-theme-reset]");
  if (!b) return;
  try { localStorage.removeItem("krill-mode"); } catch (_) { /* private mode */ }
  const q = window.matchMedia && matchMedia("(prefers-color-scheme: dark)");
  document.documentElement.dataset.mode = q && q.matches ? "dark" : "light";
  krillModeLabel();
});
