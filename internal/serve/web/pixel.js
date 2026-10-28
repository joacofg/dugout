// The pixel office: the views client.js reads, drawn as an office where every
// workstream is an agent at a desk. What changed between two views starts an
// event on the scene; every text from the daemons goes into the desk labels
// as text, never as markup. Nothing here sends anything.
"use strict";

(() => {
  const art = window.PixelArt, office = window.DugoutOffice;

  // The scene's palette: pixels only, never text.
  const P = {
    wall: "#e4ebe6", wallTop: "#d6dfd9", base: "#bfcac3", frame: "#c3cec7", sky: "#d4ebf5", sky2: "#bfe0ef",
    floor: "#eadcc6", seam: "#dccbb0", shade: "#d3bf9f",
    top: "#b88a5f", edge: "#a07450", front: "#8c6342", chair: "#46505b", chairD: "#363e47",
    lid: "#9aa3ab", lidD: "#7f8890", logo: "#c9cfd4",
    paper: "#ffffff", paperEdge: "#c9cfcb", ink: "#9aa39e",
    accent: "#1b6c80", trail: "#5f8f9b", planeEdge: "#0f4352", alarm: "#d8322f", alarmDim: "#8f2a26", beaconBase: "#3c3f44",
    alarmTint: "#f0a59b", pole: "#3c3f44", bulb: "#f3c64a", bulbD: "#8a8f94", pot: "#b5714a", potD: "#93583a", leaf: "#4f8a5b", leafD: "#3d7049",
  };
  // Shirts are identity, assigned in the order people appear sorted by name.
  const SHIRTS = [["#4a78b8", "#3a6299"], ["#d9894a", "#b86f36"], ["#5a9a5e", "#477c4a"], ["#c4617d", "#a34d66"], ["#3f9a8c", "#317d71"], ["#9a9a42", "#7d7d33"]];
  // The grid, in scene pixels: a cell per desk, the desk part of it, the wall
  // band above the floor and the outer margin.
  const CELLW = 104, DESKH = 34, WALL = 34, M = 6;
  const MAX_FLIGHTS = 6, FLIGHT_MS = 1600, ENVELOPE_MS = 5000, STILL_MS = 7000, BULB_MS = 3500, TONE_MS = 3000, BLINK_MS = 420;
  // Label sizes per scene pixel, and the least any label text may be.
  const FS = { name: 4.8, branch: 4.0, intent: 4.2, meta: 3.8 }, FS_MIN = 14;

  const $ = (id) => document.getElementById(id);
  const main = $("main"), scene = $("scene"), canvas = $("floor"), sign = $("finding");
  const st = art.stage(canvas);
  const motion = window.matchMedia("(prefers-reduced-motion: reduce)");
  let reduced = motion.matches;

  let view = null, repo = null, repoName = "", chosen = office.repoParam();
  let stale = false, failed = false;
  let desks = new Map(), looks = new Map(), focusedDesk = "";
  // toned holds until when each changed desk's label keeps its tone, so a
  // view that follows soon after does not wipe it.
  const flights = [], bulbs = new Map(), toned = new Map();
  let frames = 0;

  // ---- small DOM helpers ------------------------------------------------------

  function h(tag, cls, text) {
    const el = document.createElement(tag);
    if (cls) el.className = cls;
    if (text !== undefined) el.textContent = text;
    return el;
  }
  const fileName = (p) => p.split("/").pop();
  const count = (n, one, many) => n + " " + (n === 1 ? one : many || one + "s");
  function shortRepo(name) {
    const parts = name.split("/");
    return parts.length > 2 ? parts.slice(-2).join("/") : name;
  }
  const EMPTY = { repo: "", workstreams: [], shared: [], conflicts: [], messages: [], escalations: [] };
  const byID = (id) => repo.workstreams.find((w) => w.id === id);
  const personOf = (id) => { const w = byID(id); return w ? (w.person || w.branch) : "someone whose work ended"; };
  const branchOf = (id) => { const w = byID(id); return w ? w.branch : "a workstream that ended"; };
  const escKey = (e) => e.at + e.conflicts.join();

  // ---- what changed between two views of one repo -----------------------------------

  // diffViews compares two views of the same repo; prev is null on the first
  // view of a repo, so a page that opens, reconnects or switches repo
  // replays nothing.
  function diffViews(prev, next) {
    const out = { messages: [], intents: new Set(), joined: new Set(), escalations: [] };
    if (!prev) return out;
    const sent = new Set(prev.messages.map((m) => m.at + m.from + m.to));
    out.messages = next.messages.filter((m) => !sent.has(m.at + m.from + m.to));
    const was = new Map(prev.workstreams.map((w) => [w.id, w]));
    for (const w of next.workstreams) {
      if (!was.has(w.id)) out.joined.add(w.id);
      else if (was.get(w.id).intent !== w.intent) out.intents.add(w.id);
    }
    const open = new Set(prev.escalations.filter((e) => e.open).map(escKey));
    out.escalations = next.escalations.filter((e) => e.open && !open.has(escKey(e)));
    return out;
  }

  // ---- layout ----------------------------------------------------------------------

  const fontPx = (k, s) => Math.max(FS_MIN, Math.round(k * s));
  // cellHeight is a cell's height in scene pixels at scale s: the desk, then
  // room for a label's four lines (two of intent) at that scale's sizes.
  function cellHeight(s) {
    const px = (fontPx(FS.name, s) + fontPx(FS.branch, s) + 2 * fontPx(FS.intent, s) + fontPx(FS.meta, s)) * 1.4 + 18;
    return DESKH + Math.ceil(px / s);
  }
  let grid = { cols: 1, rows: 1, s: 1 };
  // place puts the desks on rows cellH scene pixels tall and sizes the canvas.
  function place(cellH) {
    const { cols, rows, s } = grid, n = repo.workstreams.length;
    grid.cellH = cellH;
    st.size(cols * CELLW + 2 * M, WALL + rows * cellH + 4, s);
    scene.style.width = st.W * s + "px";
    desks = new Map();
    repo.workstreams.forEach((w, i) => {
      const row = Math.floor(i / cols), inRow = Math.min(cols, n - row * cols), col = i % cols;
      desks.set(w.id, { cx: Math.round(M + (cols - inRow) * CELLW / 2 + col * CELLW + CELLW / 2), top: WALL + row * cellH, i });
    });
  }
  // fit grows the rows when a label came out taller than its room (a long
  // meta line wraps on a narrow desk), then moves the labels with them.
  function fit() {
    const tags = [...scene.querySelectorAll(".tag")];
    const tallest = Math.max(0, ...tags.map((t) => t.offsetHeight));
    const need = DESKH + Math.ceil(tallest / grid.s) + 2;
    if (need <= grid.cellH) return;
    place(need);
    const oy = canvas.offsetTop;
    for (const t of tags) t.style.top = oy + (desks.get(t.dataset.ws).top + DESKH) * grid.s + "px";
  }
  function layout() {
    const n = repo.workstreams.length;
    const cs = getComputedStyle(main);
    const availW = main.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
    const availH = window.innerHeight - (scene.getBoundingClientRect().top + window.scrollY) - 64;
    const want = n === 0 ? 3 : n <= 3 ? n : n === 4 ? 2 : n <= 6 ? 3 : 4;
    // The most columns of the grid that still draw each pixel three screen
    // pixels wide; a phone gets one column at two.
    let cols = 1, sW = 2;
    for (let c = want; c >= 1; c--) {
      const k = Math.floor(availW / (c * CELLW + 2 * M));
      if (k >= 3 || c === 1) { cols = c; sW = Math.max(2, k); break; }
    }
    const rows = Math.max(1, Math.ceil(n / cols));
    const height = (s) => WALL + rows * cellHeight(s) + 4;
    let s = Math.min(sW, 6);
    while (s > 3 && height(s) * s > availH) s--;
    grid = { cols, rows, s };
    place(cellHeight(s));
    scene.style.setProperty("--fs-name", fontPx(FS.name, s) + "px");
    scene.style.setProperty("--fs-branch", fontPx(FS.branch, s) + "px");
    scene.style.setProperty("--fs-intent", fontPx(FS.intent, s) + "px");
    scene.style.setProperty("--fs-meta", fontPx(FS.meta, s) + "px");
    scene.style.setProperty("--fs-sign", Math.max(16, 5 * s) + "px");
  }

  // ---- labels, the sign and the words ---------------------------------------------

  function labels() {
    const active = document.activeElement;
    const keep = active && active.classList && active.classList.contains("tag") ? active.dataset.ws : "";
    for (const el of scene.querySelectorAll(".tag")) el.remove();
    // On a phone the sign stands above the canvas: labels follow the canvas.
    const s = st.s, oy = canvas.offsetTop;
    for (const w of repo.workstreams) {
      const d = desks.get(w.id);
      const tag = h("div", "tag");
      tag.tabIndex = 0;
      tag.dataset.ws = w.id;
      tag.setAttribute("role", "group");
      tag.setAttribute("aria-label", (w.person || "No name set") + ", " + w.branch);
      tag.style.left = (d.cx - CELLW / 2 + 2) * s + "px";
      tag.style.top = oy + (d.top + DESKH) * s + "px";
      tag.style.width = (CELLW - 4) * s + "px";
      tag.append(h("p", "who" + (w.person ? "" : " muted"), w.person || "No name set"), h("p", "branch", w.branch),
        h("p", "intent" + (w.intent ? "" : " muted"), w.intent || "No intent declared"));
      const files = w.files || [];
      const meta = h("p", "meta", count(files.length, "file") + " in flight");
      for (const c of repo.conflicts.filter((c) => c.sides.includes(w.id))) {
        meta.append(" · ", c.owner === w.id ? h("span", "keeps", "Keeps " + fileName(c.path)) : h("span", null, "Aligns on " + fileName(c.path)));
      }
      tag.append(meta);
      const pick = () => { focusedDesk = w.id; redraw(); };
      const drop = () => { if (focusedDesk === w.id) { focusedDesk = ""; redraw(); } };
      tag.addEventListener("focus", pick); tag.addEventListener("blur", drop);
      tag.addEventListener("mouseenter", pick); tag.addEventListener("mouseleave", drop);
      const left = (toned.get(w.id) || 0) - performance.now();
      if (left > 0) { tag.classList.add("changed"); setTimeout(() => tag.classList.remove("changed"), left); }
      scene.append(tag);
    }
    if (keep) {
      const el = [...scene.querySelectorAll(".tag")].find((t) => t.dataset.ws === keep);
      if (el) el.focus({ preventScroll: true });
    }
    if (focusedDesk && !desks.has(focusedDesk)) focusedDesk = "";
  }

  // finding fills the sign on the wall: what the office needs, read first.
  function finding() {
    const open = repo.escalations.filter((e) => e.open).sort((a, b) => a.at.localeCompare(b.at));
    const line = (cls) => h("p", "line" + (cls ? " " + cls : ""));
    sign.replaceChildren();
    sign.hidden = false;
    sign.style.top = 5 * st.s + "px";
    if (open.length) {
      const e = open[0];
      const first = line();
      first.append(h("b", null, "Waiting for a human: " + personOf(e.sides[0]) + " and " + personOf(e.sides[1])), " · ", h("span", "mono", e.paths.join(", ")));
      if (open.length > 1) first.append(" and " + (open.length - 1) + " more");
      sign.className = "finding esc";
      sign.append(first, h("p", "line why", e.reason || escalationBy(e)));
    } else if (repo.conflicts.length) {
      const first = line();
      first.append(h("b", null, count(repo.conflicts.length, "span") + " in conflict"), " ", h("span", "mono", [...new Set(repo.conflicts.map((c) => fileName(c.path)))].join(", ")));
      sign.className = "finding spans";
      sign.append(first);
    } else {
      sign.className = "finding";
      sign.append(h("p", "line", !repo.workstreams.length ? "Nobody is at a desk" : repo.shared.length ? "No conflicts: shared files merge cleanly" : "Calm: nobody shares a file"));
    }
    scene.append(sign);
  }
  function escalationBy(e) {
    switch (e.trigger) {
      case "agent": return "Asked by the agent on " + branchOf(e.sides[0]);
      case "disputed": return "Escalated by Dugout: both branches claim the span";
      default: return "Escalated by Dugout: the conflict still stood after the delay";
    }
  }
  function describe() {
    const open = repo.escalations.filter((e) => e.open).length;
    const parts = [count(repo.workstreams.length, "agent") + " at their desks"];
    if (repo.conflicts.length) parts.push(count(repo.conflicts.length, "span") + " in conflict");
    if (open) parts.push(open + " waiting for a human");
    canvas.setAttribute("aria-label", "The office floor: " + parts.join(", "));
  }
  function say(text) {
    $("announce").textContent = text;
  }
  function announce(d) {
    const out = [];
    if (d.messages.length === 1) out.push(branchOf(d.messages[0].from) + " messaged " + branchOf(d.messages[0].to));
    else if (d.messages.length > 1) out.push(d.messages.length + " messages between agents");
    for (const id of d.intents) { const w = byID(id); out.push(personOf(id) + " on " + w.branch + ": " + (w.intent || "no intent")); }
    for (const e of d.escalations) out.push("Escalation: waiting for a human on " + e.paths.join(", "));
    if (out.length) say(out.join(". "));
  }

  // ---- the repo shown ----------------------------------------------------------------

  function nav(v) {
    const pick = $("repo-select");
    pick.parentElement.hidden = v.repos.length < 2;
    pick.replaceChildren(...v.repos.map((r) => { const o = h("option", null, shortRepo(r.repo)); o.value = r.repo; o.selected = r.repo === repoName; return o; }));
    pick.onchange = () => { chosen = pick.value; office.setRepoParam(chosen); if (view) render(view); };
    $("crumb-repo").textContent = repoName ? shortRepo(repoName) : "";
    $("to-clear").href = "/office" + (repoName ? "?repo=" + encodeURIComponent(repoName) : "");
  }

  // render draws view v, starting the events that make it differ from the
  // last view of the same repo.
  function render(v) {
    if (failed) return;
    view = v;
    const name = office.pickRepo(v, chosen);
    const prev = name && name === repoName ? repo : null;
    repoName = name;
    repo = v.repos.find((r) => r.repo === name) || EMPTY;
    nav(v);
    $("title").textContent = name ? shortRepo(name) : "Pixel office";
    $("title").title = name;
    $("sub").textContent = name
      ? count(repo.workstreams.length, "agent") + " at their desks · " + count(new Set(repo.workstreams.map((w) => w.person || w.id)).size, "person", "people")
      : "Nobody is working in a repo you can see.";
    const d = diffViews(prev, repo);
    const now = performance.now();
    for (const m of d.messages) flights.push({ from: m.from, to: m.to, start: now });
    flights.splice(0, Math.max(0, flights.length - MAX_FLIGHTS));
    for (const id of d.intents) bulbs.set(id, now);
    if (!prev) { flights.length = 0; bulbs.clear(); }
    people();
    layout();
    if (!prev) toned.clear();
    for (const id of [...d.intents, ...d.joined]) toned.set(id, now + TONE_MS);
    finding();
    labels();
    fit();
    describe();
    announce(d);
    redraw();
  }
  function people() {
    const names = [...new Set(repo.workstreams.map((w) => w.person || w.branch))].sort();
    looks = new Map(names.map((n, i) => [n, art.look(n, SHIRTS[i % SHIRTS.length])]));
  }

  // ---- the scene --------------------------------------------------------------------

  function room(g) {
    const { W, H } = st;
    art.rect(g, 0, 0, W, H, P.floor);
    for (let y = WALL + 5; y < H; y += 6) art.rect(g, 0, y, W, 1, P.seam);
    for (let y = WALL, k = 0; y < H; y += 6, k++) for (let x = (k % 2) * 13; x < W; x += 26) art.rect(g, x, y, 1, 6, P.seam);
    art.rect(g, 0, 0, W, WALL - 4, P.wall); art.rect(g, 0, 0, W, 2, P.wallTop);
    art.rect(g, 0, WALL - 4, W, 2, P.base); art.rect(g, 0, WALL - 2, W, 2, P.shade);
    for (let x = 24; x + 34 < W; x += 72) {
      art.rect(g, x, 6, 34, 18, P.frame); art.rect(g, x + 2, 8, 14, 14, P.sky); art.rect(g, x + 18, 8, 14, 14, P.sky);
      art.rect(g, x + 2, 8, 14, 3, P.sky2); art.rect(g, x + 18, 8, 14, 3, P.sky2);
    }
    plant(g, 4, WALL - 14); plant(g, W - 14, WALL - 14);
  }
  function plant(g, x, y) {
    art.rect(g, x + 2, y + 8, 7, 6, P.pot); art.rect(g, x + 2, y + 13, 7, 1, P.potD);
    for (const [dx, dy, c] of [[4, 0, P.leaf], [2, 2, P.leafD], [6, 1, P.leaf], [1, 4, P.leaf], [8, 3, P.leafD], [4, 3, P.leafD], [5, 5, P.leaf], [3, 6, P.leaf], [7, 6, P.leafD]])
      art.rect(g, x + dx, y + dy, 2, 2, c);
  }
  // trail draws the dotted path a message flies, a dash every fourth pixel,
  // up to p of the way.
  function trail(g, x0, y0, x1, y1, hh, p) {
    const n = Math.max(1, Math.round(Math.abs(x1 - x0) / 4));
    for (let i = 0; i <= n * p; i++) { const [x, y] = art.arcAt(x0, y0, x1, y1, hh, i / n); art.rect(g, x, y, 2, 1, P.trail); }
  }
  // still is whether idle motion (typing, blinking, the beacon) holds still:
  // under reduced motion, and while the stream reconnects, so the scene does
  // not look live when it is not.
  const still = () => reduced || stale;

  function draw(t) {
    const g = st.g;
    room(g);
    const now = performance.now();
    const alarmed = new Set(repo.escalations.filter((e) => e.open).flatMap((e) => e.sides).filter((id) => desks.has(id)));
    const lit = still() || Math.floor(t / BLINK_MS) % 2 === 0;
    for (const id of alarmed) {
      const d = desks.get(id);
      art.checker(g, d.cx - CELLW / 2 + 4, d.top + 2, CELLW - 8, DESKH - 1, P.alarmTint, still() ? 0 : Math.floor(t / BLINK_MS) % 2);
    }
    if (focusedDesk && desks.has(focusedDesk)) {
      const d = desks.get(focusedDesk), x = d.cx - CELLW / 2 + 2, y = d.top + 1, w = CELLW - 4, hh = DESKH - 1;
      art.rect(g, x, y, w, 1, P.accent); art.rect(g, x, y + hh - 1, w, 1, P.accent); art.rect(g, x, y, 1, hh, P.accent); art.rect(g, x + w - 1, y, 1, hh, P.accent);
    }
    const flags = new Map();
    for (const w of repo.workstreams) {
      const d = desks.get(w.id), x = d.cx, y = d.top;
      const phase = (art.hash(w.id) % 7) * 90;
      art.rect(g, x - 7, y + 9, 14, 7, P.chair); art.rect(g, x - 7, y + 15, 14, 1, P.chairD);
      art.person(g, x - 8, y + 4, looks.get(w.person || w.branch), {
        frame: still() ? 0 : Math.floor((t + phase) / 260), blink: !still() && (t + phase * 11) % 4200 < 140, handUp: alarmed.has(w.id) });
      art.rect(g, x - 17, y + 19, 34, 3, P.top); art.rect(g, x - 17, y + 22, 34, 1, P.edge); art.rect(g, x - 16, y + 23, 32, 9, P.front); art.rect(g, x - 16, y + 32, 32, 1, P.shade);
      art.rect(g, x - 4, y + 14, 8, 6, P.lid); art.rect(g, x - 4, y + 19, 8, 1, P.lidD); art.dot(g, x, y + 16, P.logo);
      const mine = repo.conflicts.filter((c) => c.sides.includes(w.id));
      art.papers(g, x - 15, y + 17, (w.files || []).length, P, mine.length ? P.accent : null);
      mine.forEach((c, k) => {
        art.pennant(g, x + 7 + k * 6, y + 19, P.accent, c.owner === w.id, P.pole);
        flags.set(c.id + w.id, { x: x + 7 + k * 6, y: y + 10 });
      });
      if (alarmed.has(w.id)) art.beacon(g, x + 9 + mine.length * 6, y + 12, lit, P);
    }
    // Each cord rises by how far it reaches, so two cords never share a path.
    const reach = (c) => { const a = desks.get(c.sides[0]), b = desks.get(c.sides[1]); return a && b ? Math.abs(a.cx - b.cx) + Math.abs(a.top - b.top) : 0; };
    [...repo.conflicts].sort((x, y) => reach(x) - reach(y)).forEach((c, k) => {
      const a = flags.get(c.id + c.sides[0]), b = flags.get(c.id + c.sides[1]);
      // A cord that passes over another desk clears its head.
      if (a && b) art.arc(g, a.x, a.y, b.x, b.y, (Math.abs(a.x - b.x) > CELLW * 1.2 ? 13 : 5) + 7 * k, P.accent, 2);
    });
    for (let i = flights.length - 1; i >= 0; i--) {
      const f = flights[i], a = desks.get(f.from), b = desks.get(f.to), age = now - f.start;
      if (!a || !b || age > (reduced ? STILL_MS : FLIGHT_MS + ENVELOPE_MS)) { flights.splice(i, 1); continue; }
      const x0 = a.cx, y0 = a.top + 2, x1 = b.cx + 8, y1 = b.top + 15, hh = 16;
      const p = reduced ? 1 : Math.min(1, age / FLIGHT_MS);
      trail(g, x0, y0, x1, y1, hh, p);
      if (p < 1) { const [px, py] = art.arcAt(x0, y0, x1, y1, hh, p); art.plane(g, px, py, x1 - x0, "#ffffff", P.planeEdge); }
      else art.envelope(g, x1 - 3, y1 - 3, "#ffffff", P.accent);
    }
    for (const [id, since] of bulbs) {
      const d = desks.get(id);
      if (!d || now - since > BULB_MS) { bulbs.delete(id); continue; }
      art.bulb(g, d.cx - 2, d.top - 5, reduced || Math.floor((now - since) / 300) % 2 === 0, P);
    }
    st.present();
    frames++;
    scene.dataset.frames = String(frames);
    scene.dataset.flights = String(flights.length);
  }

  // ---- when to draw ------------------------------------------------------------------

  // The frame loop runs only while something on the scene moves; under
  // reduced motion nothing moves, and a one-second timer clears the still
  // events once their time is up.
  // Idle motion changes every 260 ms at the quickest, so an office with
  // nothing in flight draws about eight frames a second, not sixty.
  const IDLE_FRAME_MS = 120;
  let looping = false, stillTimer = 0, lastFrame = 0;
  const events = () => flights.length > 0 || bulbs.size > 0;
  const moving = () => !reduced && !document.hidden && (!stale || events());
  function tick(t) {
    if (!repo || !moving()) { looping = false; return; }
    if (flights.length || t - lastFrame >= IDLE_FRAME_MS) { draw(t); lastFrame = t; }
    requestAnimationFrame(tick);
  }
  function redraw() {
    if (!repo) return;
    if (moving()) {
      if (!looping) { looping = true; requestAnimationFrame(tick); }
      return;
    }
    draw(performance.now());
    clearTimeout(stillTimer);
    if (events()) stillTimer = setTimeout(redraw, 1000);
  }
  motion.addEventListener("change", (e) => { reduced = e.matches; redraw(); });
  document.addEventListener("visibilitychange", redraw);
  let resizeTimer = 0;
  window.addEventListener("resize", () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => { if (repo && !failed) { layout(); finding(); labels(); fit(); redraw(); } }, 100);
  });
  if (document.fonts) document.fonts.ready.then(() => { if (repo && !failed) { labels(); fit(); redraw(); } });

  // ---- the legend and the connection ----------------------------------------------------

  function legend() {
    const el = $("legend");
    const item = (paint, text) => {
      const c = document.createElement("canvas");
      const s = art.stage(c);
      s.size(14, 13, 3); paint(s.g); s.present();
      const sp = h("span"); sp.append(c, text); el.append(sp);
    };
    item((g) => art.pennant(g, 4, 11, P.accent, true, P.pole), "keeps the span");
    item((g) => art.pennant(g, 4, 11, P.accent, false, P.pole), "aligns to it");
    item((g) => art.plane(g, 7, 6, 1, "#ffffff", P.planeEdge), "a message");
    item((g) => art.beacon(g, 3, 4, true, P), "waiting for a human");
  }

  let lastState = "";
  function setStatus(state, text) {
    $("status").dataset.state = state;
    $("status-text").textContent = text;
    if (state !== lastState) say(text);
    lastState = state;
    const was = stale;
    stale = state === "stale";
    if (was !== stale) redraw();
  }
  function fail(title, line) {
    failed = true;
    setStatus("down", title);
    const block = h("div", "state");
    block.append(h("h1", null, title), h("p", null, line));
    main.replaceChildren(block);
  }

  legend();
  office.connect({ onView: render, onStatus: setStatus, onFail: fail });
})();
