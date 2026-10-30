// End to end with the real engines and API, nothing answered here:
// 1. subscribe to the Blender Studio channel with "only what comes next"
//    (newest 10): nothing is downloaded, the card says the existing items
//    were marked, new and total stay 0;
// 2. "Check now": still nothing downloaded;
// 3. subscribe normally to Blender Studio's open movies playlist (newest 5,
//    360p): what is not marked yet is downloaded and counted.
// Only Blender Studio videos; step 3 downloads a few short films.
import { join } from "node:path";
import { APP, OUT, guardTestCopy, launch, api, pause, expectations } from "./lib.mjs";

const CHANNEL = "https://www.youtube.com/@BlenderStudio/videos";
const PLAYLIST = "https://www.youtube.com/playlist?list=PLav47HAVZMjnTFVZL-aImCQIC0uLZtNCz"; // Blender Open Movies
const MARKED = {
  tr: "Mevcut öğeler işaretlendi; bundan sonra yalnızca yeniler inecek.",
  en: "Existing items marked; only new ones will be downloaded from now on.",
};

const about = await guardTestCopy();
const { check, finish } = expectations();
const subs = async () => (await api("/api/subs")).subs;
const sub = async match => (await subs()).find(s => s.url.includes(match));
const job = async id => (await api("/api/jobs")).find(j => j.id === id);
const files = async () => (await api("/api/library?flat=1")).files?.length ?? 0;
async function settled(id, after = "") {
  for (;;) {
    const x = (await subs()).find(s => s.id === id);
    if (x.last_check && !x.running && x.last_job && x.last_job !== after) return x;
    await pause(2000);
  }
}

const b = await launch();
const page = async lang => {
  const ctx = await b.newContext({ viewport: { width: 1440, height: 900 }, locale: lang === "tr" ? "tr-TR" : "en-US" });
  await ctx.addInitScript(l => localStorage.setItem("indirgen.lang", JSON.stringify(l)), lang);
  const p = await ctx.newPage();
  await p.goto(APP, { waitUntil: "networkidle" });
  return p;
};
const p = await page("tr");
async function subscribe(url, { last, fromNow, height }) {
  await p.click("#viewSeg [data-v=jobs]");
  await p.fill("#urls", url);
  await p.click("#fetchBtn");
  await p.locator("#cards [data-act=subscribe]").waitFor({ timeout: 120_000 });
  if (height) await p.selectOption("#cards select.q", String(height));
  await p.click("#cards [data-act=subscribe]");
  await p.waitForSelector(".subdlg", { state: "visible" });
  await p.selectOption("#subLast", String(last));
  if (fromNow) await p.check("#subFromNow");
  await p.click("#subOk");
  await p.waitForSelector("#subRoot.hidden", { state: "attached" });
}
const cardText = async (q, id) => {
  await q.click("#viewSeg [data-v=subs]");
  await q.waitForTimeout(800);
  return (await q.locator(`.lrow.sub[data-id="${id}"] .nm > span`).textContent()).replace(/\s+/g, " ");
};

console.log(`test copy: ${APP}, data ${about.data}, archive ${about.library}`);
const start = await files();

// 1
await subscribe(CHANNEL, { last: 10, fromNow: true });
let a = await settled((await sub("BlenderStudio/videos")).id);
let j = await job(a.last_job);
console.log("1. only what comes next:", { job: j.status, note: j.note, files: await files() - start, last_new: a.last_new, total: a.total_new });
check(j.status === "done" && j.note === "marked" && a.last_marked > 0, `1: ${JSON.stringify({ j, a })}`);
check(a.last_new === 0 && a.total_new === 0 && await files() === start, `1: something was downloaded or counted ${JSON.stringify(a)}`);
for (const lang of ["tr", "en"]) {
  const q = lang === "tr" ? p : await page("en");
  const text = await cardText(q, a.id);
  check(text.includes(MARKED[lang]) && !/\d+ (öğe|items?)/.test(text), `1 ${lang}: card says "${text}"`);
  await q.screenshot({ path: join(OUT, `e2e-marked-${lang}.png`) });
  console.log(`   card (${lang}):`, text);
}

// 2
const before = a.last_job;
await p.locator(`.lrow.sub[data-id="${a.id}"] [data-sb=check]`).click();
a = await settled(a.id, before);
j = await job(a.last_job);
console.log("2. check now:", { job: j.status, note: j.note, files: await files() - start, last_new: a.last_new, total: a.total_new });
check(j.note === "no_new" && a.last_new === 0 && a.total_new === 0 && await files() === start, `2: ${JSON.stringify({ j, a })}`);

// 3
await subscribe(PLAYLIST, { last: 5, fromNow: false, height: 360 });
const n = await settled((await sub("PLav47HAVZMjnTFVZL")).id);
const got = await files() - start;
console.log("3. normal subscription:", { job: (await job(n.last_job)).status, files: got, last_new: n.last_new, total: n.total_new, last_marked: n.last_marked ?? 0 });
check(got > 0 && n.last_new === got && n.total_new === got && !n.last_marked, `3: ${JSON.stringify({ got, n })}`);
await p.screenshot({ path: join(OUT, "e2e-subs.png") });

await b.close();
finish();
