// Language, theme, screenshots, install tabs and copy buttons. The page is
// written in English; Turkish comes from tr.js, and any key missing there
// falls back to the English text on the page.
import { tr } from "./tr.js";

const html = document.documentElement;
const LANGS = ["tr", "en"];
const store = {
  get: key => { try { return localStorage.getItem(`indirgen-site.${key}`); } catch { return null; } },
  set: (key, value) => { try { localStorage.setItem(`indirgen-site.${key}`, value); } catch { /* private mode: not remembered */ } },
};
const dev = ["localhost", "127.0.0.1"].includes(location.hostname);

// ---- language -------------------------------------------------------------

const metaDescription = document.querySelector('meta[name="description"]');
const english = {
  "meta.title": document.title,
  "meta.description": metaDescription.content,
};
const texts = [...document.querySelectorAll("[data-i18n]")];
const markup = [...document.querySelectorAll("[data-i18n-html]")];
const attrs = [...document.querySelectorAll("[data-i18n-attr]")].flatMap(el =>
  el.dataset.i18nAttr.split(";").map(pair => {
    const [attr, key] = pair.split(":");
    return { el, attr, key };
  }));
for (const el of texts) english[el.dataset.i18n] = el.textContent;
for (const el of markup) english[el.dataset.i18nHtml] = el.innerHTML;
for (const { el, attr, key } of attrs) english[key] = el.getAttribute(attr);
english["copy.done"] = "Copied";
english["copy.failed"] = "Not copied: select the text";

function say(lang, key) {
  if (lang === "en") return english[key];
  if (!(key in tr) && dev) console.warn(`tr.js: missing "${key}"`);
  return tr[key] ?? english[key];
}

function applyLanguage(lang) {
  html.lang = lang;
  document.title = say(lang, "meta.title");
  metaDescription.content = say(lang, "meta.description");
  for (const el of texts) el.textContent = say(lang, el.dataset.i18n);
  for (const el of markup) el.innerHTML = say(lang, el.dataset.i18nHtml); // our own strings, not visitor input
  for (const { el, attr, key } of attrs) el.setAttribute(attr, say(lang, key));
  for (const b of document.querySelectorAll("#langSeg [data-lang]")) b.setAttribute("aria-pressed", String(b.dataset.lang === lang));
  showShots();
}

document.querySelector("#langSeg").addEventListener("click", e => {
  const lang = e.target.closest("[data-lang]")?.dataset.lang;
  if (!LANGS.includes(lang)) return;
  store.set("lang", lang);
  const url = new URL(location.href);
  url.searchParams.set("lang", lang);
  history.replaceState(null, "", url);
  applyLanguage(lang);
});

// ---- theme ----------------------------------------------------------------

const prefersLight = matchMedia("(prefers-color-scheme: light)");
const themeColor = { dark: "#0f1115", light: "#f5f3ef" };
const chosenTheme = () => html.dataset.theme ?? "auto";
const shownTheme = () => html.dataset.theme ?? (prefersLight.matches ? "light" : "dark");

function applyTheme() {
  for (const b of document.querySelectorAll("#themeSeg [data-theme]")) b.setAttribute("aria-pressed", String(b.dataset.theme === chosenTheme()));
  for (const m of document.querySelectorAll('meta[name="theme-color"]')) m.content = themeColor[shownTheme()];
  showShots();
}

document.querySelector("#themeSeg").addEventListener("click", e => {
  const theme = e.target.closest("[data-theme]")?.dataset.theme;
  if (!theme) return;
  if (theme === "auto") delete html.dataset.theme;
  else html.dataset.theme = theme;
  store.set("theme", theme);
  applyTheme();
});
prefersLight.addEventListener("change", applyTheme);

// ---- screenshots: the page's language and theme ----------------------------

function showShots() {
  for (const pic of document.querySelectorAll("picture[data-shot]")) {
    const file = `assets/shots/${pic.dataset.shot}-${html.lang}-${shownTheme()}.webp`;
    const img = pic.querySelector("img");
    pic.querySelector("source")?.remove(); // the system theme no longer decides
    if (img.getAttribute("src") !== file) img.src = file;
    const link = pic.closest("a");
    if (link) link.href = file;
  }
}

// ---- install tabs (WAI-ARIA tabs: arrows, Home, End) -----------------------

const tabs = [...document.querySelectorAll('[role="tab"]')];
function selectTab(tab, focus) {
  for (const t of tabs) {
    const on = t === tab;
    t.setAttribute("aria-selected", String(on));
    t.tabIndex = on ? 0 : -1;
    document.getElementById(t.getAttribute("aria-controls")).hidden = !on;
  }
  if (focus) tab.focus();
}
for (const tab of tabs) {
  tab.addEventListener("click", () => selectTab(tab, false));
  tab.addEventListener("keydown", e => {
    const i = tabs.indexOf(tab);
    const next = { ArrowRight: i + 1, ArrowLeft: i - 1, Home: 0, End: tabs.length - 1 }[e.key];
    if (next === undefined) return;
    e.preventDefault();
    selectTab(tabs[(next + tabs.length) % tabs.length], true);
  });
}
selectTab(tabs[0], false);

// ---- copy -----------------------------------------------------------------

const status = document.querySelector("#copyStatus");
async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // no clipboard API (plain http, older browser): select and copy
    const ta = Object.assign(document.createElement("textarea"), { value: text, readOnly: true });
    ta.style.cssText = "position:fixed;opacity:0";
    document.body.append(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    return ok;
  }
}
for (const btn of document.querySelectorAll(".copy")) {
  const label = btn.querySelector("span");
  btn.addEventListener("click", async () => {
    const ok = await copy(btn.closest(".code").querySelector("pre").textContent);
    const word = say(html.lang, ok ? "copy.done" : "copy.failed");
    label.textContent = word;
    status.textContent = word;
    btn.toggleAttribute("data-done", ok);
    clearTimeout(btn.timer);
    btn.timer = setTimeout(() => {
      label.textContent = say(html.lang, "copy.btn");
      btn.removeAttribute("data-done");
      status.textContent = "";
    }, 2000);
  });
}

applyTheme();
applyLanguage(html.lang);
html.classList.add("ready");
