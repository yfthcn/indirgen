// Runs before the first paint, so the page opens in its language and theme
// without a flash. site.js does the rest.
(() => {
  const html = document.documentElement;
  const stored = key => { try { return localStorage.getItem(`indirgen-site.${key}`); } catch { return null; } };
  const supported = ["tr", "en"];
  const theme = stored("theme");
  if (theme === "dark" || theme === "light") html.dataset.theme = theme;
  const asked = new URLSearchParams(location.search).get("lang");
  const browser = (navigator.languages?.length ? navigator.languages : [navigator.language])
    .map(l => String(l).slice(0, 2).toLowerCase()).find(l => supported.includes(l));
  html.lang = [asked, stored("lang"), browser].find(l => supported.includes(l)) ?? "en";
  html.classList.add("js");
})();
