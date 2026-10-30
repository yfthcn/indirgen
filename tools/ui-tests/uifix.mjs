// Two layout checks at 1440, 390 and 320 px, TR and EN, dark and light:
// - the theme buttons in the settings header are inline SVG icons of one
//   size, each with a name (no font glyphs);
// - a running download's figures stay on one line and keep the speed and
//   the time left.
// It needs a download in progress. With none running it starts Sintel (a
// Blender open movie, CC BY) under a 1 MB/s limit, and at the end stops it
// and puts the limit back.
import { devices } from "playwright-core";
import { join } from "node:path";
import { APP, OUT, guardTestCopy, launch, api, expectations } from "./lib.mjs";

await guardTestCopy();
let started = null, queue = null;
if (!(await api("/api/jobs")).some(j => j.status === "running")) {
  queue = await api("/api/queue");
  await api("/api/queue", { ...queue, rate_limit_mb: 1 });
  started = (await api("/api/jobs", { jobs: [{ url: "https://www.youtube.com/watch?v=eRsGyueVLvQ", engine: "yt-dlp", title: "Sintel", opts: { preset: "best" } }] })).added[0].id;
}
const { add, finish } = expectations();
const b = await launch();

for (const [w, phone] of [[1440, false], [390, true], [320, true]]) for (const lang of ["tr", "en"]) for (const theme of ["dark", "light"]) {
  const name = `${w}-${lang}-${theme}`;
  const ctx = await b.newContext({
    ...(phone ? { ...devices["iPhone 15"], viewport: { width: w, height: 844 } } : { viewport: { width: w, height: 900 } }),
    deviceScaleFactor: 2, colorScheme: theme, locale: lang === "tr" ? "tr-TR" : "en-US",
  });
  await ctx.addInitScript(l => {
    localStorage.setItem("indirgen.lang", JSON.stringify(l));
    localStorage.setItem("indirgen.view", JSON.stringify("jobs"));
  }, lang);
  const p = await ctx.newPage();
  p.on("pageerror", e => add(`${name}: ${e.message}`));
  await p.goto(APP, { waitUntil: "networkidle" });

  await p.waitForFunction(() => [...document.querySelectorAll("#jobs .job .stats")].some(s => /\/s/.test(s.textContent) && /\d:\d\d/.test(s.textContent)), null, { timeout: 120_000 });
  const st = await p.evaluate(() => {
    const s = [...document.querySelectorAll("#jobs .job .stats")].find(s => /\/s/.test(s.textContent));
    const box = s.getBoundingClientRect();
    // what the eye sees: glyph boxes inside the line box, grouped by line
    const range = document.createRange();
    range.selectNodeContents(s);
    const seen = [...range.getClientRects()].filter(r => r.width > 0 && r.top >= box.top - 1 && r.bottom <= box.bottom + 1 && r.right > box.left);
    const shown = [...s.querySelectorAll("[class^=st-]")].filter(c => { const r = c.getBoundingClientRect(); return r.width > 0 && r.top >= box.top - 1 && r.bottom <= box.bottom + 1; });
    return { lines: new Set(seen.map(r => Math.round(r.top))).size, shown: shown.map(c => c.textContent) };
  });
  const keeps = st.shown.some(x => /\/s$/.test(x)) && st.shown.some(x => /^\d+:\d\d(:\d\d)?$/.test(x));
  if (st.lines !== 1 || !keeps) add(`${name}: figures ${JSON.stringify(st)}`);
  await p.locator("#jobs .job").first().screenshot({ path: join(OUT, `job-${name}.png`) });

  await p.click("#settingsBtn");
  await p.waitForTimeout(400);
  const buttons = await p.evaluate(() => [...document.querySelectorAll("#themeSeg button")].map(b => {
    const r = b.querySelector("svg")?.getBoundingClientRect();
    return { v: b.dataset.v, text: b.textContent.trim(), size: r ? `${Math.round(r.width)}x${Math.round(r.height)}` : "", name: b.getAttribute("aria-label") ?? "" };
  }));
  if (buttons.some(x => !x.size || x.text || !x.name) || new Set(buttons.map(x => x.size)).size !== 1) add(`${name}: theme buttons ${JSON.stringify(buttons)}`);
  await p.locator(".sethead").screenshot({ path: join(OUT, `themeseg-${name}.png`) });
  console.log(name, `${st.lines} line: ${st.shown.join(" · ")} | ${buttons.map(x => x.name).join(", ")}`);
  await ctx.close();
}
await b.close();
if (started) {
  await api(`/api/jobs/${started}/cancel`, {});
  await api("/api/queue", queue);
}
finish();
