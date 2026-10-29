const $ = id => document.getElementById(id);
document.querySelectorAll("[data-i18n]").forEach(el => el.textContent = chrome.i18n.getMessage(el.dataset.i18n));

(async () => {
  const s = await getSettings();
  $("server").value = s.server; $("token").value = s.token; $("mode").value = s.mode;
})();

function status(text, ok) { $("status").textContent = text; $("status").className = ok ? "ok" : "bad"; }

$("save").addEventListener("click", async () => {
  let server = $("server").value.trim().replace(/\/+$/, "") || DEFAULTS.server;
  try { const u = new URL(server); if (!/^https?:$/.test(u.protocol)) throw 0; } catch { return status("http://…", false); }
  // Ask for access to another computer's address here: permission prompts
  // need a click, the background script cannot ask.
  if (!(await hasAccess(server))) {
    const ok = await chrome.permissions.request({ origins: [originPattern(server)] });
    if (!ok) return status(chrome.i18n.getMessage("noPermission"), false);
  }
  await chrome.storage.local.set({ server, token: $("token").value.trim(), mode: $("mode").value });
  $("server").value = server;
  try {
    const r = await call("/api/session");
    if (!r.authed) return status(chrome.i18n.getMessage("badToken"), false);
    status(chrome.i18n.getMessage("optOk", [r.version]), true);
  } catch (e) { status(e.message, false); }
});
