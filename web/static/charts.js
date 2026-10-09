(() => {
  const colors = ["#22c55e", "#1677ff", "#8b5cf6", "#f5b819"];
  const units = ["", "K", "M", "G", "T"];
  const human = (v, unit) => {
    if (unit === "%") return Math.round(v) + "%";
    let i = 0;
    while (Math.abs(v) >= 1000 && i < units.length - 1) { v /= 1000; i++; }
    return (v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)) + " " + units[i] + unit;
  };
  const cache = {};
  const load = (src) => (cache[src] ??= fetch(src, { credentials: "same-origin" }).then((r) => r.json()));

  async function draw(el) {
    const data = await load(el.dataset.src);
    const cols = el.dataset.cols.split(",");
    const labels = (el.dataset.labels || el.dataset.cols).split(",");
    const unit = el.dataset.unit || "";
    if (!data.t.length) {
      el.innerHTML = '<p class="muted">Collecting data…</p>';
      return;
    }
    const last = el.parentElement.querySelector("[data-last]");
    if (last) last.textContent = human(data.cols[cols[0]].at(-1), unit);
    const axis = { stroke: "#8ea3c4", grid: { stroke: "#1c3050", width: 1 }, ticks: { show: false }, font: "11px Inter, sans-serif" };
    const opts = {
      width: el.clientWidth,
      height: +(el.dataset.height || 170),
      legend: { show: cols.length > 1, live: false },
      cursor: { points: { size: 6 } },
      scales: {
        x: { time: true, range: () => { const now = Date.now() / 1000; return [now - +(el.dataset.window || 86400), now]; } },
        y: unit === "%" ? { range: [0, 100] } : { range: (u, lo, hi) => [0, Math.max(hi * 1.1, 1)] },
      },
      axes: [axis, { ...axis, size: 60, values: (u, vs) => vs.map((v) => human(v, unit)) }],
      series: [{}, ...cols.map((c, i) => ({ label: labels[i], stroke: colors[i], width: 1.5, fill: colors[i] + "22", value: (u, v) => (v == null ? "—" : human(v, unit)) }))],
    };
    el.innerHTML = "";
    const plot = new uPlot(opts, [data.t, ...cols.map((c) => data.cols[c])], el);
    new ResizeObserver(() => plot.setSize({ width: el.clientWidth, height: opts.height })).observe(el);
  }

  document.addEventListener("DOMContentLoaded", () => document.querySelectorAll(".chart[data-src]").forEach(draw));
})();
