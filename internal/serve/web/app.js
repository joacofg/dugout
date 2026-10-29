// The office view: it draws the views client.js reads from serve's office
// stream, and sets every text from the daemons as text, never as markup.
"use strict";

(() => {
  const TINTS = ["#dfe9f7", "#f6e3d3", "#dcefdc", "#f5dde3", "#d6efea", "#e8f0d2"];
  const MAX_MESSAGES = 12;
  const MAX_LANES = 6;
  const CHANGE_MS = 2000;

  // ---- the repo, from the address ----------------------------------------

  let chosenRepo = DugoutOffice.repoParam();

  function setRepo(repo) {
    chosenRepo = repo;
    DugoutOffice.setRepoParam(repo);
    selected = "";
    if (view) render(view, false);
    document.getElementById("main").focus();
  }

  // ---- small DOM helpers ------------------------------------------------

  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") el.className = v;
      else if (k === "text") el.textContent = v;
      else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? "" : String(v));
    }
    for (const kid of kids.flat()) {
      if (kid === undefined || kid === null || kid === false) continue;
      el.append(typeof kid === "string" ? document.createTextNode(kid) : kid);
    }
    return el;
  }
  function svg(tag, attrs) {
    const el = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, String(v));
    return el;
  }
  const mono = (s) => h("span", { class: "mono", text: s });
  // tinted sets a person's tint through the CSSOM: the CSP allows no style
  // attribute.
  const tinted = (el, color) => { el.style.backgroundColor = color || "#e6ebe8"; return el; };
  const fileName = (p) => p.split("/").pop();

  // ---- time -------------------------------------------------------------

  let serverSkew = 0; // serve's clock minus ours, from each view's "at"
  const clock = DugoutOffice.clock;
  function ago(iso) {
    const m = Math.round((Date.now() + serverSkew - Date.parse(iso)) / 60000);
    return m <= 0 ? "now" : m === 1 ? "1 min ago" : m < 60 ? m + " min ago" : "at " + clock(iso);
  }
  function timeEl(iso, text) {
    return h("time", { datetime: iso, title: new Date(iso).toLocaleString(), text });
  }

  // ---- what the view says, in words ---------------------------------------

  function people(repo) {
    const names = [...new Set(repo.workstreams.map((w) => w.person || ""))].filter(Boolean).sort();
    return new Map(names.map((n, i) => [n, TINTS[i % TINTS.length]]));
  }
  function wsOf(repo) {
    return new Map(repo.workstreams.map((w) => [w.id, w]));
  }
  function branchOf(byID, id) {
    const w = byID.get(id);
    return w ? w.branch : "a workstream that ended";
  }
  function personOf(byID, id) {
    const w = byID.get(id);
    return w ? (w.person || w.branch) : "someone whose work ended";
  }
  function initials(name) {
    const parts = (name || "?").trim().split(/\s+/);
    return parts.map((p) => [...p][0] || "").join("").slice(0, 2).toUpperCase() || "?";
  }
  function lines(ranges) {
    if (!ranges || ranges.length === 0) return "the whole file";
    const r = ranges[0];
    const first = r.len <= 0 ? "before line " + r.start : r.len === 1 ? "line " + r.start : "lines " + r.start + "–" + (r.start + r.len - 1);
    return ranges.length > 1 ? first + " and " + (ranges.length - 1) + " more" : first;
  }
  function why(span, byID) {
    const owner = branchOf(byID, span.owner);
    const other = branchOf(byID, span.sides[0] === span.owner ? span.sides[1] : span.sides[0]);
    switch (span.why) {
      case "claimed": return owner + " claimed it";
      case "yielded": return other + " yielded it";
      case "disputed": return "both said the same: Dugout's rule decides";
      default: return "Dugout's rule: no one claimed it";
    }
  }
  function escalationBy(e, byID) {
    switch (e.trigger) {
      case "agent": return "Asked by the agent on " + branchOf(byID, e.sides[0]);
      case "disputed": return "Escalated by Dugout: both branches claim the span";
      default: return "Escalated by Dugout: the conflict still stood after the delay";
    }
  }

  // ---- render -------------------------------------------------------------

  let view = null;
  let selected = "";
  let seen = null; // signatures of what the last render showed, by key
  const announced = new Set();

  const pickRepo = (v) => DugoutOffice.pickRepo(v, chosenRepo);

  function renderNav(v, current) {
    const list = document.getElementById("repos");
    const select = document.getElementById("repo-select");
    list.replaceChildren(...v.repos.map((r) => h("li", null, h("a", {
      href: "?repo=" + encodeURIComponent(r.repo), "data-repo": r.repo, "aria-current": r.repo === current ? "page" : null,
      onclick: (ev) => { ev.preventDefault(); setRepo(r.repo); },
    }, h("span", { text: shortRepo(r.repo), title: r.repo }), h("span", { class: "n", text: String(r.workstreams.length) })))));
    select.replaceChildren(...v.repos.map((r) => h("option", { value: r.repo, selected: r.repo === current, text: shortRepo(r.repo) })));
    select.onchange = () => setRepo(select.value);
    document.getElementById("crumb-repo").textContent = current ? shortRepo(current) : "";
  }
  function shortRepo(repo) {
    const parts = repo.split("/");
    return parts.length > 2 ? parts.slice(-2).join("/") : repo;
  }

  function render(v, live) {
    view = v;
    const main = document.getElementById("main");
    // A live update must not take the keyboard focus from the row, repo link
    // or repo picker it was on.
    const active = document.activeElement;
    const focused = active && active.dataset ? active.dataset.ws || "" : "";
    const focusedRepo = active && active.dataset ? active.dataset.repo || "" : "";
    const pickerFocused = active === document.getElementById("repo-select");
    const current = pickRepo(v);
    renderNav(v, current);
    document.getElementById("to-pixel").href = "/office/pixel" + (current ? "?repo=" + encodeURIComponent(current) : "");
    if (focusedRepo) {
      const link = [...document.querySelectorAll("#repos a")].find((a) => a.dataset.repo === focusedRepo);
      if (link) link.focus({ preventScroll: true });
    } else if (pickerFocused) {
      document.getElementById("repo-select").focus({ preventScroll: true });
    }
    if (!current) {
      seen = new Map();
      main.replaceChildren(stateBlock("Nobody is working in a repo you can see.", "Workstreams appear here as soon as an agent has a file in flight."));
      return;
    }
    const repo = v.repos.find((r) => r.repo === current);
    const byID = wsOf(repo);
    const tints = people(repo);
    const now = new Map();
    const mark = (el, key, sig) => {
      now.set(key, sig);
      if (live && seen && seen.get(key) !== sig) {
        el.classList.add("changed");
        setTimeout(() => el.classList.remove("changed"), CHANGE_MS);
      }
      return el;
    };
    const mine = (ids) => !selected || ids.includes(selected);

    const open = repo.escalations.filter((e) => e.open);
    const office = h("div", { class: "office" + (selected ? " picked" : "") },
      titleRow(repo, open),
      finding(repo, open, byID),
      workstreams(repo, byID, tints, mark),
      h("div", { class: "cols sec" },
        h("div", null, spans(repo, byID, mark, mine), escalations(repo, byID, mark, mine)),
        messages(repo, byID, mark, mine)));
    main.replaceChildren(office);
    seen = now;
    layoutLinks(repo);
    if (focused) focusRow(focused);
    for (const e of open) {
      const key = e.at + e.conflicts.join();
      if (live && !announced.has(key)) say("Escalation: " + personOf(byID, e.sides[0]) + " and " + personOf(byID, e.sides[1]) + ", " + e.paths.join(", "));
      announced.add(key);
    }
  }

  function stateBlock(title, line) {
    return h("div", { class: "state" }, h("h1", { text: title }), line ? h("p", { text: line }) : null);
  }

  function titleRow(repo, open) {
    const people = new Set(repo.workstreams.map((w) => w.person || w.id)).size;
    return h("div", null,
      h("div", { class: "titlerow" }, h("h1", { text: shortRepo(repo.repo), title: repo.repo }),
        open.length ? h("span", { class: "pill neg", text: open.length + (open.length === 1 ? " escalation open" : " escalations open") }) : null,
        repo.conflicts.length ? h("span", { class: "pill neu", text: repo.conflicts.length + (repo.conflicts.length === 1 ? " span in conflict" : " spans in conflict") }) : null),
      h("p", { class: "sub", text: [count(repo.workstreams.length, "workstream"), count(people, "person", "people"),
        count(new Set(repo.shared.map((s) => s.path)).size, "shared file")].join(" · ") }));
  }
  function count(n, one, many) { return n + " " + (n === 1 ? one : many || one + "s"); }

  function finding(repo, open, byID) {
    if (open.length) {
      const e = [...open].sort((a, b) => a.at.localeCompare(b.at))[0];
      return h("div", { class: "finding esc" },
        h("b", { text: "Waiting for " + personOf(byID, e.sides[0]) + " and " + personOf(byID, e.sides[1]) }),
        h("span", { class: "why" }, mono(e.paths.join(", ")), ": " + (e.reason || escalationBy(e, byID))));
    }
    if (repo.conflicts.length) {
      return h("div", { class: "finding spans" },
        h("b", { text: count(repo.conflicts.length, "span") + " in conflict" }),
        h("span", { class: "why" }, ...[...new Set(repo.conflicts.map((c) => c.path))].flatMap((p, i) => [i ? ", " : "", mono(p)])));
    }
    return h("div", { class: "finding calm", text: repo.shared.length ? "No conflicts: shared files merge cleanly" : "Calm: nobody shares a file" });
  }

  function workstreams(repo, byID, tints, mark) {
    const head = h("div", { class: "sechead" }, h("h2", { id: "h-ws", text: "Who works on what" }),
      repo.conflicts.length ? h("span", { class: "legend", "aria-hidden": "true" }, h("span", null, dotSVG(true), "keeps"), h("span", null, dotSVG(false), "aligns")) : null);
    if (!repo.workstreams.length) {
      return h("section", { class: "sec", "aria-labelledby": "h-ws" }, head, h("p", { class: "empty", text: "Nobody is working in " + shortRepo(repo.repo) + " right now." }));
    }
    const shared = new Set(repo.shared.map((s) => s.path));
    const rows = repo.workstreams.map((w) => {
      w = { ...w, files: w.files || [] };
      const roles = repo.conflicts.filter((c) => c.sides.includes(w.id));
      const sh = w.files.filter((f) => shared.has(f));
      const toggle = () => { selected = selected === w.id ? "" : w.id; render(view, false); focusRow(w.id); };
      const row = h("div", { class: "row", role: "button", tabindex: "0", "data-ws": w.id, "aria-pressed": selected === w.id ? "true" : "false",
        onclick: toggle,
        onkeydown: (ev) => {
          if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); toggle(); }
          else if (ev.key === "Escape" && selected) { selected = ""; render(view, false); focusRow(w.id); }
        } },
        h("div", { class: "who" }, tinted(h("span", { class: "av", "aria-hidden": "true", text: initials(w.person || w.branch) }), tints.get(w.person)),
          h("span", { text: w.person || "No name set", class: w.person ? null : "muted" })),
        h("div", { class: "mono", text: w.branch, title: w.branch }),
        h("div", { class: "intentcell" },
          h("p", { class: "intent" + (w.intent ? "" : " muted"), text: w.intent || "No intent declared" }),
          h("div", { class: "files" }, count(w.files.length, "file") + " in flight",
            sh.length ? " · shares " : "", ...sh.flatMap((f, i) => [i ? ", " : "", h("span", { class: "mono", title: f, text: fileName(f) })]))),
        h("div", { class: "stance" }, roles.length ? roles.map((c) => h("div", null,
          c.owner === w.id ? h("span", { class: "keeps", text: "Keeps " }) : h("span", { class: "yields", text: "Yields " }),
          h("span", { class: "mono", title: c.path, text: fileName(c.path) }))) : h("span", { class: "yields", text: "No conflict" })));
      return mark(row, "ws:" + w.id, JSON.stringify([w.branch, w.intent, w.files, w.person, roles.map((c) => [c.id, c.owner])]));
    });
    return h("section", { class: "sec", "aria-labelledby": "h-ws" }, head,
      h("div", { class: "floor" }, svg("svg", { class: "gutter", id: "gutter", "aria-hidden": "true" }), h("div", { class: "rows", id: "rows" }, rows)));
  }
  function focusRow(id) {
    const row = [...document.querySelectorAll(".row")].find((r) => r.dataset.ws === id);
    if (row) row.focus({ preventScroll: true });
  }
  // role reads a colour role from the stylesheet, so the SVG marks follow it.
  function role(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }
  function dotSVG(filled) {
    const s = svg("svg", { width: 10, height: 10, viewBox: "0 0 10 10" });
    s.append(svg("circle", filled ? { cx: 5, cy: 5, r: 4, fill: role("--accent") } : { cx: 5, cy: 5, r: 3.5, fill: role("--surface"), stroke: role("--accent"), "stroke-width": 1.5 }));
    return s;
  }

  // The span links: each conflict joins its two rows in the gutter, on the
  // leftmost lane free over the rows it spans.
  function layoutLinks(repo) {
    const g = document.getElementById("gutter");
    const rows = document.getElementById("rows");
    if (!g || !rows) return;
    const narrow = window.matchMedia("(max-width: 767px)").matches;
    const step = narrow ? 8 : 12;
    const order = repo.workstreams.map((w) => w.id);
    const spans = repo.conflicts
      .map((c) => ({ c, a: order.indexOf(c.sides[0]), b: order.indexOf(c.sides[1]) }))
      .filter((s) => s.a >= 0 && s.b >= 0)
      .map((s) => ({ ...s, lo: Math.min(s.a, s.b), hi: Math.max(s.a, s.b) }))
      .sort((x, y) => (x.hi - x.lo) - (y.hi - y.lo) || x.lo - y.lo);
    const lanes = [];
    for (const s of spans) {
      let lane = lanes.findIndex((l) => l.every((o) => o.hi < s.lo || o.lo > s.hi));
      if (lane < 0) { lane = lanes.length; lanes.push([]); }
      s.lane = lane;
      lanes[lane].push(s);
    }
    const used = Math.min(lanes.length, MAX_LANES);
    const width = narrow ? 32 : Math.max(32, 24 + step * used);
    g.closest(".floor").style.setProperty("--gutter", width + "px");
    const top = rows.getBoundingClientRect().top;
    const ys = [...rows.children].map((r) => { const b = r.getBoundingClientRect(); return b.top - top + b.height / 2; });
    g.setAttribute("viewBox", "0 0 " + width + " " + rows.offsetHeight);
    g.style.height = rows.offsetHeight + "px";
    g.replaceChildren();
    const accent = role("--accent"), surface = role("--surface");
    for (const s of spans) {
      if (s.lane >= MAX_LANES) continue;
      const x = width - 10 - s.lane * step;
      const ya = ys[s.a], yb = ys[s.b];
      const keeperY = s.c.owner === s.c.sides[0] ? ya : yb;
      const otherY = keeperY === ya ? yb : ya;
      const dim = selected && !s.c.sides.includes(selected);
      const grp = svg("g", { opacity: dim ? 0.3 : 1 });
      grp.append(svg("path", { d: "M" + width + " " + ya + " H" + x + " V" + yb + " H" + width, fill: "none", stroke: accent, "stroke-width": 1.5 }));
      grp.append(svg("circle", { cx: x, cy: keeperY, r: 5, fill: accent }));
      grp.append(svg("circle", { cx: x, cy: otherY, r: 4.5, fill: surface, stroke: accent, "stroke-width": 1.5 }));
      g.append(grp);
    }
  }

  function spans(repo, byID, mark, mine) {
    const head = h("div", { class: "sechead" }, h("h2", { id: "h-sp", text: "Who keeps each span" }));
    if (!repo.conflicts.length) return h("section", { "aria-labelledby": "h-sp" }, head, h("p", { class: "empty", text: "No spans in conflict." }));
    return h("section", { "aria-labelledby": "h-sp" }, head, h("ul", { class: "list" }, repo.conflicts.map((c) => {
      const i = c.sides.indexOf(c.owner);
      const other = c.sides[1 - i];
      const li = h("li", { class: "item" + (mine(c.sides) && selected ? " mine" : "") },
        h("div", { class: "line" }, mono(c.path), h("span", { class: "mono muted", text: c.id.slice(0, 8) })),
        h("p", null, h("b", { class: "w5", text: branchOf(byID, c.owner) }), " keeps " + lines(c.lines[i]) + "; " + branchOf(byID, other) + " aligns to them"),
        h("div", { class: "meta", text: why(c, byID) }));
      return mark(li, "span:" + c.id, c.owner + c.why + JSON.stringify(c.lines));
    })));
  }

  function escalations(repo, byID, mark, mine) {
    const head = h("div", { class: "sechead gap" }, h("h2", { id: "h-esc", text: "Escalations" }));
    if (!repo.escalations.length) return h("section", { "aria-labelledby": "h-esc" }, head, h("p", { class: "empty", text: "Nothing escalated." }));
    const list = [...repo.escalations].sort((a, b) => (b.open - a.open) || b.at.localeCompare(a.at));
    return h("section", { "aria-labelledby": "h-esc" }, head, h("ul", { class: "list" }, list.map((e) => {
      const li = h("li", { class: "item" + (e.open ? "" : " closed") + (mine(e.sides) && selected ? " mine" : "") },
        h("div", { class: "line" }, h("span", null, h("b", { class: "w6", text: personOf(byID, e.sides[0]) }), " and ",
          h("b", { class: "w6", text: personOf(byID, e.sides[1]) }), " · ", mono(e.paths.join(", "))),
          h("span", { class: "meta" }, e.urgent ? "Urgent · " : "", timeEl(e.at, ago(e.at)))),
        e.reason ? h("p", { text: e.reason }) : null,
        h("div", { class: "meta", text: escalationBy(e, byID) + (e.open ? " · waiting for a human" : " · settled") }));
      return mark(li, "esc:" + e.at + e.conflicts.join(), String(e.open));
    })));
  }

  function messages(repo, byID, mark, mine) {
    const head = h("div", { class: "sechead" }, h("h2", { id: "h-msg", text: "Messages between agents" }));
    if (!repo.messages.length) return h("section", { "aria-labelledby": "h-msg" }, head, h("p", { class: "empty", text: "No messages between agents yet." }));
    const newest = [...repo.messages].reverse();
    const shown = newest.slice(0, MAX_MESSAGES);
    return h("section", { "aria-labelledby": "h-msg" }, head,
      h("ul", { class: "list" }, shown.map((m) => mark(h("li", { class: "item msg" + (mine([m.from, m.to]) && selected ? " mine" : "") },
        h("span", { class: "muted" }, timeEl(m.at, clock(m.at))),
        h("span", null, mono(branchOf(byID, m.from)), " → ", mono(branchOf(byID, m.to)))), "msg:" + m.at + m.from + m.to, "1"))),
      newest.length > shown.length ? h("p", { class: "more", text: (newest.length - shown.length) + " earlier" }) : null);
  }

  // ---- connection -----------------------------------------------------------

  let lastLive = "";
  function setStatus(state, text) {
    const el = document.getElementById("status");
    el.dataset.state = state;
    document.getElementById("status-text").textContent = text;
    if (state !== lastLive) say(text);
    lastLive = state;
  }
  function say(text) {
    document.getElementById("announce").textContent = text;
  }
  function fail(title, line) {
    setStatus("down", title);
    document.getElementById("main").replaceChildren(stateBlock(title, line));
  }

  // Relative times move on, and the links follow the layout.
  setInterval(() => { if (view) for (const t of document.querySelectorAll("time")) if (!/^\d\d:\d\d$/.test(t.textContent)) t.textContent = ago(t.getAttribute("datetime")); }, 30000);
  let resizeTimer = 0;
  window.addEventListener("resize", () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => { if (view) layoutLinks(view.repos.find((r) => r.repo === pickRepo(view)) || { workstreams: [], conflicts: [] }); }, 100);
  });
  if (document.fonts) document.fonts.ready.then(() => { if (view) render(view, false); });

  DugoutOffice.connect({
    onView(v) {
      serverSkew = Date.parse(v.at) - Date.now();
      render(v, view !== null);
    },
    onStatus: setStatus,
    onFail: fail,
  });
})();
