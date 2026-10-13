(() => {
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch {} },
  };
  const drawerWidth = store.get("drawerWidth");
  if (drawerWidth) document.documentElement.style.setProperty("--drawer-w", drawerWidth + "px");
  const nearEdge = (d, e) => e.clientX - d.getBoundingClientRect().left < 8;
  document.addEventListener("pointerdown", (e) => {
    const d = e.target.closest(".drawer");
    if (!d || !nearEdge(d, e)) return;
    d.setPointerCapture(e.pointerId);
    d.classList.add("resizing");
    e.preventDefault();
  });
  document.addEventListener("pointermove", (e) => {
    const d = e.target.closest(".drawer");
    if (!d) return;
    if (!d.classList.contains("resizing")) { d.style.cursor = nearEdge(d, e) ? "col-resize" : ""; return; }
    const w = Math.round(Math.min(Math.max(window.innerWidth - e.clientX, 360), window.innerWidth * 0.8));
    document.documentElement.style.setProperty("--drawer-w", w + "px");
  });
  document.addEventListener("pointerup", (e) => {
    const d = e.target.closest(".drawer.resizing");
    if (!d) return;
    d.classList.remove("resizing");
    store.set("drawerWidth", parseInt(getComputedStyle(document.documentElement).getPropertyValue("--drawer-w"), 10));
  });
  document.addEventListener("dblclick", (e) => {
    const d = e.target.closest(".drawer");
    if (!d || !nearEdge(d, e)) return;
    document.documentElement.style.removeProperty("--drawer-w");
    store.set("drawerWidth", "");
  });
  const place = (menu, x, y, alignRight) => {
    menu.style.position = "fixed";
    const w = menu.offsetWidth, h = menu.offsetHeight;
    let left = alignRight ? x - w : x, top = y;
    if (top + h > innerHeight - 8) top = Math.max(8, y - h - (alignRight ? 40 : 0));
    menu.style.left = Math.max(8, Math.min(left, innerWidth - w - 8)) + "px";
    menu.style.top = top + "px";
    menu.style.right = "auto";
    menu.dataset.opened = Date.now();
  };
  const closeMenus = (except) => {
    document.querySelectorAll(".menu-list:not([hidden])").forEach((m) => { if (m !== except) m.hidden = true; });
  };

  document.addEventListener("click", (e) => {
    const toggle = e.target.closest("[data-menu]");
    if (toggle) {
      const list = toggle.parentElement.querySelector(".menu-list");
      closeMenus(list);
      const inTable = toggle.closest(".table-wrap");
      if (inTable) list.style.position = "fixed";
      list.hidden = !list.hidden;
      if (!list.hidden && inTable) {
        const r = toggle.getBoundingClientRect();
        place(list, r.right, r.bottom + 4, true);
      }
      e.preventDefault();
      return;
    }
    if (!e.target.closest(".menu-list")) closeMenus();
    else if (e.target.closest("button, a")) closeMenus();

    const head = e.target.closest(".nav-head");
    if (head && (head.tagName === "BUTTON" || e.target.closest(".chev"))) {
      e.preventDefault();
      const sec = head.parentElement;
      sec.classList.toggle("closed");
      store.set("nav:" + sec.id, sec.classList.contains("closed") ? "closed" : "open");
      return;
    }
    const twisty = e.target.closest(".twisty");
    if (twisty) {
      const li = twisty.closest("li");
      li.classList.toggle("closed");
      store.set("tree:" + li.dataset.id, li.classList.contains("closed") ? "closed" : "open");
      e.preventDefault();
      e.stopPropagation();
      return;
    }
    const trow = e.target.closest(".tree span.row");
    if (trow && !e.target.closest("a, button, .menu-list")) {
      const a = trow.querySelector(":scope > a[href]");
      if (a) { location.href = a.href; return; }
    }
    const reveal = e.target.closest("[data-reveal]");
    if (reveal) {
      const input = reveal.parentElement.querySelector("input");
      input.type = input.type === "password" ? "text" : "password";
      return;
    }
    const all = e.target.closest("[data-select-all]");
    if (all) {
      document.querySelectorAll("input[name=sel]").forEach((c) => { c.checked = all.checked; });
      syncSelection();
      return;
    }
    if (e.target.matches("input[name=sel]")) { syncSelection(); return; }
    const row = e.target.closest("tr[data-href]");
    if (row && !e.target.closest("input, button, a, form")) {
      htmx.ajax("GET", row.dataset.href, { target: "#drawer", swap: "innerHTML" });
      if (row.dataset.page) history.replaceState(null, "", row.dataset.page);
      document.querySelectorAll("tr.selected").forEach((r) => r.classList.remove("selected"));
      row.classList.add("selected");
      document.querySelector(".content").classList.add("with-drawer");
    }
    const mrow = e.target.closest("tr[data-modal]");
    if (mrow && !e.target.closest("input, button, a, form")) {
      htmx.ajax("GET", mrow.dataset.modal, { target: "#modal", swap: "innerHTML" });
      return;
    }
    const dlg = e.target.closest("[data-dialog]");
    if (dlg && !(dlg.tagName === "TR" && e.target.closest("button, a, input, select, .menu"))) {
      const d = document.getElementById(dlg.dataset.dialog);
      if (dlg.dataset.fill) for (const [k, v] of Object.entries(JSON.parse(dlg.dataset.fill))) {
        if (k === "action") { const f = d.querySelector("form"); f.setAttribute("hx-post", v); htmx.process(f); }
        else if (Array.isArray(v)) d.querySelectorAll(`[name="${k}"]`).forEach((el) => { el.checked = v.includes(el.value); });
        else d.querySelector(`[name="${k}"]`).value = v;
      }
      d.querySelectorAll("form").forEach(archCheck);
      d.showModal();
      return;
    }
    if (e.target.closest("[data-close-dialog]")) { e.target.closest("dialog").close(); return; }
    if (e.target.closest("[data-reload]")) { location.reload(); return; }
    const close = e.target.closest("[data-close-drawer]");
    if (close) {
      document.getElementById("drawer").innerHTML = "";
      history.replaceState(null, "", "/");
      document.querySelector(".content").classList.remove("with-drawer");
      document.querySelectorAll("tr.selected").forEach((r) => r.classList.remove("selected"));
    }
  });

  document.addEventListener("change", (e) => {
    const sel = e.target.closest("select[data-interval]");
    if (!sel) return;
    const input = sel.nextElementSibling;
    input.hidden = sel.value !== "custom";
    if (sel.value === "custom") input.focus();
    else input.value = sel.value;
  });

  addEventListener("scroll", (e) => {
    if (e.target.closest?.(".menu-list")) return;
    document.querySelectorAll(".menu-list:not([hidden])").forEach((m) => { if (m.dataset.opened && Date.now() - m.dataset.opened > 250) m.hidden = true; });
  }, true);

  document.addEventListener("contextmenu", (e) => {
    const item = e.target.closest("[data-context]") || e.target.closest("tr:has(> .row-actions .menu-list)");
    if (!item) return;
    let menu = item.querySelector(".menu-list");
    const tpl = item.querySelector("template.ctx");
    if (tpl) {
      menu = document.getElementById("ctxmenu");
      menu.innerHTML = tpl.innerHTML;
      htmx.process(menu);
    }
    if (!menu) return;
    e.preventDefault();
    closeMenus(menu);
    menu.style.position = "fixed";
    menu.hidden = false;
    place(menu, e.clientX, e.clientY, false);
  });

  const syncSSO = () => {
    const sel = document.querySelector("select[name=org]"), sso = document.getElementById("sso");
    if (sel && sso) sso.hidden = !sel.selectedOptions[0]?.hasAttribute("data-sso");
  };
  document.addEventListener("change", (e) => {
    if (e.target.matches("select[name=org]")) syncSSO();
    if (e.target.matches("select[data-prefill]")) {
      const o = e.target.selectedOptions[0];
      for (const [k, v] of Object.entries(o.dataset)) { const i = e.target.form.querySelector(`[name="${k}"]`); if (i) i.value = v; }
    }
  });

  document.addEventListener("htmx:afterRequest", (e) => {
    const d = e.target.closest("dialog");
    if (d && !d.contains(e.detail.target)) d.close();
  });
  document.addEventListener("htmx:xhr:progress", (e) => {
    const id = e.target.closest("[data-progress]")?.dataset.progress;
    const p = id && document.getElementById(id);
    if (p && e.detail.total) { p.hidden = false; p.value = e.detail.loaded / e.detail.total * 100; }
  });
  document.addEventListener("htmx:afterRequest", (e) => {
    if (e.target.matches("[data-opens-drawer]")) document.querySelector(".content").classList.add("with-drawer");
  });
  document.addEventListener("htmx:afterSwap", (e) => {
    if (e.target.id === "flash") {
      clearTimeout(e.target.clearAfter);
      e.target.clearAfter = setTimeout(() => { e.target.textContent = ""; }, 8000);
    }
  });
  document.addEventListener("htmx:afterSwap", (e) => {
    if (e.target.id === "modal" && e.target.innerHTML.trim()) e.target.showModal();
  });

  document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeMenus(); });

  function syncSelection() {
    const n = document.querySelectorAll("input[name=sel]:checked").length;
    document.querySelectorAll("[data-needs-selection]").forEach((b) => { b.disabled = n === 0; });
  }

  const ask = (question, ok) => {
    const dlg = document.getElementById("confirm");
    const form = dlg.querySelector("form"), btn = form.querySelector("[type=submit]");
    const verb = (question.match(/^(Delete|Remove|Destroy|Revoke|Shut down|Reboot|Restore|Erase|Export|Apply|Update|Migrate|Fail over)\b/) || [])[1];
    dlg.querySelector("h2").textContent = question;
    btn.textContent = verb || "OK";
    btn.className = "btn " + (/^(Delete|Remove|Destroy|Revoke|Erase)/.test(question) ? "danger" : "primary");
    form.onsubmit = (ev) => { ev.preventDefault(); dlg.close(); ok(); };
    dlg.showModal();
    btn.focus();
  };
  document.addEventListener("submit", (e) => {
    const f = e.target;
    if (!f.dataset.confirm || f.dataset.confirmed) return;
    e.preventDefault();
    ask(f.dataset.confirm, () => { f.dataset.confirmed = "1"; f.requestSubmit(); });
  }, true);

  document.addEventListener("htmx:confirm", (e) => {
    const el = e.detail.elt, d = el.dataset, dlg = document.getElementById("prompt");
    if (d.prompt === undefined && e.detail.question) {
      e.preventDefault();
      ask(e.detail.question, () => e.detail.issueRequest(true));
      return;
    }
    if (d.prompt === undefined || !dlg) return;
    e.preventDefault();
    const form = dlg.querySelector("form"), input = form.elements.value;
    dlg.querySelector("h2").textContent = d.prompt;
    input.value = d.promptValue || "";
    input.placeholder = d.promptPlaceholder || "";
    input.required = d.promptOptional === undefined;
    input.setCustomValidity("");
    input.oninput = () => input.setCustomValidity("");
    form.onsubmit = (ev) => {
      ev.preventDefault();
      if (d.promptMatch !== undefined && input.value !== d.promptMatch) {
        input.setCustomValidity("Type " + d.promptMatch + " to confirm");
        input.reportValidity();
        return;
      }
      const v = input.value;
      dlg.close();
      el.addEventListener("htmx:configRequest", (c) => { c.detail.headers["HX-Prompt"] = v; }, { once: true });
      e.detail.issueRequest(true);
    };
    dlg.showModal();
    input.select();
  });

  const archCheck = (form) => {
    const src = form.querySelector("[data-arch-source]");
    const want = src ? (src.selectedOptions?.[0]?.dataset.arch || src.value) : form.dataset.arch;
    const warn = form.querySelector(".arch-warn");
    if (!warn || !want) return;
    warn.hidden = ![...form.querySelectorAll("input[data-arch]:checked, select:not([data-arch-source]) option[data-arch]:checked")].some((e) => e.dataset.arch !== want);
  };
  document.addEventListener("change", (e) => {
    const f = e.target.closest("form[data-arch], form:has([data-arch-source])");
    if (f) archCheck(f);
  });

  const buildPanes = (root) => {
    if (root.querySelector(":scope > .panes-nav")) return;
    const nav = document.createElement("nav"), body = document.createElement("div");
    nav.className = "panes-nav";
    body.className = "panes-body";
    root.querySelectorAll(":scope > .pane").forEach((s) => {
      const a = document.createElement("a");
      a.href = "#" + s.id;
      a.innerHTML = `<svg class="i"><use href="/static/vendor/lucide/icons.svg#i-${s.dataset.icon}"/></svg>`;
      a.append(s.querySelector("h2").textContent);
      nav.append(a);
      body.append(s);
    });
    root.append(nav, body);
  };
  const showPane = (root, id) => {
    buildPanes(root);
    const links = [...root.querySelectorAll(".panes-nav a")];
    if (!links.length) return;
    const has = (v) => links.some((a) => a.hash === "#" + v);
    if (!has(id)) id = store.get("pane:" + location.pathname);
    if (!has(id)) id = links[0].hash.slice(1);
    links.forEach((a) => a.classList.toggle("active", a.hash === "#" + id));
    root.querySelectorAll(".pane").forEach((s) => s.classList.toggle("active", s.id === id));
    root.classList.add("ready");
  };
  document.addEventListener("click", (e) => {
    const a = e.target.closest(".panes:not(.static) > .panes-nav a");
    if (!a) return;
    e.preventDefault();
    const id = a.hash.slice(1);
    store.set("pane:" + location.pathname, id);
    history.replaceState(null, "", a.hash);
    showPane(a.closest(".panes"), id);
  });

  const restore = () => {
    document.querySelectorAll(".nav-section[id]").forEach((s) => {
      const v = store.get("nav:" + s.id);
      if (v) s.classList.toggle("closed", v === "closed");
    });
    document.querySelectorAll(".panes:not(.static)").forEach((root) => root.closest(".panes.static") || showPane(root, location.hash.slice(1)));
    document.querySelectorAll(".tree li[data-id]").forEach((li) => {
      const v = store.get("tree:" + li.dataset.id);
      if (v) li.classList.toggle("closed", v === "closed");
    });
    syncSelection();
  };
  document.addEventListener("DOMContentLoaded", () => {
    restore();
    syncSSO();
    const d = location.hash && document.querySelector("dialog" + location.hash);
    if (d) d.showModal();
  });
  document.addEventListener("htmx:afterSettle", restore);
})();
