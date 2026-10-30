// The subscribe dialog at 1440 and 390 px, TR and EN, dark and light: focus
// on open, Tab stays inside, Esc closes and gives focus back, and the chosen
// values reach POST /api/subs. That request is answered here, so no
// subscription is made; the channel is inspected once, for real.
import { devices } from "playwright-core";
import { join } from "node:path";
import { APP, OUT, guardTestCopy, launch, api, expectations } from "./lib.mjs";

const CHANNEL = "https://www.youtube.com/@BlenderStudio/videos";
await guardTestCopy();
const probe = JSON.stringify(await api("/api/probe", { url: CHANNEL }));
const { check, add, finish } = expectations();
const b = await launch();

for (const [w, phone] of [[1440, false], [390, true]]) for (const lang of ["tr", "en"]) for (const theme of ["dark", "light"]) {
  const name = `${w}-${lang}-${theme}`;
  const ctx = await b.newContext({
    ...(phone ? { ...devices["iPhone 15"], viewport: { width: 390, height: 844 } } : { viewport: { width: 1440, height: 900 } }),
    deviceScaleFactor: 1, colorScheme: theme, locale: lang === "tr" ? "tr-TR" : "en-US", reducedMotion: "reduce",
  });
  await ctx.addInitScript(l => {
    localStorage.setItem("indirgen.lang", JSON.stringify(l));
    localStorage.setItem("indirgen.view", JSON.stringify("jobs"));
  }, lang);
  await ctx.route("**/api/probe", r => r.fulfill({ status: 200, contentType: "application/json", body: probe }));
  let sent = null;
  await ctx.route("**/api/subs", r => {
    if (r.request().method() !== "POST") return r.continue();
    sent = r.request().postDataJSON();
    return r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "x" }) });
  });
  const p = await ctx.newPage();
  p.on("pageerror", e => add(`${name}: ${e.message}`));
  p.on("console", m => m.type() === "error" && add(`${name} console: ${m.text()}`));
  await p.goto(APP, { waitUntil: "networkidle" });
  await p.fill("#urls", CHANNEL);
  await p.click("#fetchBtn");
  const subBtn = p.locator("#cards [data-act=subscribe]");
  await subBtn.click();
  await p.waitForSelector(".subdlg", { state: "visible" });
  const st = await p.evaluate(() => ({
    focus: document.activeElement.id,
    every: document.querySelector("#subEvery").value,
    last: document.querySelector("#subLast").value,
    fromNow: document.querySelector("#subFromNow").checked,
    intervals: document.querySelectorAll("#subEvery option").length,
    limits: [...document.querySelectorAll("#subLast option")].map(o => o.value).join(","),
    box: (r => ({ l: r.left, r: r.right, t: r.top }))(document.querySelector(".subdlg").getBoundingClientRect()),
  }));
  check(st.focus === "subEvery", `${name}: focus on open is ${st.focus}`);
  check(st.every === "6" && st.last === "10" && !st.fromNow, `${name}: defaults ${JSON.stringify(st)}`);
  check(st.intervals === 7 && st.limits === "5,10,25,50,100", `${name}: options ${JSON.stringify(st)}`);
  check(st.box.l >= 0 && st.box.r <= w && st.box.t >= 0, `${name}: dialog off screen ${JSON.stringify(st.box)}`);
  await p.screenshot({ path: join(OUT, `dialog-${name}.png`) });

  await p.check("#subFromNow");
  await p.selectOption("#subLast", "25");
  await p.selectOption("#subEvery", "168");
  check((await p.textContent("#subHint")).includes("25"), `${name}: hint does not follow the limit`);
  await p.screenshot({ path: join(OUT, `dialog-${name}-fromnow.png`) });

  await p.focus("#subOk");
  await p.keyboard.press("Tab");
  check(await p.evaluate(() => !!document.activeElement.closest(".subdlg")), `${name}: Tab left the dialog`);
  await p.keyboard.press("Shift+Tab");
  check(await p.evaluate(() => !!document.activeElement.closest(".subdlg")), `${name}: Shift+Tab left the dialog`);

  await p.keyboard.press("Escape");
  const closed = await p.evaluate(() => ({ hidden: document.querySelector("#subRoot").classList.contains("hidden"), back: document.activeElement.dataset.act }));
  check(closed.hidden && closed.back === "subscribe" && sent === null, `${name}: Esc ${JSON.stringify(closed)}, sent ${JSON.stringify(sent)}`);

  await subBtn.click();
  await p.selectOption("#subEvery", "24");
  await p.selectOption("#subLast", "50");
  await p.check("#subFromNow");
  await p.click("#subOk");
  await p.waitForSelector("#subRoot.hidden", { state: "attached" });
  check(sent?.interval_h === 24 && sent?.opts.limit === 50 && sent?.from_now === true && sent?.opts.playlist === true,
    `${name}: sent ${JSON.stringify(sent)}`);
  console.log(name, JSON.stringify({ interval_h: sent?.interval_h, limit: sent?.opts.limit, from_now: sent?.from_now }));
  await ctx.close();
}
await b.close();
finish();
