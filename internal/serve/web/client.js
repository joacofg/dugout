// The office views' shared client: the view token and serve's office stream
// (GET /v1/office/stream), whose events each carry the whole view as
// GET /v1/office answers it. It is the pages' only request, and a GET.
"use strict";

window.DugoutOffice = (() => {
  const TOKEN_KEY = "dugout.office.token";

  // ---- the token, from the address once, then from the tab ------------------

  function storage() {
    try { return window.sessionStorage; } catch (e) { return null; }
  }
  // takeToken moves a token from the fragment into this tab's session
  // storage, so switching between the two views keeps it without putting it
  // in a link.
  function takeToken() {
    const m = /(?:^#|&)t=([^&]+)/.exec(location.hash);
    const st = storage();
    if (m) {
      try { st && st.setItem(TOKEN_KEY, decodeURIComponent(m[1])); } catch (e) { /* the token stays in memory only */ }
      history.replaceState(null, "", location.pathname + location.search);
      return decodeURIComponent(m[1]);
    }
    try { return (st && st.getItem(TOKEN_KEY)) || ""; } catch (e) { return ""; }
  }
  const token = takeToken();

  // ---- the repo shown ----------------------------------------------------------

  function repoParam() {
    return new URLSearchParams(location.search).get("repo") || "";
  }
  function setRepoParam(repo) {
    const q = new URLSearchParams(location.search);
    q.set("repo", repo);
    history.replaceState(null, "", location.pathname + "?" + q.toString());
  }
  // pickRepo is the repo to show: the chosen one while it is in the view,
  // else the one with an open escalation, else the busiest.
  function pickRepo(v, chosen) {
    const names = v.repos.map((r) => r.repo);
    if (chosen && names.includes(chosen)) return chosen;
    const open = v.repos.find((r) => r.escalations.some((e) => e.open));
    if (open) return open.repo;
    return [...v.repos].sort((a, b) => b.workstreams.length - a.workstreams.length)[0]?.repo || "";
  }

  function clock(iso) {
    return new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  }

  // ---- the stream ------------------------------------------------------------------

  // connect reads the stream for as long as the page is open. onView gets each
  // view; onStatus(state, text) the status line, state "live", "stale" or
  // "down"; onFail(title, line) a state the page cannot leave by waiting.
  function connect({ onView, onStatus, onFail }) {
    let last = null;
    let delay = 1000;
    const retry = () => {
      if (last) onStatus("stale", "Reconnecting · last update " + clock(last.at));
      else onStatus("down", "Connecting");
      setTimeout(open, delay);
      delay = Math.min(delay * 2, 30000);
    };
    const event = (block) => {
      let name = "", data = "";
      for (const line of block.split("\n")) {
        if (line.startsWith("event: ")) name = line.slice(7);
        else if (line.startsWith("data: ")) data += line.slice(6);
      }
      if (name !== "office" || !data) return;
      let v;
      try { v = JSON.parse(data); } catch (e) { return; }
      // A view the page cannot draw must not tear down a healthy stream: keep
      // the last one drawn and wait for the next event.
      try { onView(v); } catch (e) { console.error("office view: could not draw this update", e); return; }
      last = v;
      onStatus("live", "Live · " + clock(v.at) + " · Read-only");
    };
    async function open() {
      let resp;
      try {
        resp = await fetch("/v1/office/stream", { headers: token ? { Authorization: "Bearer " + token } : {}, cache: "no-store" });
      } catch (e) {
        return retry();
      }
      if (resp.status === 401) {
        return token
          ? onFail("This view's token expired or was revoked.", "Run dugout office to open it again.")
          : onFail("This view needs a token.", "Run dugout office to open it.");
      }
      if (resp.status === 404) {
        return onFail("This server is older than the office view.", "Update dugout serve.");
      }
      if (!resp.ok || !resp.body) return retry();
      delay = 1000;
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      try {
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let cut;
          while ((cut = buf.indexOf("\n\n")) >= 0) {
            event(buf.slice(0, cut));
            buf = buf.slice(cut + 2);
          }
        }
      } catch (e) { /* the stream broke: retry below */ }
      retry();
    }
    open();
  }

  return Object.freeze({ token, connect, pickRepo, clock, repoParam, setRepoParam });
})();
