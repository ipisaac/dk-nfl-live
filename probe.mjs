// Throwaway: dk-scraper's exact snapshot request on Node 22.20, run by probe.go between Go probes.
import dc from "node:diagnostics_channel";
let remote = "";
dc.subscribe("undici:client:connected", ({ socket }) => { remote = `${socket.remoteAddress}:${socket.remotePort}`; });
const r = await fetch("https://sportsbook-nash.draftkings.com/api/sportscontent/dkcaon/v1/leagues/88808", {
  headers: {
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    Accept: "application/json, text/plain, */*",
    "Accept-Language": "en-CA,en;q=0.9",
    Origin: "https://sportsbook.draftkings.com",
    Referer: "https://sportsbook.draftkings.com/",
  },
  redirect: "follow",
  signal: AbortSignal.timeout(10000),
}).catch(e => ({ status: 0, text: async () => e.message }));
const body = await r.text();
let json = false;
try { JSON.parse(body); json = true; } catch {}
console.log(`status=${r.status} bytes=${body.length} json=${json} remote=${remote}`);
