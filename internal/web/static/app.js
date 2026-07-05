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

// Mounts an INTERACTIVE exec terminal into `.k-term[data-term-ws]`: xterm with
// stdin (binary frames), resize (text JSON frames), and binary output. Status
// text is hardcoded English to match the log viewer.
function mountExecTerminal(el) {
  if (!el || el.dataset.mounted) return;
  if (typeof Terminal === "undefined" || typeof FitAddon === "undefined" || !FitAddon.FitAddon) {
    setTimeout(function () { mountExecTerminal(el); }, 50);
    return;
  }
  el.dataset.mounted = "1";
  const term = new Terminal({ fontSize: 12, cursorBlink: true, theme: { background: "#0f1115" } });
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

// Mounts the structured log viewer into `.k-logs[data-ws]`: streams JSON frames
// {t,lvl,msg}, renders time|level|message rows with per-level colors, and does
// client-side text search + level filtering. Status text is hardcoded English.
function mountLogViewer(el) {
  if (!el || el.dataset.mounted) return;
  el.dataset.mounted = "1";
  const MAX_ROWS = 2000;
  const body = el.querySelector(".k-logs-body");
  const searchEl = el.querySelector(".k-logs-search");
  const FILTERABLE = ["debug", "info", "warn", "error"];
  const enabled = { debug: true, info: true, warn: true, error: true };
  let query = "";

  function rowVisible(row) {
    const lvl = row.dataset.level;
    const lvlOK = FILTERABLE.indexOf(lvl) === -1 || enabled[lvl];
    const txtOK = query === "" || row.dataset.text.indexOf(query) !== -1;
    return lvlOK && txtOK;
  }
  function applyFilter() {
    body.querySelectorAll(".k-log-row").forEach((r) => { r.style.display = rowVisible(r) ? "" : "none"; });
  }
  if (searchEl) {
    searchEl.addEventListener("input", () => { query = searchEl.value.toLowerCase(); applyFilter(); });
  }
  el.querySelectorAll(".k-logs-lvlchip").forEach((chip) => {
    chip.addEventListener("click", () => {
      const lvl = chip.dataset.level;
      enabled[lvl] = !enabled[lvl];
      chip.classList.toggle("k-logs-lvlchip-off", !enabled[lvl]);
      applyFilter();
    });
  });

  function fmtTime(t) {
    if (!t) return "";
    const d = new Date(t);
    if (isNaN(d.getTime())) return "";
    const p = (n) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  }
  function addRow(line) {
    const row = document.createElement("div");
    row.className = "k-log-row";
    row.dataset.level = line.lvl || "";
    row.dataset.text = (line.msg || "").toLowerCase();
    const t = document.createElement("span");
    t.className = "k-log-time";
    t.textContent = fmtTime(line.t);
    const lvl = document.createElement("span");
    lvl.className = "k-log-lvl";
    if (line.lvl) lvl.textContent = line.lvl.toUpperCase();
    const msg = document.createElement("span");
    msg.className = "k-log-msg";
    msg.textContent = line.msg || "";
    row.appendChild(t); row.appendChild(lvl); row.appendChild(msg);
    if (!rowVisible(row)) row.style.display = "none";
    const nearBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 40;
    body.appendChild(row);
    while (body.childElementCount > MAX_ROWS) body.removeChild(body.firstElementChild);
    if (nearBottom) body.scrollTop = body.scrollHeight;
  }

  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}${el.dataset.ws}`);
  let got = false;
  ws.onmessage = (e) => { got = true; try { addRow(JSON.parse(e.data)); } catch (_) { /* ignore */ } };
  ws.onclose = () => { if (!got) addRow({ msg: "no logs yet — start or redeploy the service to stream logs" }); };
  ws.onerror = () => addRow({ lvl: "error", msg: "log stream error" });
}

// krillExecConnect (re)connects the terminal using the current command field.
window.krillExecConnect = function () {
  const el = document.querySelector(".k-term[data-term-ws]");
  if (el && el._krillConnect) el._krillConnect();
};

function mountMonitoring(el) {
  if (!el || el.dataset.mounted) return;
  if (typeof uPlot === "undefined") { setTimeout(() => mountMonitoring(el), 50); return; }
  el.dataset.mounted = "1";
  const base = el.dataset.url;
  let range = "24h", cpuU = null, memU = null, nodeFilter = "", lastData = null;
  const I18N = { stale: el.dataset.i18nStale || "stale", allNodes: el.dataset.i18nAllnodes || "All nodes" };
  const ACTIVE = "k-seg-active"; // must match the template + CSS active class

  el.querySelectorAll("#mon-range .k-seg-btn").forEach((b) => b.addEventListener("click", () => {
    el.querySelectorAll("#mon-range .k-seg-btn").forEach((x) => x.classList.remove(ACTIVE));
    b.classList.add(ACTIVE); range = b.dataset.range; load();
  }));

  // Node filter (charts only): re-render from the last fetched data, no re-fetch.
  const nodeSel = el.querySelector("#mon-node-sel");
  if (nodeSel) nodeSel.addEventListener("change", () => { nodeFilter = nodeSel.value; if (lastData) build(lastData); });

  function fmtMB(mb){ return mb >= 1024 ? (mb/1024).toFixed(1)+" GB" : Math.round(mb)+" MB"; }
  function mkChart(node, data, series){
    return new uPlot({ width: node.clientWidth || 520, height: 190, padding:[6,8,0,2], legend:{show:false},
      cursor:{points:{size:5}}, scales:{x:{time:true}},
      axes:[{stroke:"#8b8b93",grid:{stroke:"#1b1b20"},font:"10px ui-monospace",size:30},
            {stroke:"#8b8b93",grid:{stroke:"#1b1b20"},font:"10px ui-monospace",size:42}], series }, data, node);
  }
  function build(d){
    lastData = d;
    const xs = d.x || [];
    // Refresh the node filter options, preserving the current selection (fall back to All if that node vanished).
    if (nodeSel) {
      const want = nodeFilter;
      nodeSel.innerHTML = [`<option value="">${escapeHtml(I18N.allNodes)}</option>`]
        .concat((d.nodes||[]).map((n) => `<option value="${escapeHtml(n.node)}">${escapeHtml(n.node)}</option>`)).join("");
      nodeSel.value = want;
      nodeFilter = nodeSel.value;
    }
    const cpuData=[xs], memData=[xs], cpuSer=[{}], memSer=[{}];
    for (const s of (d.series||[])) {
      if (nodeFilter && s.node !== nodeFilter) continue; // charts: show only the selected node (All = no filter)
      cpuData.push(s.cpu); memData.push(s.mem);
      cpuSer.push({label:s.name,stroke:s.color||"#888",width:1.5,points:{show:false},spanGaps:false});
      memSer.push({label:s.name,stroke:s.color||"#888",width:1.5,points:{show:false},spanGaps:false});
    }
    const cpuNode=el.querySelector("#mon-cpu"), memNode=el.querySelector("#mon-mem");
    if (cpuU) cpuU.destroy(); if (memU) memU.destroy();
    cpuNode.innerHTML=""; memNode.innerHTML="";
    cpuU=mkChart(cpuNode,cpuData,cpuSer); memU=mkChart(memNode,memData,memSer);

    const nodes = d.nodes || [];
    el.querySelector("#mon-nodes").innerHTML = nodes.map(function(n){
      const memPct = n.mem_total ? Math.round(n.mem_used/n.mem_total*100) : 0;
      const stale = n.stale ? ` <span class='k-badge k-badge-warn'>${escapeHtml(I18N.stale)}</span>` : "";
      return `<span class='k-mon-node'><b>${escapeHtml(n.node)}</b>${stale} cpu ${(n.cpu_pct||0).toFixed(0)}% · mem ${fmtMB((n.mem_used||0)/1048576)} / ${fmtMB((n.mem_total||0)/1048576)} (${memPct}%) · ${n.containers||0}c</span>`;
    }).join("");

    // Group by node: a node header, then that node's components ordered by class (control/infra/app/db) then name.
    const ord = function(g){ return g==="control"?0:g==="infra"?1:g==="app"?2:g==="db"?3:9; };
    const rows = (d.rows||[]).slice().sort(function(a,b){
      const an=a.node||"", bn=b.node||"";
      if (an!==bn) return an<bn?-1:1;
      const go=ord(a.group)-ord(b.group);
      return go!==0 ? go : (a.name||"").localeCompare(b.name||"");
    });
    let html="<table class='k-table'><thead><tr><th>Component</th><th>CPU</th><th>Memory</th></tr></thead><tbody>", curNode=null;
    for (const r of rows) {
      if (nodeFilter && r.node !== nodeFilter) continue; // node selector also filters the table (All = no filter)
      if (r.node!==curNode){ curNode=r.node; html+=`<tr class='k-mon-grp'><td colspan='3'>${escapeHtml(curNode||"—")}</td></tr>`; }
      html+=`<tr><td><span style='display:inline-block;width:9px;height:9px;border-radius:2px;margin-right:8px;background:${r.color||"#555"}'></span>${escapeHtml(r.name)}</td><td>${(r.cpu||0).toFixed(1)} %</td><td>${fmtMB(r.mem/1048576)}</td></tr>`;
    }
    el.querySelector("#mon-table").innerHTML = html+"</tbody></table>";
  }
  function escapeHtml(s){ const d=document.createElement("div"); d.textContent=s==null?"":String(s); return d.innerHTML; }
  function load(){ fetch(`${base}?range=${range}`).then(r=>r.json()).then(build).catch(()=>{}); }
  load();
  // Store the poll timer globally so krillEnhance clears it on the next nav/swap
  // (otherwise an extra fetch loop would stack on every visit to this page).
  if (window.__krillMonTimer) clearInterval(window.__krillMonTimer);
  window.__krillMonTimer = setInterval(load, 10000);
  // A single resize handler, stored globally and removed on the next nav/swap by
  // krillEnhance (otherwise a new listener pinning a detached chart leaks per visit).
  if (window.__krillMonResize) window.removeEventListener("resize", window.__krillMonResize);
  window.__krillMonResize = function(){
    if (cpuU) cpuU.setSize({width:el.querySelector("#mon-cpu").clientWidth,height:190});
    if (memU) memU.setSize({width:el.querySelector("#mon-mem").clientWidth,height:190});
  };
  window.addEventListener("resize", window.__krillMonResize);
}

function mountTopology(el) {
  if (!el || el.dataset.mounted) return;
  el.dataset.mounted = "1";
  const base = el.dataset.url;
  const I18N = {
    empty: el.dataset.i18nEmpty || "No services yet",
    cross: el.dataset.i18nCrossnode || "cross-node",
  };
  const NS = "http://www.w3.org/2000/svg";
  const canvas = el.querySelector(".k-topo-canvas");
  const btn = el.querySelector(".k-topo-refresh");
  let data = null;

  function mk(tag, attrs, text) {
    const n = document.createElementNS(NS, tag);
    for (const k in attrs) n.setAttribute(k, attrs[k]);
    if (text != null) n.textContent = text;
    return n;
  }

  function draw() {
    canvas.innerHTML = "";
    const realSvcs = (data && data.services || []).filter((s) => s.kind === "app" || s.kind === "db");
    if (!realSvcs.length) {
      const p = document.createElement("p");
      p.className = "k-muted";
      p.style.padding = "1rem";
      p.textContent = I18N.empty;
      canvas.appendChild(p);
      return;
    }
    const LANE_W = 240, LANE_GAP = 56, PAD = 16, HEAD_H = 34,
      BOX_H = 46, BOX_GAP = 12, CHIP_H = 20, CHIP_GAP = 4, BOX_W = LANE_W - PAD * 2;

    // Order lanes: control-plane/manager first, workers by name, unplaced last.
    const rank = (n) => (n.id === "ingress" ? -1 : n.id === "unplaced" ? 2 : (n.leader || n.role === "manager") ? 0 : 1);
    const nodes = (data.nodes || []).slice().sort((a, b) => {
      const r = rank(a) - rank(b);
      return r !== 0 ? r : (a.name || "").localeCompare(b.name || "");
    });

    const byNode = {};
    for (const s of data.services) (byNode[s.node] = byNode[s.node] || []).push(s);
    const dbsByInst = {};
    for (const d of (data.dbs || [])) (dbsByInst[d.service] = dbsByInst[d.service] || []).push(d);

    // Geometry, keyed by service id and "L"+dbId.
    const anchor = {};
    let maxH = 0;
    nodes.forEach((n, ci) => {
      const laneX = PAD + ci * (LANE_W + LANE_GAP);
      let y = HEAD_H + PAD;
      for (const s of (byNode[n.id] || [])) {
        const chips = s.kind === "db" ? (dbsByInst[s.id] || []) : [];
        s.__x = laneX + PAD; s.__y = y; s.__w = BOX_W;
        anchor[s.id] = {
          left: { x: laneX + PAD, y: y + BOX_H / 2 },
          right: { x: laneX + PAD + BOX_W, y: y + BOX_H / 2 },
        };
        let cy = y + BOX_H + CHIP_GAP;
        for (const d of chips) {
          d.__x = laneX + PAD + 8; d.__y = cy; d.__w = BOX_W - 16;
          anchor["L" + d.id] = {
            left: { x: laneX + PAD + 8, y: cy + CHIP_H / 2 },
            right: { x: laneX + PAD + BOX_W - 8, y: cy + CHIP_H / 2 },
          };
          cy += CHIP_H + CHIP_GAP;
        }
        const h = BOX_H + (chips.length ? chips.length * (CHIP_H + CHIP_GAP) + CHIP_GAP : 0);
        y += h + BOX_GAP;
      }
      n.__x = laneX;
      if (y > maxH) maxH = y;
    });

    const totalW = PAD * 2 + nodes.length * LANE_W + Math.max(0, nodes.length - 1) * LANE_GAP;
    const totalH = Math.max(maxH + PAD, 160);
    const svg = mk("svg", { width: totalW, height: totalH, viewBox: `0 0 ${totalW} ${totalH}`, class: "k-topo-svg" });

    const defs = mk("defs", {});
    const marker = mk("marker", { id: "k-topo-arrow", viewBox: "0 0 10 10", refX: "9", refY: "5", markerWidth: "6", markerHeight: "6", orient: "auto-start-reverse" });
    marker.appendChild(mk("path", { d: "M 0 0 L 10 5 L 0 10 z", class: "k-topo-arrowhead" }));
    defs.appendChild(marker);
    svg.appendChild(defs);

    // Lanes.
    for (const n of nodes) {
      svg.appendChild(mk("rect", { x: n.__x, y: 0, width: LANE_W, height: totalH, rx: 10, class: "k-topo-lane" }));
      svg.appendChild(mk("text", { x: n.__x + PAD, y: 22, class: "k-topo-lanehead" }, n.name));
      if (n.role) svg.appendChild(mk("text", { x: n.__x + LANE_W - PAD, y: 22, "text-anchor": "end", class: "k-topo-lanerole" }, n.role));
    }

    // Links (under boxes). Directed: arrowhead at the `to` end.
    const linkEls = [];
    for (const l of (data.links || [])) {
      const from = anchor[l.from];
      const to = l.to_kind === "logical" ? anchor["L" + l.to_id] : anchor[l.to_id];
      if (!from || !to) continue;
      const rightward = from.right.x <= to.left.x;
      const a = rightward ? from.right : from.left;
      const b = rightward ? to.left : to.right;
      const dx = Math.abs(b.x - a.x) / 2 + 24;
      const c1 = a.x + (b.x >= a.x ? dx : -dx);
      const c2 = b.x + (b.x >= a.x ? -dx : dx);
      const d = `M ${a.x} ${a.y} C ${c1} ${a.y}, ${c2} ${b.y}, ${b.x} ${b.y}`;
      let cls = "k-topo-link";
      if (l.kind === "ingress") cls += " k-topo-link-ingress";
      else cls += " k-topo-link-" + (l.engine || "postgres");
      if (l.detected) cls += " k-topo-link-detected";
      if (l.cross_node) cls += " k-topo-cross";
      const toKey = l.to_kind === "logical" ? "L" + l.to_id : l.to_id;
      const path = mk("path", { d: d, class: cls, "marker-end": "url(#k-topo-arrow)", "data-from": l.from, "data-to": toKey });
      let tip;
      if (l.kind === "ingress") tip = l.label || "";
      else {
        const base = l.label || (l.field ? l.var + " → " + l.field : l.var);
        tip = base + (l.detected ? " (env)" : "") + (l.cross_node ? " (" + I18N.cross + ")" : "");
      }
      path.appendChild(mk("title", {}, tip));
      linkEls.push(path);
      svg.appendChild(path);
      // domain label on ingress edges (gateway -> app)
      if (l.kind === "ingress" && l.label) {
        svg.appendChild(mk("text", { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 - 4, "text-anchor": "middle", class: "k-topo-edgelabel" }, l.label));
      }
    }

    function focusOn(key) {
      svg.classList.add("k-topo-focused");
      for (const p of linkEls) {
        const on = p.getAttribute("data-from") === key || p.getAttribute("data-to") === key;
        p.classList.toggle("k-topo-lit", on);
      }
    }
    function focusOff() {
      svg.classList.remove("k-topo-focused");
      for (const p of linkEls) p.classList.remove("k-topo-lit");
    }

    // Service boxes + chips (on top; chips are siblings, not nested, so hover
    // is independent).
    for (const n of nodes) {
      for (const s of (byNode[n.id] || [])) {
        const g = mk("g", { class: "k-topo-svc k-topo-" + s.kind + (s.status && s.status !== "running" ? " k-topo-off" : ""), "data-id": s.id });
        g.appendChild(mk("rect", { x: s.__x, y: s.__y, width: s.__w, height: BOX_H, rx: 8, class: "k-topo-box" }));
        g.appendChild(mk("text", { x: s.__x + 12, y: s.__y + 20, class: "k-topo-name" }, s.label));
        if (s.sub) g.appendChild(mk("text", { x: s.__x + 12, y: s.__y + 36, class: "k-topo-sub" }, s.sub));
        g.addEventListener("mouseenter", () => focusOn(s.id));
        g.addEventListener("mouseleave", focusOff);
        svg.appendChild(g);
        for (const d of (dbsByInst[s.id] || [])) {
          const cg = mk("g", { class: "k-topo-chip", "data-id": "L" + d.id });
          cg.appendChild(mk("rect", { x: d.__x, y: d.__y, width: d.__w, height: CHIP_H, rx: 5, class: "k-topo-chipbox" }));
          cg.appendChild(mk("text", { x: d.__x + 8, y: d.__y + 14, class: "k-topo-chiptext" }, d.name + (d.env ? " · " + d.env : "")));
          cg.addEventListener("mouseenter", () => focusOn("L" + d.id));
          cg.addEventListener("mouseleave", focusOff);
          svg.appendChild(cg);
        }
      }
    }

    canvas.appendChild(svg);
  }

  function load() {
    fetch(base).then((r) => r.json()).then((d) => { data = d; draw(); }).catch(() => {});
  }
  if (btn) btn.addEventListener("click", load);
  load();

  // Redraw on resize (layout is width-independent here, but keep the same
  // single-global-handler discipline as monitoring so it is cleaned up on nav).
  if (window.__krillTopoResize) window.removeEventListener("resize", window.__krillTopoResize);
  window.__krillTopoResize = function () { if (data) draw(); };
  window.addEventListener("resize", window.__krillTopoResize);
}

// krillEnhance wires up content present on the page or just swapped in: it mounts
// any unmounted log terminals and initializes the env editor. Idempotent.
window.krillEnhance = function () {
  document.querySelectorAll(".k-term[data-ws]:not([data-mounted])").forEach(mountTerminalEl);
  document.querySelectorAll(".k-term[data-term-ws]:not([data-mounted])").forEach(mountExecTerminal);
  document.querySelectorAll(".k-logs[data-ws]:not([data-mounted])").forEach(mountLogViewer);
  document.querySelectorAll(".k-mon:not([data-mounted])").forEach(mountMonitoring);
  document.querySelectorAll(".k-topo:not([data-mounted])").forEach(mountTopology);
  if (document.getElementById("env-form")) {
    let mode = "kv";
    try { mode = localStorage.getItem("krillEnvMode") || "kv"; } catch (_) { /* private mode */ }
    krillEnvMode(mode);
  }
  krillMountDBLink();
  // Stop a monitoring poll timer from a previous page (cleared on every nav/swap;
  // re-armed by mountMonitoring only when the monitoring page is present).
  if (!document.querySelector(".k-mon")) {
    if (window.__krillMonTimer) { clearInterval(window.__krillMonTimer); window.__krillMonTimer = null; }
    if (window.__krillMonResize) { window.removeEventListener("resize", window.__krillMonResize); window.__krillMonResize = null; }
  }
  if (!document.querySelector(".k-topo")) {
    if (window.__krillTopoResize) { window.removeEventListener("resize", window.__krillTopoResize); window.__krillTopoResize = null; }
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

// Engine-aware Scheme select for the DB-link form: rebuilds the scheme options to
// only those valid for the currently selected database (Postgres -> postgresql/
// postgres, Redis -> redis) and reveals a hint explaining that postgres:// and
// postgresql:// are interchangeable aliases (the choice is about the consuming
// framework, not the database). Idempotent (guarded by data-mounted).
function krillMountDBLink() {
  const dbSel = document.getElementById("dblink-db");
  const schemeSel = document.getElementById("dblink-scheme");
  if (!dbSel || !schemeSel || schemeSel.dataset.mounted) return;
  schemeSel.dataset.mounted = "1";
  const hint = document.getElementById("dblink-scheme-hint");
  const fieldSel = document.getElementById("dblink-field");
  const schemeWrap = document.getElementById("dblink-scheme-wrap");
  const all = Array.from(schemeSel.options).map((o) => ({ value: o.value, label: o.textContent, engine: o.dataset.engine }));
  const apply = () => {
    const engine = dbSel.value.split(":")[0] || "";
    const prev = schemeSel.value;
    schemeSel.innerHTML = "";
    all.filter((o) => o.engine === engine).forEach((o) => {
      const opt = document.createElement("option");
      opt.value = o.value;
      opt.textContent = o.label;
      schemeSel.appendChild(opt);
    });
    if (Array.from(schemeSel.options).some((o) => o.value === prev)) schemeSel.value = prev;
    // Scheme and its hint only apply to the "url" field of a postgres DB.
    const isUrl = !fieldSel || fieldSel.value === "url";
    if (schemeWrap) schemeWrap.style.display = isUrl ? "" : "none";
    if (hint) hint.style.display = (isUrl && engine === "pg") ? "" : "none";
  };
  dbSel.addEventListener("change", apply);
  if (fieldSel) fieldSel.addEventListener("change", apply);
  apply();
}

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

// Disable a submit button on form submit to prevent double-submits and signal activity.
window.krillBusy = function (btn, label) {
  if (!btn) return;
  btn.disabled = true;
  btn.dataset.label = btn.textContent;
  btn.textContent = label || "Working…";
};

// Toggle masking of a single input (e.g. a connection string) between password
// and text; flips the eye icon on the button that triggered it.
window.krillReveal = function (btn) {
  const inp = btn.parentElement.querySelector("input");
  if (!inp) return;
  inp.type = inp.type === "password" ? "text" : "password";
  btn.textContent = inp.type === "password" ? "👁" : "🙈";
};

// Apply the current reveal state (masked by default, Dokploy-style) to BOTH the
// key/value value inputs AND the raw textarea. Key/value mode masks each value
// input (type=password); raw mode masks the whole textarea (-webkit-text-security)
// since a textarea can't mask per-value. Called on toggle and on editor init/mode
// switch so masking holds in both modes.
window.krillApplyEnvReveal = function () {
  const on = !!window.__krillEnvReveal;
  document.querySelectorAll("#env-rows .env-val").forEach(function (i) {
    i.type = on ? "text" : "password";
  });
  const ta = document.getElementById("env-textarea");
  if (ta) ta.style.webkitTextSecurity = on ? "none" : "disc";
};
window.krillEnvRevealToggle = function (btn) {
  window.__krillEnvReveal = !window.__krillEnvReveal;
  window.krillApplyEnvReveal();
  const on = window.__krillEnvReveal;
  btn.textContent = (on ? "🙈 " : "👁 ") + (on ? btn.dataset.hide : btn.dataset.show);
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
  v.type = window.__krillEnvReveal ? "text" : "password"; // values masked by default
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
  // Honor the current masked/revealed state in the now-active mode (so the raw
  // textarea is masked by default on load, matching the key/value inputs).
  window.krillApplyEnvReveal();
};

// Run enhancements on first load and after every hx-boost swap. Guarded so a
// re-executed app.js does not stack duplicate listeners.
if (!window.__krillEnhanceInit) {
  window.__krillEnhanceInit = true;
  document.addEventListener("DOMContentLoaded", window.krillEnhance);
  document.addEventListener("htmx:afterSettle", window.krillEnhance);
}
window.krillEnhance();
