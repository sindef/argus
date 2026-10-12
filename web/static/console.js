const screen = document.getElementById("screen");
const state = document.getElementById("state");
const url = "wss://" + location.host + screen.dataset.ws + screen.dataset.kind;
if (screen.dataset.kind === "vnc") {
  const { default: RFB } = await import("/static/vendor/novnc/core/rfb.js");
  const rfb = new RFB(screen, url);
  rfb.scaleViewport = true;
  rfb.addEventListener("connect", () => { state.textContent = ""; });
  rfb.addEventListener("disconnect", () => { state.textContent = "Disconnected"; });
} else {
  const { Terminal } = await import("/static/vendor/xterm/xterm.mjs");
  const term = new Terminal();
  term.open(screen);
  const ws = new WebSocket(url);
  ws.binaryType = "arraybuffer";
  const enc = new TextEncoder();
  ws.onopen = () => { state.textContent = ""; term.focus(); ws.send(enc.encode("\r")); };
  ws.onclose = () => { state.textContent = "Disconnected"; };
  ws.onmessage = (e) => term.write(new Uint8Array(e.data));
  term.onData((d) => ws.send(enc.encode(d)));
}
