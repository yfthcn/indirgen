// Screenshots of İndirgen for the site, taken from a throwaway test copy.
//
//   node capture.mjs seed    add the films through the UI and start the slow one
//   node capture.mjs shots   every view, TR and EN, dark and light → ../assets/shots/*.webp
//   node capture.mjs og      the 1200×630 social card → ../assets/og-image.png
//
// Only Blender open movies (CC BY) are downloaded. README.md says how to start
// the test copy so no real address, user name or home folder shows up.

import { chromium, devices } from "playwright-core";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, copyFileSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const SITE = join(HERE, "..");
const APP = process.env.INDIRGEN_URL ?? "http://127.0.0.1:8898";
const CHROMIUM = process.env.CHROMIUM_PATH; // unset: the browser playwright-core installed

const yt = id => `https://www.youtube.com/watch?v=${id}`;
// Finished downloads, oldest first, so the archive lists them newest first.
const FILMS = [
  "SkVqJ1SGeL0", // Caminandes 3: Llamigos
  "Z4C82eyhwgU", // Caminandes 2: Gran Dillama
  "mN0zPOpADL4", // Agent 327: Operation Barbershop
  "UXqq0ZvbOnk", // Charge
  "WhWc3b3KhnY", // Spring
  "_cMxraX_5RE", // Sprite Fright
  "aqz-KE-bpKQ", // Big Buck Bunny
  "eRsGyueVLvQ", // Sintel
].map(yt);
// Tears of Steel, in full quality under the speed limit: still running while
// the shots are taken (README.md: concurrency 16 makes that ~64 KB/s).
const SLOW = yt("R6MlUcmOul8");
const INSPECT = yt("Y-rmzh0PI3c"); // Cosmos Laundromat: only inspected, the quality card

const launch = () => chromium.launch({ executablePath: CHROMIUM });

async function api(path, body) {
  const r = await fetch(APP + path, body === undefined ? {} : {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
  });
  if (!r.ok) throw new Error(`${path}: ${r.status} ${await r.text()}`);
  return r.json();
}

// paste links, inspect them, pick a quality per card and download them all
async function addThroughUI(page, urls, height) {
  await page.fill("#urls", urls.join("\n"));
  await page.click("#fetchBtn");
  await page.waitForFunction(n => document.querySelectorAll("#cards .card select.q").length === n, urls.length, { timeout: 300_000 });
  for (const sel of await page.$$("#cards .card select.q")) {
    const values = await sel.$$eval("option", o => o.map(x => +x.value));
    const pick = height ? Math.max(...values.filter(v => v && v <= height)) : 0;
    await sel.selectOption(String(pick));
  }
  await page.click(urls.length > 1 ? "#sendAll" : "#cards .card [data-act=send]");
  await page.waitForFunction(() => !document.querySelector("#cards .card"), null, { timeout: 60_000 });
}

async function seed() {
  const b = await launch();
  const page = await (await b.newContext({ viewport: { width: 1280, height: 800 } })).newPage();
  await page.goto(APP, { waitUntil: "networkidle" });
  if (process.argv[3] !== "slow") {
    await addThroughUI(page, FILMS, 480);
    console.log("eklendi:", FILMS.length, "film; bitmeleri bekleniyor");
  }
  for (;;) {
    const left = (await api("/api/jobs")).filter(j => j.status === "queued" || j.status === "running");
    if (!left.length) break;
    await new Promise(r => setTimeout(r, 5000));
  }
  const q = await api("/api/queue");
  await api("/api/queue", { ...q, rate_limit_mb: 1 });
  await addThroughUI(page, [SLOW], 0);
  console.log("Sintel 1 MB/s ile başladı; şimdi: node capture.mjs shots");
  await b.close();
}

// The test copy listens on 127.0.0.1 only, so it has no network access, no
// pairing and no signed-in devices. These three answers are replaced with an
// example home network; the QR code is still drawn by the app's own encoder.
const EXAMPLE = {
  url: "http://192.168.1.20:8899",
  code: "3f9c1a7e5b2d48c6a0e4f17b9d23c58e",
  short: "K7QM-3XRP",
  token: "example-token-not-real",
};
function appQR(text) {
  const dir = mkdtempSync(join(tmpdir(), "indirgen-qr-"));
  try {
    copyFileSync(join(HERE, "../../qr.go"), join(dir, "qr.go"));
    writeFileSync(join(dir, "go.mod"), "module qr\n\ngo 1.26\n");
    writeFileSync(join(dir, "main.go"), `package main

import (
	"fmt"
	"os"
)

func main() {
	q, err := qrEncode(os.Args[1])
	if err != nil {
		panic(err)
	}
	fmt.Print(q.svg())
}

func abs(x int) int { return max(x, -x) } // lives in subs.go in the app
`);
    return execFileSync("go", ["run", ".", text], { cwd: dir, encoding: "utf8" });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
async function exampleNetwork(ctx, qrSVG) {
  const json = body => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const ago = min => new Date(Date.now() - min * 60_000).toISOString();
  await ctx.route("**/api/connect", r => r.request().method() === "GET"
    ? r.fulfill(json({ listen: "0.0.0.0:8899", lan: true, urls: [EXAMPLE.url], token: EXAMPLE.token, can_edit: true, docker: false }))
    : r.continue());
  await ctx.route("**/api/pair", r => r.fulfill(json({
    expires: new Date(Date.now() + 4 * 60_000 + 42_000).toISOString(),
    short: EXAMPLE.short,
    links: [{ url: `${EXAMPLE.url}/pair/${EXAMPLE.code}`, svg: qrSVG }],
  })));
  await ctx.route("**/api/sessions", r => r.fulfill(json({
    current: "",
    sessions: [
      { id: "a1", name: "iPhone · Safari", via: "qr", ip: "192.168.1.31", created: ago(9000), last_seen: ago(2) },
      { id: "b2", name: "Android · Chrome", via: "qr", ip: "192.168.1.44", created: ago(20000), last_seen: ago(180) },
      { id: "c3", name: "Windows · Edge", via: "token", ip: "192.168.1.27", created: ago(40000), last_seen: ago(2900) },
    ],
  })));
}

const OUT = join(SITE, "assets/shots");
const pause = ms => new Promise(r => setTimeout(r, ms));

async function context(b, { lang, theme, phone }) {
  const ctx = await b.newContext({
    ...(phone ? { ...devices["iPhone 15"], viewport: { width: 390, height: 844 } } : { viewport: { width: 1280, height: 800 } }),
    deviceScaleFactor: 2,
    colorScheme: theme,
    locale: lang === "tr" ? "tr-TR" : "en-US",
    timezoneId: "Europe/Istanbul",
    reducedMotion: "reduce",
  });
  await ctx.addInitScript(l => {
    localStorage.setItem("indirgen.lang", JSON.stringify(l));
    localStorage.setItem("indirgen.theme", JSON.stringify("auto"));
    localStorage.setItem("indirgen.view", JSON.stringify("jobs"));
    localStorage.setItem("indirgen.libFlat", "true"); // the archive as one list of films, not folders
  }, lang);
  return ctx;
}

async function open(ctx) {
  const page = await ctx.newPage();
  page.on("pageerror", e => console.error("sayfa hatası:", e.message));
  await page.goto(APP, { waitUntil: "networkidle" });
  await page.waitForSelector("#jobs .job");
  return page;
}

async function settings(page, sec) {
  await page.click("#settingsBtn");
  await page.click(`#setNav [data-sec=${sec}]`);
  await pause(600);
}

async function archive(page) {
  await page.click("#viewSeg [data-v=library]");
  await page.waitForSelector("[data-act=view]");
  await pause(800);
}

// name → what to do on a fresh page before the shot
const DESKTOP = {
  downloads: async () => {},
  inspect: async page => {
    await page.fill("#urls", INSPECT);
    await page.click("#fetchBtn");
    const q = await page.waitForSelector("#cards .card select.q", { timeout: 120_000 });
    const values = await q.$$eval("option", o => o.map(x => x.value).filter(v => v !== "0"));
    await q.selectOption(values[0]);
    await page.click("#urls"); // the select keeps no focus ring
    await page.$eval("#urls", e => e.blur());
    await page.waitForFunction(() => [...document.querySelectorAll("#cards img")].every(i => i.complete));
  },
  archive,
  preview: async page => {
    await archive(page);
    await page.locator(".lrow", { hasText: "Big Buck Bunny" }).locator("button[data-act=view]").click();
    const v = await page.waitForSelector("video");
    await v.evaluate(async el => {
      el.muted = true;
      await new Promise(r => el.readyState >= 1 ? r() : el.addEventListener("loadedmetadata", r, { once: true }));
      el.pause();
      el.currentTime = 157; // the bunny in the meadow, in daylight
      await new Promise(r => el.addEventListener("seeked", r, { once: true }));
      while (el.readyState < 3) await new Promise(r => setTimeout(r, 100));
    });
    await page.mouse.move(640, 300); // the controls show, as they do for a viewer
    await pause(800);
  },
  engines: page => settings(page, "s-engines"),
  pairing: async page => {
    await settings(page, "s-conn");
    await page.click("#pairBtn");
    await page.waitForSelector(".pairqr svg");
    await page.$eval("#s-conn", e => e.scrollIntoView({ block: "start" }));
    await pause(300);
  },
  devices: async page => {
    await settings(page, "s-conn");
    await page.waitForSelector("#devBox .devrow");
    await page.$eval("#devBox", e => e.scrollIntoView({ block: "center" }));
    await pause(300);
  },
};
// the phone shows the list from the tabs down: the add form fills the first screen
const toTabs = page => page.$eval("#viewSeg", e =>
  window.scrollTo(0, e.getBoundingClientRect().top + scrollY - document.querySelector("header").offsetHeight - 12));
const PHONE = {
  "phone-downloads": toTabs,
  "phone-archive": async page => { await archive(page); await toTabs(page); },
};

function toWebP(png, webp, width) {
  // -strip: no EXIF or other metadata; the screenshots are flat UI, method 6 packs them best
  execFileSync("magick", [png, "-resize", `${width}x>`, "-strip", "-quality", "82", "-define", "webp:method=6", webp]);
}

async function shots() {
  mkdirSync(OUT, { recursive: true });
  const RAW = mkdtempSync(join(tmpdir(), "indirgen-shots-"));
  const qrSVG = appQR(`${EXAMPLE.url}/pair/${EXAMPLE.code}`);
  const only = process.argv[3]?.split(","); // e.g. "engines,preview" while trying some views
  const b = await launch();
  for (const lang of ["en", "tr"]) for (const theme of ["dark", "light"]) {
    for (const [views, phone, width] of [[DESKTOP, false, 1600], [PHONE, true, 780]]) {
      for (const [name, prepare] of Object.entries(views)) {
        if (only && !only.includes(name)) continue;
        const ctx = await context(b, { lang, theme, phone });
        await exampleNetwork(ctx, qrSVG);
        const page = await open(ctx);
        await prepare(page);
        const png = join(RAW, `${name}-${lang}-${theme}.png`);
        await page.screenshot({ path: png, animations: "disabled", caret: "hide" });
        toWebP(png, join(OUT, `${name}-${lang}-${theme}.webp`), width);
        console.log(`${name}-${lang}-${theme}`);
        await ctx.close();
      }
    }
  }
  await b.close();
  console.log("PNG'ler:", RAW);
}

async function og() {
  const b = await launch();
  const page = await (await b.newContext({ viewport: { width: 1200, height: 630 } })).newPage();
  await page.goto(pathToFileURL(join(HERE, "og.html")).href, { waitUntil: "load" });
  const png = join(mkdtempSync(join(tmpdir(), "indirgen-og-")), "og.png");
  await page.screenshot({ path: png });
  await b.close();
  // a 256-colour palette keeps the flat card sharp and small; -strip drops metadata
  execFileSync("magick", [png, "-strip", "-colors", "256", "-define", "png:compression-level=9", join(SITE, "assets/og-image.png")]);
  console.log("assets/og-image.png");
}

const steps = { seed, shots, og };
const step = steps[process.argv[2]];
if (!step) {
  console.error(`kullanım: node capture.mjs ${Object.keys(steps).join("|")}`);
  process.exit(2);
}
await step();
