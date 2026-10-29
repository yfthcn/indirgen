// İndirgen'e gönder: toolbar button, right-click menu and Alt+Shift+I send
// the page (or the clicked link / media) to POST /api/add.
if (typeof importScripts === "function") importScripts("common.js"); // Chrome service worker; Firefox loads it via manifest

function menus() {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({ id: "page", title: chrome.i18n.getMessage("menuPage"), contexts: ["page"] });
    chrome.contextMenus.create({ id: "link", title: chrome.i18n.getMessage("menuLink"), contexts: ["link"] });
    chrome.contextMenus.create({ id: "media", title: chrome.i18n.getMessage("menuMedia"), contexts: ["image", "video", "audio"] });
  });
}
chrome.runtime.onInstalled.addListener(menus);
chrome.runtime.onStartup?.addListener(menus);

function badge(tabId, text, color) {
  chrome.action.setBadgeBackgroundColor({ tabId, color });
  chrome.action.setBadgeText({ tabId, text });
  if (text) setTimeout(() => chrome.action.setBadgeText({ tabId, text: "" }), 2500);
}

function notify(message, isError) {
  chrome.notifications.create({
    type: "basic", iconUrl: "icons/128.png",
    title: isError ? "İndirgen ✕" : "İndirgen", message,
  });
}

async function send(url, tabId) {
  if (!/^https?:\/\//i.test(url || "")) return notify(chrome.i18n.getMessage("badUrl"), true);
  if (tabId !== undefined) badge(tabId, "…", "#8a92a6");
  try {
    const s = await getSettings();
    const r = await call("/api/add", { method: "POST", body: { url, mode: s.mode } });
    if (tabId !== undefined) badge(tabId, "✓", "#2f9e6e");
    notify(r.message || chrome.i18n.getMessage("sent"));
  } catch (e) {
    if (tabId !== undefined) badge(tabId, "!", "#d9534f");
    notify(e.message, true);
  }
}

chrome.action.onClicked.addListener(tab => send(tab.url, tab.id));

chrome.commands.onCommand.addListener(async cmd => {
  if (cmd !== "send-page") return;
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tab) send(tab.url, tab.id);
});

chrome.contextMenus.onClicked.addListener((info, tab) => {
  // A clicked link wins; media only when it is a real address (YouTube's
  // video element is a blob: URL, so the page goes instead).
  let url = info.pageUrl;
  if (info.menuItemId === "link") url = info.linkUrl;
  else if (info.menuItemId === "media" && /^https?:/i.test(info.srcUrl || "")) url = info.srcUrl;
  send(url, tab?.id);
});
