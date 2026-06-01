// Монтирует xterm в #terminal и стримит логи по WebSocket.
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

// Переключение полей источника в форме создания приложения.
window.krillToggleSource = function (val) {
  document.querySelectorAll('[data-src]').forEach(function (el) {
    el.style.display = (el.getAttribute('data-src') === val) ? '' : 'none';
  });
};

// Открыть/закрыть нативную модалку <dialog> по id.
window.krillOpenModal = function (id) {
  const d = document.getElementById(id);
  if (d && typeof d.showModal === "function") d.showModal();
};
window.krillCloseModal = function (id) {
  const d = document.getElementById(id);
  if (d && typeof d.close === "function") d.close();
};
// Клик по подложке (вне содержимого) закрывает модалку.
document.addEventListener("click", function (e) {
  if (e.target && e.target.tagName === "DIALOG" && e.target.classList.contains("k-modal")) {
    e.target.close();
  }
});
