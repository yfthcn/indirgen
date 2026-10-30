// Checks the site the way a visitor meets it, and writes full-page
// screenshots at 1440, 390 and 320 px, TR and EN, dark and light.
//
//   node check.mjs [out-dir] [--online]
//
// Fails (exit 1) on: console errors, failed requests, broken in-page links,
// images without alt text or not loading, horizontal scrolling, text below
// WCAG AA contrast, keys missing between index.html and tr.js, metadata in
// images, a site over 3 MB. --online also asks every outside link.

import { chromium } from "playwright-core";
import { createServer } from "node:http";
import { readFile, readdir, mkdtemp, mkdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, extname, dirname, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { tr } from "../assets/tr.js";

const SITE = join(dirname(fileURLToPath(import.meta.url)), "..");
const args = process.argv.slice(2);
const online = args.includes("--online");
const OUT = args.find(a => !a.startsWith("--")) ?? await mkdtemp(join(tmpdir(), "indirgen-site-check-"));
const LIMIT = 3 * 1024 * 1024;
const problems = [];
const fail = msg => { problems.push(msg); console.log("  ✗", msg); };

// ---- files: size, metadata ------------------------------------------------

async function files(dir) {
  const out = [];
  for (const e of await readdir(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) { if (relative(SITE, p) !== "tools") out.push(...await files(p)); }
    else out.push(p);
  }
  return out;
}
// chunk names that carry metadata: EXIF, XMP, text, timestamps
function metadataChunks(buf, ext) {
  const found = [];
  if (ext === ".webp") {
    for (let i = 12; i + 8 <= buf.length;) {
      const id = buf.toString("ascii", i, i + 4), size = buf.readUInt32LE(i + 4);
      if (["EXIF", "XMP ", "ICCP"].includes(id)) found.push(id.trim());
      i += 8 + size + (size % 2);
    }
  } else if (ext === ".png") {
    for (let i = 8; i + 8 <= buf.length;) {
      const size = buf.readUInt32BE(i), id = buf.toString("ascii", i + 4, i + 8);
      if (["eXIf", "tEXt", "iTXt", "zTXt", "tIME"].includes(id)) found.push(id);
      i += 12 + size;
    }
  }
  return found;
}

const all = await files(SITE);
let total = 0;
for (const f of all) {
  const buf = await readFile(f);
  total += buf.length;
  const meta = metadataChunks(buf, extname(f));
  if (meta.length) fail(`${relative(SITE, f)}: metadata ${meta.join(", ")}`);
}
console.log(`site: ${all.length} files, ${(total / 1024 / 1024).toFixed(2)} MB (limit 3 MB, tools/ not published)`);
if (total > LIMIT) fail(`site is ${total} bytes, over 3 MB`);

// ---- translations: the same keys on both sides ----------------------------

const page = await readFile(join(SITE, "index.html"), "utf8");
const keys = new Set([
  ...[...page.matchAll(/data-i18n(?:-html)?="([^"]+)"/g)].map(m => m[1]),
  ...[...page.matchAll(/data-i18n-attr="([^"]+)"/g)].flatMap(m => m[1].split(";").map(p => p.split(":")[1])),
  "meta.title", "meta.description", "copy.done", "copy.failed",
]);
for (const k of keys) if (!(k in tr)) fail(`tr.js has no "${k}"`);
for (const k of Object.keys(tr)) if (!keys.has(k)) fail(`tr.js has "${k}", which index.html does not use`);
console.log(`translations: ${keys.size} keys`);

// ---- a static server for site/ (tools/ is not served, as on Pages) --------

const TYPES = { ".html": "text/html; charset=utf-8", ".css": "text/css", ".js": "text/javascript", ".svg": "image/svg+xml", ".png": "image/png", ".webp": "image/webp" };
const server = createServer(async (req, res) => {
  const path = decodeURIComponent(new URL(req.url, "http://x").pathname);
  const file = join(SITE, path.endsWith("/") ? path + "index.html" : path);
  if (!file.startsWith(SITE) || relative(SITE, file).startsWith("tools")) { res.writeHead(404).end(); return; }
  try {
    const body = await readFile(file);
    res.writeHead(200, { "Content-Type": TYPES[extname(file)] ?? "application/octet-stream" }).end(body);
  } catch { res.writeHead(404).end(); }
});
await new Promise(r => server.listen(0, "127.0.0.1", r));
const BASE = `http://127.0.0.1:${server.address().port}/`;

// ---- in the browser -------------------------------------------------------

// contrast of every visible text against the background behind it (images and
// screenshots excluded); large text (24 px, or 18.66 px bold) needs 3:1
const contrastProbe = () => {
  // "rgb(r g b / a)" in 0–255, or "color(srgb r g b / a)" (color-mix) in 0–1
  const rgb = s => {
    const m = s.match(/[\d.]+/g)?.map(Number) ?? [0, 0, 0, 0];
    const k = s.startsWith("color(srgb") ? 255 : 1;
    return { r: m[0] * k, g: m[1] * k, b: m[2] * k, a: m[3] ?? 1 };
  };
  const lum = ({ r, g, b }) => [r, g, b].map(v => { v /= 255; return v <= .03928 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4; })
    .reduce((s, v, i) => s + v * [.2126, .7152, .0722][i], 0);
  const over = (top, under) => ({ r: top.r * top.a + under.r * (1 - top.a), g: top.g * top.a + under.g * (1 - top.a), b: top.b * top.a + under.b * (1 - top.a), a: 1 });
  const background = el => {
    const layers = [];
    for (let e = el; e; e = e.parentElement) {
      const c = rgb(getComputedStyle(e).backgroundColor);
      if (c.a > 0) { layers.push(c); if (c.a === 1) break; }
    }
    return layers.reverse().reduce((acc, c) => over(c, acc), { r: 255, g: 255, b: 255, a: 1 });
  };
  const out = [];
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    const el = n.parentElement;
    if (!n.textContent.trim() || !el.checkVisibility({ opacityProperty: true, visibilityProperty: true })) continue;
    if (el.closest(".sr-only, .skip")) continue;
    const cs = getComputedStyle(el);
    const bg = background(el), fg = over(rgb(cs.color), bg);
    const [a, b] = [lum(fg), lum(bg)].sort((x, y) => y - x);
    const ratio = (a + .05) / (b + .05);
    const size = parseFloat(cs.fontSize), bold = +cs.fontWeight >= 700;
    const need = size >= 24 || (size >= 18.66 && bold) ? 3 : 4.5;
    out.push({ text: n.textContent.trim().slice(0, 40), ratio: Math.round(ratio * 100) / 100, need });
  }
  return out;
};

const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH });
await mkdir(OUT, { recursive: true });
const external = new Set();

for (const width of [1440, 390, 320]) for (const lang of ["en", "tr"]) for (const theme of ["dark", "light"]) {
  const name = `${width}-${lang}-${theme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: width > 800 ? 900 : 844 }, colorScheme: theme,
    locale: lang === "tr" ? "tr-TR" : "en-US", isMobile: width < 800, hasTouch: width < 800,
  });
  const page = await ctx.newPage();
  const errors = [];
  page.on("console", m => { if (m.type() === "error" || m.type() === "warning") errors.push(`console ${m.type()}: ${m.text()}`); });
  page.on("pageerror", e => errors.push(`page error: ${e.message}`));
  page.on("requestfailed", r => errors.push(`request failed: ${r.url()}`));
  page.on("response", r => { if (r.status() >= 400) errors.push(`HTTP ${r.status()}: ${r.url()}`); });
  await page.goto(`${BASE}?lang=${lang}`, { waitUntil: "networkidle" });
  // scroll down slowly so every lazy image loads, as when reading
  await page.evaluate(async () => {
    for (let y = 0; y < document.documentElement.scrollHeight; y += innerHeight / 2) {
      scrollTo(0, y);
      await new Promise(r => setTimeout(r, 120));
    }
  });
  await page.waitForFunction(() => [...document.images].every(i => i.complete), null, { timeout: 30_000 });
  await page.evaluate(() => scrollTo(0, 0));
  const facts = await page.evaluate(([lang, theme]) => ({
    lang: document.documentElement.lang,
    title: document.title,
    overflow: document.documentElement.scrollWidth - innerWidth,
    broken: [...document.images].filter(i => !i.naturalWidth).map(i => i.currentSrc || i.src),
    noAlt: [...document.images].filter(i => !i.hasAttribute("alt")).map(i => i.src),
    wrongShot: [...document.querySelectorAll("picture[data-shot] img")].filter(i => !i.src.endsWith(`-${lang}-${theme}.webp`)).map(i => i.src),
    anchors: [...document.querySelectorAll('a[href^="#"]')].filter(a => a.getAttribute("href").length > 1 && !document.querySelector(a.getAttribute("href"))).map(a => a.getAttribute("href")),
    external: [...document.querySelectorAll("a[href^=http]")].map(a => a.href),
  }), [lang, theme]);
  for (const e of errors) fail(`${name}: ${e}`);
  if (facts.lang !== lang) fail(`${name}: <html lang> is ${facts.lang}`);
  if (lang === "tr" && !facts.title.includes("yapıştır")) fail(`${name}: title not Turkish: ${facts.title}`);
  if (facts.overflow > 0) fail(`${name}: scrolls sideways by ${facts.overflow}px`);
  for (const s of facts.broken) fail(`${name}: image did not load: ${s}`);
  for (const s of facts.noAlt) fail(`${name}: image without alt: ${s}`);
  for (const s of facts.wrongShot) fail(`${name}: screenshot not in ${lang}/${theme}: ${s}`);
  for (const a of facts.anchors) fail(`${name}: link to missing ${a}`);
  facts.external.forEach(u => external.add(u));
  const low = (await page.evaluate(contrastProbe)).filter(c => c.ratio < c.need);
  for (const c of low) fail(`${name}: contrast ${c.ratio} < ${c.need}: "${c.text}"`);
  await page.screenshot({ path: join(OUT, `${name}.png`), fullPage: true });
  console.log(`${name}: ${errors.length + low.length ? "problems above" : "ok"}`);
  await ctx.close();
}

// ---- keyboard, tabs, copy, preferences --------------------------------------

{
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: "dark", locale: "en-US", permissions: ["clipboard-read", "clipboard-write"] });
  const page = await ctx.newPage();
  await page.goto(BASE, { waitUntil: "networkidle" });
  const focusRing = [];
  for (let i = 0; i < 14; i++) {
    await page.keyboard.press("Tab");
    focusRing.push(await page.evaluate(() => {
      const el = document.activeElement, cs = getComputedStyle(el);
      return { what: (el.textContent.trim() || el.getAttribute("aria-label") || el.tagName).slice(0, 24), ring: cs.outlineStyle !== "none" && parseFloat(cs.outlineWidth) > 0 };
    }));
  }
  const unseen = focusRing.filter(f => !f.ring);
  if (unseen.length) fail(`keyboard: no visible focus on ${unseen.map(f => f.what).join(", ")}`);
  console.log(`keyboard: ${focusRing.map(f => f.what).join(" | ")}`);

  await page.focus("#tab-linux");
  await page.keyboard.press("ArrowRight");
  const tabs = await page.evaluate(() => ({
    focused: document.activeElement.id,
    linuxHidden: document.querySelector("#panel-linux").hidden,
    dockerHidden: document.querySelector("#panel-docker").hidden,
  }));
  if (tabs.focused !== "tab-docker" || !tabs.linuxHidden || tabs.dockerHidden) fail(`tabs: arrow key did not switch to Docker ${JSON.stringify(tabs)}`);
  await page.click("#panel-docker .copy");
  const clip = await page.evaluate(() => navigator.clipboard.readText());
  if (!clip.startsWith("mkdir indirgen && cd indirgen") || !clip.includes("indirgen token")) fail(`copy: clipboard holds ${JSON.stringify(clip.slice(0, 60))}`);
  const copied = await page.textContent("#panel-docker .copy span");
  if (copied !== "Copied") fail(`copy: button says ${copied}`);

  await page.click('#langSeg [data-lang="tr"]');
  await page.click('#themeSeg [data-theme="light"]');
  await page.reload({ waitUntil: "networkidle" });
  const kept = await page.evaluate(() => ({ lang: document.documentElement.lang, theme: document.documentElement.dataset.theme, url: location.search, h1: document.querySelector("h1").textContent }));
  if (kept.lang !== "tr" || kept.theme !== "light" || kept.h1 !== "Yapıştır, indir.") fail(`preferences not kept after reload: ${JSON.stringify(kept)}`);
  console.log(`tabs, copy, remembered language and theme: ${JSON.stringify(kept)}`);
  await ctx.close();
}

// localStorage that throws (blocked site data): the page still works
{
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, locale: "tr-TR" });
  await ctx.addInitScript(() => {
    const no = () => { throw new DOMException("blocked", "SecurityError"); };
    Object.defineProperty(window, "localStorage", { get: no });
  });
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", e => errors.push(e.message));
  await page.goto(BASE, { waitUntil: "networkidle" });
  await page.click('#langSeg [data-lang="en"]');
  const h1 = await page.textContent("h1");
  if (errors.length || h1 !== "Paste, download.") fail(`blocked storage: ${errors.join("; ")} h1=${h1}`);
  console.log(`blocked storage: Turkish from the browser, switch to English works (${h1})`);
  await ctx.close();
}

// without JavaScript: English, both install commands, no dead controls
{
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, javaScriptEnabled: false, locale: "tr-TR" });
  const page = await ctx.newPage();
  await page.goto(BASE, { waitUntil: "networkidle" });
  const nojs = await page.evaluate(() => ({
    h1: document.querySelector("h1").textContent,
    panels: [...document.querySelectorAll("[role=tabpanel]")].filter(p => p.checkVisibility()).length,
    controls: [...document.querySelectorAll(".js-only")].filter(e => e.checkVisibility()).length,
    bodyOpacity: getComputedStyle(document.body).opacity,
  }));
  await page.screenshot({ path: join(OUT, "390-nojs.png"), fullPage: true });
  if (nojs.h1 !== "Paste, download." || nojs.panels !== 2 || nojs.controls !== 0 || nojs.bodyOpacity !== "1") fail(`without JavaScript: ${JSON.stringify(nojs)}`);
  console.log(`without JavaScript: ${JSON.stringify(nojs)}`);
  await ctx.close();
}

// reduced motion: the arrow fades in instead of growing
{
  const ctx = await browser.newContext({ reducedMotion: "reduce" });
  const page = await ctx.newPage();
  await page.goto(BASE, { waitUntil: "networkidle" });
  const anim = await page.evaluate(() => getComputedStyle(document.querySelector(".mark"), "::after").animationName);
  if (anim !== "fade") fail(`reduced motion: the arrow still animates with ${anim}`);
  console.log(`reduced motion: ${anim}`);
  await ctx.close();
}

await browser.close();
server.close();

// ---- outside links ----------------------------------------------------------

console.log(`outside links: ${[...external].join(" ")}`);
if (online) {
  for (const url of external) {
    if (url.startsWith("http://127.0.0.1")) continue; // the visitor's own İndirgen
    const r = await fetch(url, { method: "GET", redirect: "follow" }).catch(e => ({ status: e.message }));
    console.log(`  ${r.status} ${url}`);
    if (r.status !== 200) fail(`outside link ${url}: ${r.status}`);
  }
}

console.log(`\nscreenshots: ${OUT}`);
console.log(problems.length ? `${problems.length} problem(s)` : "no problems");
process.exit(problems.length ? 1 : 0);
