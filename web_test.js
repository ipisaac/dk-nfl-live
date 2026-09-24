"use strict";
// Runs web/app.js against a fake DOM, EventSource and clock. `go test` runs it via TestAppJS.
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const src = fs.readFileSync(path.join(__dirname, "web", "app.js"), "utf8");

function load() {
  let now = 0, nextId = 1;
  const timers = new Map();
  const sources = [];
  const node = () => ({ textContent: "", hidden: false, dataset: {} });
  const els = { games: { tBodies: [] }, state: node(), detail: node(), empty: node() };
  const document = { getElementById: (id) => els[id], body: node() };

  class EventSource {
    static CLOSED = 2;
    constructor() { this.readyState = 0; this.listeners = {}; sources.push(this); }
    addEventListener(name, fn) { this.listeners[name] = fn; }
    close() { this.readyState = 2; }
    emit(name, data) { this.readyState = 1; this.listeners[name]({ data: JSON.stringify(data) }); }
    fail() { this.readyState = 2; this.onerror(); }
  }

  const setTimeout = (fn, ms) => { timers.set(nextId, { at: now + ms, fn }); return nextId++; };
  const clearTimeout = (id) => timers.delete(id);
  function advance(ms) {
    const end = now + ms;
    for (;;) {
      let due = null;
      for (const [id, t] of timers) if (t.at <= end && (!due || t.at < due[1].at)) due = [id, t];
      if (!due) break;
      timers.delete(due[0]);
      now = due[1].at;
      due[1].fn();
    }
    now = end;
  }

  vm.runInNewContext(src, { document, EventSource, setTimeout, clearTimeout });
  return { state: () => document.body.dataset.state, sources, latest: () => sources.at(-1), advance };
}

const live = { rows: [], status: { state: "live" } };

// A browser connecting before the first snapshot gets an empty board.
{
  const p = load();
  p.latest().emit("board", { rows: [], status: { state: "connecting" } });
  assert.strictEqual(p.state(), "connecting");
  p.latest().emit("status", { state: "live" });
  assert.strictEqual(p.state(), "live");
}

// Reconnect attempts that keep failing must not postpone staleness.
{
  const p = load();
  p.latest().emit("board", live);
  for (let t = 1000; t <= 60000; t += 1000) {
    if (p.latest().readyState !== 2) p.latest().fail();
    p.advance(1000);
    assert.strictEqual(p.state(), t < 35000 ? "reconnecting" : "stale", `at ${t} ms`);
  }
  p.latest().emit("board", live);
  assert.strictEqual(p.state(), "live");
}

// A stream that stays open but silent goes stale at 35 s and is reopened every 35 s until data arrives.
{
  const p = load();
  p.latest().emit("board", live);
  p.advance(34999);
  assert.strictEqual(p.state(), "live");
  p.advance(1);
  assert.strictEqual(p.state(), "stale");
  assert.strictEqual(p.sources.length, 2);
  p.advance(35000);
  assert.strictEqual(p.sources.length, 3);
  assert.strictEqual(p.state(), "stale");
  p.latest().emit("status", { state: "live" });
  assert.strictEqual(p.state(), "live");
}

console.log("ok");
