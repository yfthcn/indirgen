// Shared by the background script and the options page.
const DEFAULTS = { server: "http://127.0.0.1:8899", token: "", mode: "library" };

async function getSettings() {
  const s = await chrome.storage.local.get(DEFAULTS);
  s.server = (s.server || DEFAULTS.server).trim().replace(/\/+$/, "");
  return s;
}

// The extension can always reach this computer; any other address needs a
// host permission the user grants on the options page.
function originPattern(server) {
  const u = new URL(server);
  return `${u.protocol}//${u.hostname}/*`;
}

async function hasAccess(server) {
  const u = new URL(server);
  if (u.hostname === "127.0.0.1" || u.hostname === "localhost") return true;
  return chrome.permissions.contains({ origins: [originPattern(server)] });
}

async function call(path, { method = "GET", body } = {}) {
  const s = await getSettings();
  if (!(await hasAccess(s.server))) throw new Error(chrome.i18n.getMessage("noPermission"));
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (s.token) headers["Authorization"] = "Bearer " + s.token.trim();
  let r;
  try {
    r = await fetch(s.server + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new Error(chrome.i18n.getMessage("noServer", [s.server]));
  }
  const j = await r.json().catch(() => ({}));
  if (r.status === 401) throw new Error(chrome.i18n.getMessage("badToken"));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}
