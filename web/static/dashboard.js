(() => {
  const keep = 180, every = 5000;
  const units = ["B", "KB", "MB", "GB", "TB"];
  const human = (v, suffix = "") => { let i = 0; while (v >= 1000 && i < units.length - 1) { v /= 1000; i++; } return (v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)) + " " + units[i] + suffix; };
  const $ = (id) => document.getElementById(id);
  const series = { t: [], cpu: [], mem: [], rd: [], wr: [], rx: [], tx: [] };
  const sum = (m, k) => Object.values(m || {}).reduce((a, c) => a + c[k], 0);
  let prev, charts = {};
  const axis = { stroke: "#8ea3c4", grid: { stroke: "#1c3050", width: 1 }, ticks: { show: false }, font: "11px Inter, sans-serif" };
  const make = (id, cols, labels, colors, fmt, range) => {
    const el = $(id);
    const opts = {
      width: el.clientWidth, height: 160, legend: { show: cols.length > 1, live: false },
      scales: { x: { time: true }, y: range ? { range } : { range: (u, lo, hi) => [0, Math.max(hi * 1.1, 1)] } },
      axes: [axis, { ...axis, size: 64, values: (u, vs) => vs.map(fmt) }],
      series: [{}, ...cols.map((c, i) => ({ label: labels[i], stroke: colors[i], width: 1.5, fill: colors[i] + "22" }))],
    };
    const u = new uPlot(opts, [series.t, ...cols.map((c) => series[c])], el);
    new ResizeObserver(() => u.setSize({ width: el.clientWidth, height: 160 })).observe(el);
    charts[id] = { u, cols };
  };
  const pct = (v) => Math.round(v) + "%";
  function draw() {
    if (!charts["chart-cpu"]) {
      make("chart-cpu", ["cpu"], ["CPU"], ["#22c55e"], pct, [0, 100]);
      make("chart-mem", ["mem"], ["Used"], ["#1677ff"], (v) => human(v));
      make("chart-disk", ["rd", "wr"], ["Read", "Write"], ["#8b5cf6", "#1677ff"], (v) => human(v, "/s"));
      make("chart-net", ["rx", "tx"], ["In", "Out"], ["#22c55e", "#1677ff"], (v) => human(v, "/s"));
    }
    for (const c of Object.values(charts)) c.u.setData([series.t, ...c.cols.map((k) => series[k])]);
  }
  async function tick() {
    try {
      const r = await fetch($("dash-error").dataset.src, { credentials: "same-origin" });
      if (!r.ok) throw new Error(await r.text());
      const d = await r.json(), h = d.host;
      $("dash-error").textContent = "";
      const memUsed = h.memTotal - h.memAvail;
      $("dash-mem").textContent = human(memUsed);
      $("dash-mem-detail").textContent = "of " + human(h.memTotal) + ", " + human(d.vmMem) + " assigned to VMs";
      $("dash-mem-bar").style.width = (memUsed / h.memTotal * 100) + "%";
      $("dash-load").textContent = "load " + h.load.map((l) => l.toFixed(2)).join(" ") + " · " + h.cores + " cores";
      $("dash-vms").textContent = d.running + " / " + d.vms;
      $("dash-vms-detail").textContent = "running, " + d.vcpus + " vCPUs in use";
      const up = Math.floor(h.uptime / 86400), hrs = Math.floor(h.uptime % 86400 / 3600);
      $("dash-uptime").textContent = "· up " + (up ? up + "d " : "") + hrs + "h";
      $("dash-pools").innerHTML = (h.pools || []).map((p) => {
        const u = Math.round(p.alloc / p.size * 100);
        return `<div><div class="between"><b>${p.name}</b><span class="muted">${p.health}</span></div><div class="detail">${human(p.alloc)} of ${human(p.size)} (${u}%)</div><div class="bar${u >= 90 ? " bad" : u >= 80 ? " warn" : ""}"><span style="width:${u}%"></span></div></div>`;
      }).join("");
      if (prev) {
        const dt = (h.t - prev.t) / 1000;
        const busy = 100 * (1 - (h.cpuIdle - prev.cpuIdle) / Math.max(h.cpuTotal - prev.cpuTotal, 1));
        const rate = (a, b) => Math.max(a - b, 0) / dt;
        const point = {
          cpu: busy, mem: memUsed,
          rd: rate(sum(h.disks, "a"), sum(prev.disks, "a")), wr: rate(sum(h.disks, "b"), sum(prev.disks, "b")),
          rx: rate(sum(h.net, "a"), sum(prev.net, "a")), tx: rate(sum(h.net, "b"), sum(prev.net, "b")),
        };
        series.t.push(h.t / 1000);
        for (const k of Object.keys(point)) series[k].push(point[k]);
        if (series.t.length > keep) for (const k of Object.keys(series)) series[k].shift();
        $("dash-cpu").textContent = pct(busy);
        $("dash-cpu-bar").style.width = busy + "%";
        $("dash-disk").textContent = human(point.rd, "/s") + " / " + human(point.wr, "/s");
        $("dash-net").textContent = human(point.rx, "/s") + " / " + human(point.tx, "/s");
        draw();
      }
      prev = h;
    } catch (e) {
      $("dash-error").textContent = "Stats unavailable: " + e.message;
    }
  }
  document.addEventListener("DOMContentLoaded", () => { tick(); setInterval(() => { if (!document.hidden) tick(); }, every); });
})();
