// Throwaway: dk-scraper's exact snapshot request on Node 22.20, from our Render service.
const t = () => AbortSignal.timeout(10000);
const ip = await fetch("https://api.ipify.org", { signal: t() }).then(r => r.text()).catch(e => e.message);
const r = await fetch("https://sportsbook-nash.draftkings.com/api/sportscontent/dkcaon/v1/leagues/88808", {
  headers: {
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    Accept: "application/json, text/plain, */*",
    "Accept-Language": "en-CA,en;q=0.9",
    Origin: "https://sportsbook.draftkings.com",
    Referer: "https://sportsbook.draftkings.com/",
  },
  redirect: "follow",
  signal: t(),
}).catch(e => ({ status: 0, text: async () => e.message }));
const body = await r.text();
let json = false;
try { JSON.parse(body); json = true; } catch {}
console.log(`PROBE node=${process.version} openssl=${process.versions.openssl} egress=${ip} status=${r.status} bytes=${body.length} json=${json}`);
await new Promise(res => setTimeout(res, 3000));
