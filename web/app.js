"use strict";
// DK strings reach the DOM through textContent only.

const WATCHDOG_MS = 35000;
const DELAY_NOTE = "Median over recent DK updates, estimated without comparing clocks: DK's own processing, " +
  "half of each network round trip, and our server's processing. Excludes uneven network paths, " +
  "the stream write and your browser drawing the change. Not the age of any one price.";
const MARKETS = [["spread", "away", "home"], ["total", "over", "under"], ["moneyline", "away", "home"]];

const table = document.getElementById("games");
const stateEl = document.getElementById("state");
const detailEl = document.getElementById("detail");
const emptyEl = document.getElementById("empty");

const games = new Map(); // id → {tb, r}
let server = { state: "connecting" }; // the last status the server sent
let conn = "open"; // open | reconnecting | timeout (browser-side)
let lastEvent = null, lastMove = null, es = null, watchdog = 0, retry = 0;
let rtt = null, probing = false; // browser ↔ server round trip, ms; null until measured

const clock = (d) => d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit", second: "2-digit" });

function kickoff(r) {
  if (r.live) return "LIVE";
  if (!r.start) return "";
  return new Date(r.start).toLocaleString([], { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
}

function order(a, b) {
  return (Date.parse(a.start) || 0) - (Date.parse(b.start) || 0) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
}

function build(id) {
  const tb = document.createElement("tbody");
  tb.dataset.id = id;
  for (let i = 0; i < 2; i++) {
    const tr = tb.insertRow();
    if (i === 0) {
      const t = tr.insertCell();
      t.className = "time";
      t.rowSpan = 2;
    }
    tr.insertCell().className = "team";
    for (let m = 0; m < MARKETS.length; m++) tr.insertCell().className = "price";
  }
  return tb;
}

function lineText(market, side, line) {
  if (line == null) return "";
  if (market === "total") return (side === "over" ? "O " : "U ") + line;
  return line > 0 ? "+" + line : String(line);
}

function move(was, p) {
  if (!was || !p) return "";
  if (p.decimal && was.decimal && p.decimal !== was.decimal) return p.decimal > was.decimal ? "up" : "down";
  if (p.line != null && was.line != null && p.line !== was.line) return p.line > was.line ? "up" : "down";
  return "";
}

function setPrice(td, r, old, market, side) {
  const p = r[market][side];
  const suspended = r.suspended[market];
  const line = document.createElement("span");
  const odds = document.createElement("span");
  line.className = "line";
  odds.className = "odds";
  if (suspended) {
    odds.textContent = "🔒";
    td.title = "Suspended";
  } else if (p) {
    line.textContent = lineText(market, side, p.line);
    odds.textContent = p.american;
    td.title = "";
  } else {
    odds.textContent = "–";
    td.title = "No price";
  }
  td.replaceChildren(line, odds);
  td.classList.toggle("susp", suspended);

  const dir = suspended || (old && old.suspended[market]) ? "" : move(old && old[market][side], p);
  if (dir) {
    td.classList.remove("up", "down");
    void td.offsetWidth; // restart the animation
    td.classList.add(dir);
  }
}

function fill(tb, r, old) {
  const [away, home] = tb.rows;
  const time = away.cells[0];
  time.textContent = kickoff(r);
  time.classList.toggle("live", r.live);
  away.cells[1].textContent = r.away;
  home.cells[0].textContent = "@ " + r.home;
  MARKETS.forEach(([market, a, h], i) => {
    setPrice(away.cells[2 + i], r, old, market, a);
    setPrice(home.cells[1 + i], r, old, market, h);
  });
}

function place(tb, r) {
  for (const other of table.tBodies) {
    if (other !== tb && order(r, games.get(other.dataset.id).r) < 0) {
      table.insertBefore(tb, other);
      return;
    }
  }
  table.appendChild(tb);
}

function upsert(r) {
  const g = games.get(r.id);
  if (!g) {
    const tb = build(r.id);
    fill(tb, r, null);
    games.set(r.id, { tb, r });
    place(tb, r);
    return;
  }
  fill(g.tb, r, g.r);
  const moved = g.r.start !== r.start;
  g.r = r;
  if (moved) place(g.tb, r);
}

function remove(id) {
  const g = games.get(id);
  if (g) {
    g.tb.remove();
    games.delete(id);
  }
}

// A board after a reconnect patches what we hold, so moves across the gap still flash.
function onBoard(b) {
  const keep = new Set(b.rows.map((r) => r.id));
  for (const id of [...games.keys()]) if (!keep.has(id)) remove(id);
  for (const r of b.rows) upsert(r);
  for (const r of b.rows) table.appendChild(games.get(r.id).tb); // server order
  server = b.status;
}

function onPatch(p) {
  for (const r of p.rows || []) upsert(r);
  for (const id of p.removed || []) remove(id);
  lastMove = new Date();
}

function render() {
  let state = server.state, since = server.staleSince ? new Date(server.staleSince) : null;
  if (conn === "timeout" && lastEvent) [state, since] = ["stale", lastEvent];
  else if (conn !== "open") state = "reconnecting";

  document.body.dataset.state = state;
  stateEl.textContent = {
    connecting: "Connecting…",
    live: "LIVE",
    polling: "Polling DK",
    reconnecting: "Reconnecting…",
    stale: since ? "Stale since " + clock(since) : "Stale",
  }[state] || state;

  const parts = [];
  if (lastMove) parts.push("updated " + clock(lastMove));
  const delay = state === "live" && server.delayP50ms && rtt != null;
  if (delay) parts.push("est. delay ~" + Math.round(server.delayP50ms + rtt / 2) + " ms");
  detailEl.textContent = parts.join(" · ");
  detailEl.title = delay ? DELAY_NOTE : "";
  emptyEl.hidden = games.size > 0 || server.state === "connecting";
}

// Half a round trip estimates the server → browser hop without comparing our clock to the server's.
async function probe() {
  if (probing) return;
  probing = true;
  const start = performance.now();
  try {
    await fetch("/healthz", { method: "HEAD", cache: "no-store", signal: AbortSignal.timeout(5000) });
    rtt = performance.now() - start;
  } catch {
  } finally {
    probing = false;
  }
  render();
}

function heard() {
  lastEvent = new Date();
  conn = "open";
  arm();
}

// Only an event, including the 15 s status heartbeat, resets the watchdog; reconnect attempts don't. When
// it fires the server is unreachable and can't report its own staleness, so we say so and reopen the
// stream, again every WATCHDOG_MS until an event arrives.
function arm() {
  clearTimeout(watchdog);
  watchdog = setTimeout(() => {
    conn = "timeout";
    render();
    connect();
    arm();
  }, WATCHDOG_MS);
}

function on(event, handle) {
  es.addEventListener(event, (e) => {
    heard();
    handle(JSON.parse(e.data));
    render();
  });
}

function connect() {
  clearTimeout(retry);
  if (es) es.close();
  es = new EventSource("/events");
  on("board", (b) => { onBoard(b); probe(); });
  on("patch", onPatch);
  on("status", (s) => { server = s; probe(); });
  es.onerror = () => {
    if (conn === "open") conn = "reconnecting";
    render();
    if (es.readyState === EventSource.CLOSED) retry = setTimeout(connect, 3000); // the browser gave up (e.g. a 502)
  };
}

connect();
arm();
render();
