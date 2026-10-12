const screen = document.getElementById("screen");
const state = document.getElementById("state");
const url = "wss://" + location.host + screen.dataset.ws + screen.dataset.kind;
if (screen.dataset.kind === "vnc") {
  const { default: RFB } = await import("/static/vendor/novnc/core/rfb.js");
  const rfb = new RFB(screen, url);
  rfb.scaleViewport = true;
  rfb.addEventListener("connect", () => { state.textContent = ""; });
  rfb.addEventListener("disconnect", () => { state.textContent = "Disconnected"; });
  const shifted = '~!@#$%^&*()_+{}|:"<>?';
  document.getElementById("paste-form").addEventListener("submit", (e) => {
    e.preventDefault();
    for (const ch of e.target.text.value.replace(/\r\n?/g, "\n")) {
      const c = ch.codePointAt(0);
      const sym = ch === "\n" ? 0xff0d : ch === "\t" ? 0xff09 : c;
      if (sym === c && (c < 0x20 || c > 0x7e)) continue;
      const shift = (ch >= "A" && ch <= "Z") || shifted.includes(ch);
      if (shift) rfb.sendKey(0xffe1, "ShiftLeft", true);
      rfb.sendKey(sym, null, true);
      rfb.sendKey(sym, null, false);
      if (shift) rfb.sendKey(0xffe1, "ShiftLeft", false);
    }
    e.target.reset();
    e.target.closest("dialog").close();
    rfb.focus();
  });
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
