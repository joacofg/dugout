// The pixel office's drawing kit: a low-resolution buffer scaled up by a
// whole number with square pixels, and every sprite drawn from flat
// rectangles in code, so the page ships no image file.
"use strict";

window.PixelArt = (() => {
  // stage keeps a W x H buffer and paints it onto canvas s times larger.
  function stage(canvas) {
    const buf = document.createElement("canvas");
    const g = buf.getContext("2d");
    const out = canvas.getContext("2d");
    const st = { canvas, g, W: 0, H: 0, s: 1,
      size(W, H, s) {
        st.W = W; st.H = H; st.s = s;
        buf.width = W; buf.height = H;
        // A whole number of device pixels per scene pixel keeps them square.
        const k = Math.max(1, Math.round(s * (window.devicePixelRatio || 1)));
        canvas.width = W * k; canvas.height = H * k;
        canvas.style.width = W * s + "px"; canvas.style.height = H * s + "px";
      },
      present() {
        out.imageSmoothingEnabled = false;
        out.clearRect(0, 0, canvas.width, canvas.height);
        out.drawImage(buf, 0, 0, canvas.width, canvas.height);
      } };
    return st;
  }

  const rect = (g, x, y, w, h, c) => { g.fillStyle = c; g.fillRect(Math.round(x), Math.round(y), w, h); };
  const dot = (g, x, y, c) => rect(g, x, y, 1, 1, c);
  // checker paints every other pixel of an area: a dithered tint, the pixel
  // artist's shading without a gradient.
  function checker(g, x, y, w, h, c, phase) {
    g.fillStyle = c;
    for (let j = 0; j < h; j++) for (let i = (j + (phase || 0)) % 2; i < w; i += 2) g.fillRect(x + i, y + j, 1, 1);
  }
  const bez = (a, c, b, t) => (1 - t) * (1 - t) * a + 2 * (1 - t) * t * c + t * t * b;
  function curve(g, x0, y0, cx, cy, x1, y1, col, upto) {
    const n = Math.ceil(Math.hypot(x1 - x0, y1 - y0) * 1.4);
    for (let i = 0; i <= n * (upto ?? 1); i++) dot(g, bez(x0, cx, x1, i / n), bez(y0, cy, y1, i / n), col);
  }
  // arc draws a parabola from (x0, y0) to (x1, y1) rising h pixels above the
  // higher end, thick pixels tall, stepped by x so it never doubles up; a
  // near-vertical one bows sideways instead.
  function arc(g, x0, y0, x1, y1, h, col, thick, upto) {
    if (Math.abs(x1 - x0) < 6) return curve(g, x0, y0, (x0 + x1) / 2 + 10, Math.min(y0, y1) - h, x1, y1, col, upto);
    const cy = Math.min(y0, y1) - h;
    const n = Math.abs(x1 - x0), sx = x1 > x0 ? 1 : -1;
    let prev = null;
    for (let i = 0; i <= n * (upto ?? 1); i++) {
      const y = Math.round(bez(y0, cy, y1, i / n));
      const top = prev === null ? y : Math.min(y, prev), bot = prev === null ? y : Math.max(y, prev);
      rect(g, x0 + sx * i, top, 1, bot - top + (thick || 1), col);
      prev = y;
    }
  }
  // arcAt is the point of arc's path at t in [0, 1].
  const arcAt = (x0, y0, x1, y1, h, t) => [x0 + (x1 - x0) * t, bez(y0, Math.min(y0, y1) - h, y1, t)];

  // ---- a stable figure per person ---------------------------------------------
  const SKIN = [["#f1c7a5", "#d9a57f"], ["#d9a07a", "#b97f5b"], ["#a8714f", "#8a5a3d"], ["#7a4e34", "#613c27"], ["#f5d6bd", "#e0b391"]];
  const HAIR = [["#2b2220", "#1b1514"], ["#5a3a22", "#432a18"], ["#b8862f", "#946a22"], ["#1f1f24", "#121215"], ["#8a3b1f", "#6b2c16"], ["#9a9aa0", "#7b7b82"]];
  const STYLES = ["short", "long", "fringe", "bun", "crop"];
  function hash(s) {
    let h = 2166136261;
    for (const ch of s) { h ^= ch.codePointAt(0); h = Math.imul(h, 16777619); }
    return h >>> 0;
  }
  // look is a person's figure: skin, hair and its style from their name,
  // the shirt from their place among the office's people.
  function look(name, shirt) {
    const h = hash(name || "?");
    const [skin, skinD] = SKIN[h % SKIN.length], [hair, hairD] = HAIR[(h >>> 4) % HAIR.length];
    return { skin, skinD, hair, hairD, style: STYLES[(h >>> 8) % STYLES.length], shirt: shirt[0], shirtD: shirt[1] };
  }

  // person draws a seated figure facing the room, 16 x 16 from (x, y); the
  // desk drawn after it hides the legs.
  function person(g, x, y, L, pose) {
    const f = pose.frame || 0;
    if (L.style === "long") rect(g, x + 3, y + 2, 10, 9, L.hairD);
    rect(g, x + 4, y + 2, 8, 7, L.skin);
    rect(g, x + 4, y + 8, 8, 1, L.skinD);
    rect(g, x + 4, y, 8, 1, L.hair); rect(g, x + 3, y + 1, 10, 2, L.hair);
    if (L.style === "short" || L.style === "long") { rect(g, x + 3, y + 3, 1, 3, L.hair); rect(g, x + 12, y + 3, 1, 3, L.hair); }
    if (L.style === "fringe") rect(g, x + 4, y + 3, 4, 1, L.hair);
    if (L.style === "bun") rect(g, x + 6, y - 2, 4, 2, L.hair);
    if (L.style === "crop") rect(g, x + 4, y + 1, 8, 1, L.hairD);
    const eye = pose.blink ? L.skinD : "#1d1b22";
    dot(g, x + 6, y + 5, eye); dot(g, x + 9, y + 5, eye);
    rect(g, x + 7, y + 7, 2, 1, L.skinD);
    rect(g, x + 7, y + 9, 2, 1, L.skinD);
    rect(g, x + 3, y + 10, 10, 6, L.shirt); rect(g, x + 2, y + 11, 12, 5, L.shirt); rect(g, x + 2, y + 15, 12, 1, L.shirtD);
    rect(g, x + 7, y + 10, 2, 1, L.skinD);
    if (pose.handUp) {
      rect(g, x + 13, y + 3, 2, 9, L.shirt); rect(g, x + 13, y + 1, 2, 2, L.skin);
      rect(g, x + 2, y + 13, 2, 2, L.skin);
    } else {
      rect(g, x + 2, y + 13 + (f % 2), 2, 2, L.skin);
      rect(g, x + 12, y + 14 - (f % 2), 2, 2, L.skin);
    }
  }

  // papers draws n files in flight as a stack of up to six sheets; mark
  // colours the top sheet when the desk holds a span in conflict.
  function papers(g, x, y, n, P, mark) {
    const k = Math.min(n, 6);
    for (let i = 0; i < k; i++) {
      const sx = x + (i % 2), sy = y - i, top = i === k - 1 && mark;
      rect(g, sx, sy, 6, 4, top ? mark : P.paper);
      rect(g, sx, sy + 4, 6, 1, P.paperEdge);
      if (!top) rect(g, sx + 1, sy + 1, 4, 1, P.ink);
    }
  }
  // plane draws a paper plane, 8 x 6 around (x, y), its nose towards dir.
  const PLANE = ["o.......", "oww.....", "owwwo...", "owwwwwo.", ".owwwoo.", "..oo...."];
  function plane(g, x, y, dir, body, edge) {
    x = Math.round(x) - 4; y = Math.round(y) - 3;
    PLANE.forEach((row, j) => [...row].forEach((ch, i) => {
      if (ch !== ".") dot(g, dir >= 0 ? x + i : x + 7 - i, y + j, ch === "w" ? body : edge);
    }));
  }
  function envelope(g, x, y, c, ink) {
    rect(g, x, y, 7, 5, c);
    for (const [dx, dy] of [[1, 1], [2, 2], [3, 3], [4, 2], [5, 1]]) dot(g, x + dx, y + dy, ink);
  }
  // pennant draws a flag on a pole whose foot is (x, y): filled at the side
  // that keeps a span, an outline at the side that aligns.
  function pennant(g, x, y, c, filled, pole) {
    rect(g, x, y - 9, 1, 10, pole);
    if (filled) {
      for (let j = 0; j < 7; j++) rect(g, x + 1, y - 9 + j, j < 4 ? 5 - j : 4 - (j - 3), 1, c);
      return;
    }
    rect(g, x + 1, y - 9, 5, 1, c);
    for (let j = 0; j < 4; j++) { dot(g, x + 5 - j, y - 9 + j, c); dot(g, x + 1, y - 9 + j, c); }
  }
  // beacon draws an alarm light, 8 wide from (x, y); on lights it and its rays.
  function beacon(g, x, y, on, P) {
    const c = on ? P.alarm : P.alarmDim;
    rect(g, x - 1, y + 6, 10, 2, P.beaconBase);
    rect(g, x + 1, y, 6, 1, c); rect(g, x, y + 1, 8, 5, c);
    rect(g, x + 2, y + 2, 2, 2, on ? "#ffe2dc" : P.alarm);
    if (on) for (const [dx, dy, w, h] of [[-4, 2, 3, 1], [-3, -1, 2, 1], [9, 2, 3, 1], [9, -1, 2, 1], [3, -4, 2, 2]]) rect(g, x + dx, y + dy, w, h, P.alarm);
  }
  // bulb draws the light bulb of a new intent, 4 wide from (x, y).
  function bulb(g, x, y, lit, P) {
    rect(g, x, y, 4, 3, P.bulb); rect(g, x + 1, y + 3, 2, 1, P.bulbD);
    if (lit) { dot(g, x - 2, y, P.bulb); dot(g, x + 5, y, P.bulb); dot(g, x + 1, y - 2, P.bulb); dot(g, x + 2, y - 2, P.bulb); }
  }

  return Object.freeze({ stage, rect, dot, checker, arc, arcAt, look, person, papers, plane, envelope, pennant, beacon, bulb, hash });
})();
