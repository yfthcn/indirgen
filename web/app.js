"use strict";
/* ------------------------------------------------------------------ i18n */
const LS = {get(k,d){try{const v=localStorage.getItem("indirgen."+k);return v==null?d:JSON.parse(v)}catch{return d}},
            set(k,v){try{localStorage.setItem("indirgen."+k,JSON.stringify(v))}catch{}}};
let lang = LS.get("lang", (navigator.language||"").toLowerCase().startsWith("tr") ? "tr" : "en");
// With {n} = 1, a "<key>_one" text wins where the language has one ("1 file").
// "{c|file|files}" inside a text picks by another count; it is resolved before
// the values go in, so a folder name with "{" or "|" stays as it is.
const t = (k, vars) => { const one = vars && +vars.n === 1, pick = d => d && ((one ? d[k+"_one"] : undefined) ?? d[k]);
  let s = pick(I18N[lang]) ?? pick(I18N.en) ?? k;
  s = s.replace(/\{(\w+)\|([^|{}]*)\|([^|{}]*)\}/g, (_, v, a, b) => vars && +vars[v] === 1 ? a : b);
  if (vars) for (const [a,b] of Object.entries(vars)) s = s.replace("{"+a+"}", b); return s; };
function applyI18n(){
  document.documentElement.lang = lang;
  document.querySelectorAll("[data-t]").forEach(el => el.textContent = t(el.dataset.t));
  document.querySelectorAll("[data-tp]").forEach(el => el.placeholder = t(el.dataset.tp));
  document.querySelectorAll("[data-ta]").forEach(el => { el.title = t(el.dataset.ta); el.setAttribute("aria-label", el.title); }); // icon-only buttons
  setSeg("#langSeg", lang);
}

/* --------------------------------------------------------------- helpers */
const $ = s => document.querySelector(s);
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
const icon = (id, cls="") => `<svg class="${cls}"><use href="#${id}"/></svg>`;
function fmtBytes(n){ if(!n||n<0) return "–"; const u=["B","KB","MB","GB","TB"]; let i=0; while(n>=1024&&i<u.length-1){n/=1024;i++} return (n>=100||i===0?n.toFixed(0):n.toFixed(1))+" "+u[i]; }
function fmtDur(s){ s=Math.round(s||0); if(!s) return ""; const h=Math.floor(s/3600), m=Math.floor(s%3600/60), x=s%60;
  return (h?h+":"+String(m).padStart(2,"0"):m)+":"+String(x).padStart(2,"0"); }
function ago(iso){ if(!iso||iso.startsWith("0001")) return "–"; const d=(Date.now()-new Date(iso))/1000;
  if(d<60) return t("just_now"); if(d<3600) return t("min_ago",{n:Math.floor(d/60)}); if(d<86400) return t("h_ago",{n:Math.floor(d/3600)}); return t("d_ago",{n:Math.floor(d/86400)}); }
// A clock time; in Turkish with the "at" suffix the way it is read: 14:05'te, 14:30'da.
function atTime(iso){ const d=new Date(iso), h=d.getHours(), m=d.getMinutes(), hm=String(h).padStart(2,"0")+":"+String(m).padStart(2,"0");
  if (lang!=="tr") return hm;
  const n = m || h, last = n%10 || n; // the word read last: the minutes, or the hour on the hour
  return hm+"'"+({0:"da",1:"de",2:"de",3:"te",4:"te",5:"te",6:"da",7:"de",8:"de",9:"da",10:"da",20:"de",30:"da",40:"ta",50:"de"}[last]); }
// GitHub's rate limit, while it lasts: when the next check is ("" when none).
const waitUntil = iso => iso && !iso.startsWith("0001") && new Date(iso) > Date.now() ? t("gh_rate",{t:atTime(iso)}) : "";
function toast(msg, bad){ const el=document.createElement("div"); el.className="toast"+(bad?" bad":""); el.textContent=msg; $("#toasts").append(el); setTimeout(()=>el.remove(), bad?6000:3200); }
function setSeg(sel, v){ document.querySelectorAll(sel+" button").forEach(b=>b.classList.toggle("on", b.dataset.v===v)); }

async function api(path, {method="GET", body, signal}={}){
  const r = await fetch(path, {method, credentials:"same-origin", signal,
    headers: body!==undefined ? {"Content-Type":"application/json"} : {},
    body: body!==undefined ? JSON.stringify(body) : undefined});
  if (r.status===401){ showLogin(); throw new Error(t("login_hint")); }
  const j = await r.json().catch(()=>({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}

// busy: while fn waits for the server, b shows a spinner and takes no
// second click.
async function busy(b, fn){
  if (!b || b.getAttribute("aria-busy") === "true") return;
  b.setAttribute("aria-busy", "true"); b.disabled = true;
  try{ return await fn(); } finally{ b.removeAttribute("aria-busy"); b.disabled = false; }
}
// errHTML: an engine error; a long one folds to its first line.
// The summary is İndirgen's own explanation when there is one (it comes
// before the raw "(ERROR: …)"), else the first line.
function errHTML(msg){
  const m = String(msg || ""), first = m.split("\n")[0], own = first.split(" (ERROR")[0];
  if (m.length <= 160 && !m.includes("\n")) return `<div class="errtxt">${esc(m)}</div>`;
  const sum = own !== first && own.length >= 20 ? own : first.length > 140 ? first.slice(0, 140) + "…" : first;
  return `<details class="errtxt"><summary>${esc(sum)}</summary><div class="errfull">${esc(m)}</div></details>`;
}
// emptyHTML: what goes here, and the one next step (data-empty, below).
function emptyHTML(ico, text, act, label){
  return `<div class="empty">${ico}<p>${esc(text)}</p>${act ? `<button class="btn sm" data-empty="${act}">${esc(label)}</button>` : ""}</div>`;
}
document.addEventListener("click", e => {
  const b = e.target.closest("[data-empty]"); if (!b) return;
  ({ paste: () => { setView("jobs"); window.scrollTo({top:0}); $("#urls").focus(); },
     all: () => $('#tabs button[data-f="all"]').click(),
     storage: () => openDrawer("s-storage"),
     clear: () => { $("#libQ").value = ""; L.q = ""; loadLib(); },
     up: () => libGo(L.path.split("/").slice(0, -1).join("/")),
  })[b.dataset.empty]?.();
});

/* ----------------------------------------------------------------- state */
const S = {
  jobs: new Map(), engines: [], cards: [], filter:"all", open: new Set(), info:null, view: LS.get("view","jobs"),
  dest: LS.get("dest","library"),
  defaults: {preset:"best", height:0, subs:false, sponsor:false, embed:true, ...LS.get("defaults",{})},
};

/* ---------------------------------------------------------------- intake */
const URL_RE = /https?:\/\/[^\s<>"'`]+/gi;
function extractUrls(text){
  const out=[]; for (let m of (text.match(URL_RE)||[])) { m=m.replace(/[),.;!?\]]+$/,""); if(!out.includes(m)) out.push(m); } return out;
}
let cardSeq = 0;
function intake(text){
  const urls = extractUrls(text).filter(u => !S.cards.some(c => c.url===u));
  if (!urls.length){ toast(t("nothing_new")); return; }
  for (const url of urls){
    const c = {key:++cardSeq, url, state:"probing", res:null, err:null, opts:{...S.defaults, playlist:false}};
    S.cards.push(c);
  }
  $("#urls").value = "";
  renderCards();
  pumpProbes();
}
let probing = 0;
function startProbe(c, engine){
  c.inflight=true; probing++;
  probe(c, engine).finally(()=>{ probing--; c.inflight=false; pumpProbes(); });
}
function pumpProbes(){
  for (const c of S.cards){
    if (probing>=4) break;
    if (c.state==="probing" && !c.inflight) startProbe(c);
  }
}
async function probe(c, engine){
  try{
    const res = await api("/api/probe", {method:"POST", body:{url:c.url, engine:engine||""}});
    c.res=res; c.err=null; c.state="ready";
    if (res.is_playlist) c.opts.playlist = true;
    if (res.kind==="profile"){ c.opts.sections = [...res.profile.preselect]; c.opts.new_only = LS.get("new_only", true); }
    if (res.kind==="audio" && (c.opts.preset==="best"||c.opts.preset==="compat")) c.opts.preset="mp3";
  }catch(e){ c.err=e.message; c.state="error"; }
  renderCards();
}

/* ----------------------------------------------------------------- cards */
function renderCards(){
  $("#prepSec").classList.toggle("hidden", S.cards.length===0);
  $("#prepCount").textContent = S.cards.length;
  const ready = S.cards.filter(c=>c.state==="ready").length;
  $("#sendAll").classList.toggle("hidden", ready<2);
  const root = $("#cards");
  const keep = new Set(S.cards.map(c=>String(c.key)));
  root.querySelectorAll(".card").forEach(el=>{ if(!keep.has(el.dataset.key)) el.remove(); });
  for (const c of S.cards){
    let el = root.querySelector(`.card[data-key="${c.key}"]`);
    const html = cardHTML(c);
    if (!el){ el=document.createElement("div"); el.dataset.key=c.key; root.append(el); }
    if (el._html !== html){ el._html = html; el.className = "card"+(c.state==="error"?" err":""); el.innerHTML = html; }
  }
}
function kindIcon(k){ return k==="gallery" ? "i-img" : k==="audio" ? "i-note" : "i-film"; }
function cardHTML(c){
  const x = `<button class="iconbtn x" data-act="drop" title="✕">${icon("i-x")}</button>`;
  if (c.state==="probing") return `<div class="thumb noimg">${icon("i-film")}</div><div>
      <div class="title">${esc(c.url)}</div><div class="meta">${esc(t("probing"))}</div>
      <div class="skel u-skel-short"></div></div>${x}`;
  if (c.state==="error") return `<div class="thumb noimg">${icon("i-x")}</div><div>
      <div class="title">${esc(c.url)}</div>
      ${errHTML(c.err)}
      <div class="opts"><button class="btn sm" data-act="reprobe">${icon("i-retry")}${esc(t("retry"))}</button>
      <button class="btn sm ghost" data-act="force" data-e="gallery-dl">${esc(t("try_with",{e:"gallery-dl"}))}</button>
      <button class="btn sm ghost" data-act="force" data-e="yt-dlp">${esc(t("try_with",{e:"yt-dlp"}))}</button></div></div>${x}`;
  if (c.res.kind==="profile") return profileCardHTML(c, x);
  const r=c.res, o=c.opts, isG = r.engine==="gallery-dl";
  const badge = isG ? `${r.count}${r.count_capped?"+":""}` : r.is_playlist ? t("videos",{n:r.count}) : fmtDur(r.duration);
  const thumb = r.thumbnail ? `<img src="${esc(r.thumbnail)}" loading="lazy" referrerpolicy="no-referrer" data-drop>` : "";
  const meta = [r.uploader && esc(r.uploader), esc(r.host),
    `<span class="eng ${isG?"g":"y"}">${esc(r.engine)}</span>`,
    isG && esc(t("items",{n:r.count+(r.count_capped?"+":"")})),
    r.alt && `<button class="linkbtn" data-act="force" data-e="${esc(r.alt)}">${esc(t("try_with",{e:r.alt}))}</button>`
  ].filter(Boolean).join(" · ");
  let opts = "";
  if (!isG){
    const audioOnly = r.kind==="audio";
    const presets = audioOnly ? ["mp3","audio"] : ["best","compat","mp3","audio"];
    opts += `<div class="seg">${presets.map(p=>`<button data-act="preset" data-v="${p}" class="${o.preset===p?"on":""}">${esc(t("p_"+p))}</button>`).join("")}</div>`;
    const isAudio = o.preset==="mp3"||o.preset==="audio";
    if (!isAudio && (r.formats||[]).length > 1 || (!isAudio && r.is_playlist)){
      const hs = (r.formats||[]).length ? r.formats.map(f=>f.height) : [2160,1440,1080,720,480,360];
      opts += `<select class="q" data-act="height" title="${esc(t("quality"))}"><option value="0">${esc(t("q_best"))}</option>${
        hs.map(h=>`<option value="${h}" ${+o.height===h?"selected":""}>${h}p</option>`).join("")}</select>`;
    }
    if (!isAudio) opts += chk("subs", o.subs, t("o_subs"));
    if (/(^|\.)(youtube\.com|youtu\.be)$/.test(r.host)) opts += chk("sponsor", o.sponsor, t("o_sponsor"));
    opts += chk("embed", o.embed, t("o_embed"));
    if (r.can_playlist && !r.is_playlist) opts += chk("playlist", o.playlist, t("o_playlist"));
  }
  opts += chk("night", o.night, t("o_night"));
  const ex = r.existing;
  return `<div class="thumb${thumb?"":" noimg"}">${icon(kindIcon(r.kind))}${thumb}${badge?`<span class="badge">${esc(badge)}</span>`:""}</div>
    <div><div class="title" title="${esc(r.title)}">${esc(r.title)}</div><div class="meta">${meta}</div>${ex?existingHTML(ex):""}
    <div class="opts">${opts}${canSub(r)?`<button class="btn sm" data-act="subscribe">${icon("i-retry")}${esc(t("subscribe"))}</button>`:""}<button class="btn ${ex?"":"primary "}sm go" data-act="send">${icon("i-dl")}${esc(t(ex?(ex.active?"ex_again_q":"ex_again"):"send"))}</button></div></div>${x}`;
}
// Downloaded before (or in the queue): say so and offer the files.
function existingHTML(ex){
  if (ex.active) return `<div class="exline act">⏳ ${esc(t("ex_queued"))}</div>`;
  const when = ex.when ? new Date(ex.when).toLocaleDateString(lang==="tr"?"tr-TR":"en-GB",{day:"numeric",month:"short",year:"numeric"}) : "";
  const f = ex.files[0] || {};
  const more = ex.files.length>1 ? ` · ${esc(t("ex_files",{n:ex.files.length}))}` : "";
  const pv = f.url && previewable(f.kind) ? `<button class="linkbtn" data-act="expv">${esc(t("ex_view"))}</button>` : "";
  return `<div class="exline"><span>✓ ${esc(t("ex_done"))}${when?" · "+esc(when):""} · ${esc(fmtBytes(ex.size))}${more}</span>
    ${f.url?`<a href="${esc(f.url)}" download>${esc(t("ex_get"))}</a>`:""}${pv}</div>`;
}
// Worth subscribing: anything that lists many items over time.
const canSub = r => r.kind==="playlist" || r.is_playlist || (r.engine==="gallery-dl" && r.count>1);
function profileCardHTML(c, x){
  const r=c.res, p=r.profile, o=c.opts;
  const chips = p.sections.map(s=>`<button class="chip-t ${o.sections.includes(s)?"on":""}" data-act="sec" data-v="${s}">${esc(t("sec_"+s))}</button>`).join("");
  const warn = p.has_cookies ? "" : `<div class="warnline">⚠ ${esc(t("ig_no_cookies"))} <button class="linkbtn" data-act="settings">${esc(t("settings"))}</button></div>`;
  return `<div class="thumb avatar"><span>${esc((p.user[0]||"@").toUpperCase())}</span></div>
    <div><div class="title">@${esc(p.user)}</div>
    <div class="meta">Instagram · ${esc(t("profile"))} · <span class="eng g">gallery-dl</span></div>
    <div class="chips">${chips}</div>
    ${o.sections.includes("saved")?`<div class="hint u-mt6">${esc(t("saved_note"))}</div>`:""}${warn}
    <div class="opts">${chk("new_only", o.new_only, t("o_new_only"))}${chk("night", o.night, t("o_night"))}
      <button class="btn sm" data-act="browse">${icon("i-grid")}${esc(t("browse"))}</button>
      <button class="btn sm" data-act="subscribe" ${o.sections.length?"":"disabled"}>${icon("i-retry")}${esc(t("subscribe"))}</button>
      <button class="btn primary sm go" data-act="send" ${o.sections.length?"":"disabled"}>${icon("i-dl")}${esc(t("dl_sections"))}</button></div></div>${x}`;
}
const chk = (k, on, label) => `<label class="chk"><input type="checkbox" data-act="opt" data-k="${k}" ${on?"checked":""}>${esc(label)}</label>`;

$("#cards").addEventListener("click", e => {
  const b = e.target.closest("[data-act]"); if (!b || b.tagName==="SELECT" || b.type==="checkbox") return;
  const c = S.cards.find(x => x.key == b.closest(".card").dataset.key); if (!c) return;
  const act = b.dataset.act;
  if (act==="drop"){ S.cards = S.cards.filter(x=>x!==c); renderCards(); }
  else if (act==="reprobe"){ c.state="probing"; renderCards(); pumpProbes(); }
  else if (act==="force"){ c.state="probing"; renderCards(); startProbe(c, b.dataset.e); }
  else if (act==="preset"){ c.opts.preset=b.dataset.v; rememberDefaults(c.opts); renderCards(); }
  else if (act==="send"){ busy(b, () => send([c])); }
  else if (act==="expv"){ const f=c.res.existing.files[0]; previewOne(f.name, f.url, f.kind); }
  else if (act==="subscribe"){ openSubDialog(c, b); }
  else if (act==="sec"){ const v=b.dataset.v, i=c.opts.sections.indexOf(v); i>=0 ? c.opts.sections.splice(i,1) : c.opts.sections.push(v); renderCards(); }
  else if (act==="browse"){ openBrowse(c); }
  else if (act==="settings"){ openDrawer("s-cookies"); }
});
$("#cards").addEventListener("change", e => {
  const el = e.target; const c = S.cards.find(x => x.key == el.closest(".card")?.dataset.key); if (!c) return;
  if (el.dataset.act==="height"){ c.opts.height = +el.value; rememberDefaults(c.opts); }
  if (el.dataset.act==="opt"){ c.opts[el.dataset.k] = el.checked;
    if (el.dataset.k==="new_only") LS.set("new_only", el.checked);
    else if (el.dataset.k!=="playlist") rememberDefaults(c.opts); }
  renderCards();
});
function rememberDefaults(o){ S.defaults = {preset:o.preset, height:o.height, subs:o.subs, sponsor:o.sponsor, embed:o.embed}; LS.set("defaults", S.defaults); }

async function send(cards){
  cards = cards.filter(c=>c.state==="ready");
  if (cards.length>1) cards = cards.filter(c=>!c.res.existing); // "download all" skips what is already here
  if(!cards.length) return;
  cards = cards.filter(c => c.res.kind!=="profile" || c.opts.sections.length);
  if(!cards.length) return;
  const jobs = cards.map(c => c.res.kind==="profile"
    ? {url:c.res.url, engine:"gallery-dl", title:`@${c.res.profile.user} · ${c.opts.sections.map(s=>t("sec_"+s)).join(", ")}`,
       opts:{mode:S.dest, sections:[...c.opts.sections], new_only:!!c.opts.new_only, night:!!c.opts.night}}
    : {url:c.res.url, engine:c.res.engine, title:c.res.title, thumb:c.res.thumbnail||"",
       expected: c.res.engine==="gallery-dl" ? c.res.count : 0, opts:{...c.opts, mode:S.dest}});
  try{
    const r = await api("/api/jobs", {method:"POST", body:{jobs}});
    (r.added||[]).forEach(j => S.jobs.has(j.id) || upsertJob(j)); // a live event may already be newer
    if (r.errors?.length) toast(r.errors.join("\n"), true);
    S.cards = S.cards.filter(c => !cards.includes(c));
    renderCards(); renderJobs();
    toast(t("added",{n:(r.added||[]).length}));
  }catch(e){ toast(e.message, true); }
}

// The subscribe dialog: how often to check, how many of the newest, and
// whether to skip what is there now (the first check only marks it as seen).
const SD = {card:null, back:null};
const intervalLabel = h => h===168 ? t("sub_weekly") : t("sub_every",{h:hoursLabel(h)});
function openSubDialog(c, from){
  SD.card = c; SD.back = from;
  const opts = (list, cur, fmt) => list.map(v=>`<option value="${v}" ${v===cur?"selected":""}>${esc(fmt(v))}</option>`).join("");
  $("#subEvery").innerHTML = opts(SB.intervals, 6, intervalLabel);
  $("#subLast").innerHTML = opts(SB.limits, 10, n=>t("sub_last",{n}));
  $("#subFromNow").checked = false;
  $("#subWhat").textContent = c.res.kind==="profile" ? `@${c.res.profile.user}` : c.res.title;
  subHint();
  $("#subRoot").classList.remove("hidden");
  $("#subEvery").focus();
}
function subHint(){ $("#subHint").textContent = t($("#subFromNow").checked ? "sub_dlg_mark" : "sub_dlg_now", {n: +$("#subLast").value}); }
function closeSubDialog(){
  $("#subRoot").classList.add("hidden");
  SD.back?.isConnected && SD.back.focus();
  SD.card = SD.back = null;
}
$("#subLast").onchange = subHint;
$("#subFromNow").onchange = subHint;
$("#subScrim").onclick = $("#subClose").onclick = $("#subCancel").onclick = closeSubDialog;
$("#subOk").onclick = e => { const c = SD.card; if (!c) return;
  const choice = {interval_h: +$("#subEvery").value, limit: +$("#subLast").value, from_now: $("#subFromNow").checked};
  busy(e.currentTarget, async () => { if (await subscribe(c, choice)) closeSubDialog(); }); };
// Tab stays inside the dialog
$("#subRoot").addEventListener("keydown", e => {
  if (e.key!=="Tab") return;
  const f = [...$("#subRoot section").querySelectorAll("button,select,input")].filter(x => !x.disabled);
  const i = f.indexOf(document.activeElement);
  if (e.shiftKey && i<=0){ e.preventDefault(); f.at(-1).focus(); }
  else if (!e.shiftKey && i===f.length-1){ e.preventDefault(); f[0].focus(); }
});
async function subscribe(c, choice){
  const r = c.res, prof = r.kind==="profile", {interval_h, limit, from_now} = choice;
  const body = prof
    ? {url:r.url, engine:"gallery-dl", title:`@${r.profile.user}`, interval_h, from_now, opts:{sections:[...c.opts.sections], limit}}
    : {url:r.url, engine:r.engine, title:r.title, thumb:r.thumbnail||"", interval_h, from_now, opts:{...c.opts, limit, playlist: c.opts.playlist || r.is_playlist}};
  try{
    await api("/api/subs", {method:"POST", body});
    S.cards = S.cards.filter(x=>x!==c); renderCards();
    toast(t("subbed"));
    return true;
  }catch(e){ toast(e.message,true); return false; }
}

/* ------------------------------------------------------------------ jobs */
function upsertJob(j){ S.jobs.set(j.id, j); }
const ACTIVE = new Set(["queued","running"]);
function filtered(){
  const all = [...S.jobs.values()].sort((a,b)=> new Date(b.created)-new Date(a.created));
  return all.filter(j => S.filter==="all" || (S.filter==="active" && ACTIVE.has(j.status)) ||
    (S.filter==="done" && j.status==="done") || (S.filter==="error" && (j.status==="error"||j.status==="canceled")));
}
let renderPending=false;
function rerenderJobs(){ for (const el of document.querySelectorAll(".job")) el._html=null; renderJobs(); }
// Batch renders per frame, but never wait on requestAnimationFrame alone:
// browsers pause it for background tabs and unfocused/occluded windows,
// which froze job progress until the page was reloaded.
function renderJobs(){
  if (renderPending) return;
  renderPending = true;
  const run = () => { if (!renderPending) return; renderPending = false; doRenderJobs(); };
  requestAnimationFrame(run);
  setTimeout(run, 250);
}
function doRenderJobs(){
  const list = filtered(), root = $("#jobs");
  const act = [...S.jobs.values()].filter(j=>ACTIVE.has(j.status)).length;
  $("#jobCount").textContent = act ? `${act} / ${S.jobs.size}` : S.jobs.size || "";
  if (!list.length){ root.innerHTML = S.jobs.size ? emptyHTML(`<svg><use href="#logo"/></svg>`, t("no_jobs_f"), "all", t("empty_all"))
      : emptyHTML(`<svg><use href="#logo"/></svg>`, t("no_jobs"), "paste", t("empty_paste")); return; }
  root.querySelector(".empty")?.remove();
  const ids = new Set(list.map(j=>j.id));
  root.querySelectorAll(".job").forEach(el=>{ if(!ids.has(el.dataset.id)) el.remove(); });
  let prev = null;
  for (const j of list){
    let el = root.querySelector(`.job[data-id="${j.id}"]`);
    if (!el){ el=document.createElement("div"); el.className="job"; el.dataset.id=j.id; }
    const html = jobHTML(j);
    if (el._html !== html){
      const open = el.querySelector("details.errtxt")?.open; // a re-render keeps an unfolded error open
      el._html=html; el.innerHTML=html;
      el.querySelectorAll(".bar i[data-w]").forEach(i => { i.style.width = i.dataset.w + "%"; });
      if (open) el.querySelector("details.errtxt")?.setAttribute("open", "");
    }
    if (prev ? prev.nextSibling!==el : root.firstChild!==el) root.insertBefore(el, prev ? prev.nextSibling : root.firstChild);
    prev = el;
  }
}
function jobHTML(j){
  const isG = j.engine==="gallery-dl";
  const kind = isG ? "gallery" : (j.opts.preset==="mp3"||j.opts.preset==="audio") ? "audio" : "video";
  const thumb = j.thumb ? `<img src="${esc(j.thumb)}" loading="lazy" referrerpolicy="no-referrer" data-drop>` : "";
  let stTxt = t("st_"+j.status);
  if (j.status==="running" && j.phase && j.phase!=="downloading") stTxt = t("ph_"+j.phase);
  const nightHold = j.status==="queued" && j.opts.night && !S.queue?.night;
  if (nightHold) stTxt = t("st_night");
  else if (j.status==="queued" && S.queue?.disk_low) stTxt = t("wait_disk");
  // The status and host share one box so a host too long for the line is shortened, not hidden (see app.css).
  const head = `<span class="mi mh"><span class="st ${j.status}">${esc(stTxt)}</span>${j.host ? `<span class="hn"> · ${esc(j.host)}</span>` : ""}</span>`;
  const meta = head + [j.engine && `<span class="eng ${isG?"g":"y"}">${esc(j.engine)}</span>`,
    !isG && esc(t("p_"+j.opts.preset)) + (j.opts.height?` ${j.opts.height}p`:""),
    j.opts.items?.length && esc(t("n_posts",{n:j.opts.items.length})),
    j.opts.new_only && esc(t("only_new")),
    j.opts.mode==="temp" && esc(t("dest_temp")),
    j.status==="queued" && j.not_before && new Date(j.not_before)>Date.now() && esc(t("attempt",{n:j.attempts+1})),
    j.expired && esc(t("expired"))].filter(Boolean).map(x=>`<span class="mi"> · ${x}</span>`).join("");

  let bar="", stats="";
  if (j.status==="running" || j.status==="queued" && j.progress>0){
    const ind = j.progress<0 || (j.status==="running" && j.phase!=="downloading" && j.phase!=="processing");
    bar = `<div class="bar${ind?" ind":""}"><i data-w="${Math.max(2,Math.round((j.progress||0)*100))}"></i></div>`;
    const s=[]; // [kind, text]: on a narrow screen the speed and the time left stay, the rest gives way
    if (j.items>1 || isG) s.push(["n", `${j.item||0}${j.items?"/"+j.items:""}`]);
    if (!isG && j.progress>=0) s.push(["pct", Math.round(j.progress*100)+"%"]);
    if (j.total) s.push(["bytes", `${fmtBytes(j.bytes)} / ${fmtBytes(j.total)}`]);
    if (j.speed) s.push(["speed", fmtBytes(j.speed)+"/s"]);
    if (j.eta) s.push(["eta", fmtDur(j.eta)]);
    stats = `<div class="stats live">${s.map(([k,v])=>`<span class="st-${k}">${esc(v)}</span>`).join("")}</div>`;
  }
  if (j.status==="done") bar = `<div class="bar ok"><i data-w="100"></i></div>`;
  const err = j.error && j.status!=="done" ? errHTML(j.error)
    : j.note ? `<div class="stats">${esc(t("note_"+j.note))}</div>` : "";

  const files = j.file_names||[];
  let actions = "";
  if (nightHold) actions += `<button class="btn sm" data-act="now">${esc(t("start_now"))}</button>`;
  if (ACTIVE.has(j.status)) actions += `<button class="btn sm ghost" data-act="cancel" title="${esc(t("cancel"))}">${icon("i-stop")}</button>`;
  if (j.status==="error"||j.status==="canceled") actions += `<button class="btn sm" data-act="retry">${icon("i-retry")}${esc(t("retry"))}</button>`;
  if (j.status==="done" && files.length===1 && !j.expired) actions += `<a class="btn sm primary" href="/api/jobs/${j.id}/file/0">${icon("i-dl")}${esc(t("get_file"))}</a>`;
  if (j.status==="done" && files.length>1 && !j.expired) actions += `<a class="btn sm primary" href="/api/jobs/${j.id}/zip">${icon("i-zip")}${esc(t("zip"))} · ${files.length}</a>`;
  actions += `<button class="iconbtn" data-act="more" title="${esc(t("open_details"))}">${icon("i-more")}</button>`;

  let details = "";
  if (S.open.has(j.id)){
    const fl = files.length && !j.expired ? `<div class="files">${files.map((f,i)=>`<a href="/api/jobs/${j.id}/file/${i}" title="${esc(f)}">${icon("i-dl")} ${esc(f)}</a>`).join("")}</div>` : "";
    details = `<div class="details"><code>${esc(j.url)}</code>${fl}
      <div class="u-chips">
        <button class="btn sm ghost" data-act="remove">${esc(t("remove"))}</button>
        ${j.opts.mode!=="temp" && files.length ? `<button class="btn sm ghost danger" data-act="remove_files">${esc(t("remove_files"))}</button>`:""}
      </div></div>`;
  }
  return `<div class="thumb">${icon(kindIcon(kind))}${thumb}</div>
    <div class="u-min0"><div class="title" title="${esc(j.title)}">${esc(j.title)}</div><div class="meta">${meta}</div>${bar}${stats}${err}</div>
    <div class="actions">${actions}</div>${details}`;
}
$("#jobs").addEventListener("click", async e => {
  const b = e.target.closest("[data-act]"); if (!b) return;
  const id = b.closest(".job").dataset.id;
  const run = async () => {
    switch (b.dataset.act){
      case "cancel": await api(`/api/jobs/${id}/cancel`, {method:"POST", body:{}}); break;
      case "retry": await api(`/api/jobs/${id}/retry`, {method:"POST", body:{}}); break;
      case "now": await api(`/api/jobs/${id}/now`, {method:"POST", body:{}}); break;
      case "more": S.open.has(id) ? S.open.delete(id) : S.open.add(id); renderJobs(); return;
      case "remove": await api(`/api/jobs/${id}`, {method:"DELETE"}); break;
      case "remove_files": if (confirm(t("confirm_files"))){ await api(`/api/jobs/${id}?files=1`, {method:"DELETE"}); toast(t("deleted")); } break;
    }
    resync();
  };
  try{ await (b.dataset.act === "more" ? run() : busy(b, run)); }
  catch(err){ toast(err.message, true); }
});
$("#tabs").addEventListener("click", e => { const b=e.target.closest("button"); if(!b) return;
  S.filter=b.dataset.f; document.querySelectorAll("#tabs button").forEach(x=>x.classList.toggle("on",x===b)); renderJobs(); });
$("#clearDone").onclick = async () => {
  try{ await api("/api/jobs/clear",{method:"POST",body:{}}); resync(); }catch(e){ toast(e.message,true); }
};

/* --------------------------------------------------------------- engines */
function renderEngines(){
  const e = S.engines, updating = e.some(x=>x.updating), missing = e.some(x=>x.required && !x.available);
  const dot = $("#engDot"); dot.className = "dot " + (updating?"warn pulse":missing?"bad":"ok");
  $("#engTxt").textContent = updating ? t("eng_updating") : missing ? t("eng_missing") : t("eng_ready");
  $("#engPill").title = e.map(x=>`${x.name} ${x.version||"–"}`).join("\n");
  $("#engList").innerHTML = e.map(x => `<div class="erow">
      <i class="dot ${x.updating?"warn pulse":x.available?"ok":x.required?"bad":""}"></i>
      <div><span class="n">${esc(x.name)}</span> <span class="v">${esc(x.version||"–")}</span>
        <div class="meta"><span class="tag">${esc(t("src_"+x.source))}</span>${x.verified?`<span class="tag ok">${esc(t("verified"))}</span>`:""}${x.latest && x.latest!==x.version?`<span class="tag u-accent"">${esc(t("latest",{v:x.latest}))}</span>`:""}${x.held?`<span class="tag">${esc(t("held"))}</span>`:""}
        ${x.source==="managed"?`<span>${esc(t("updated_ago",{t:ago(x.updated)}))}</span>`:""}<span>${esc(t("checked_ago",{t:ago(x.checked)}))}</span></div></div>
      <div class="eact">${x.channel && x.source!=="system"?`<div class="seg sm" title="${esc(t("ch_hint"))}"><button data-ch="stable" class="${x.channel==="stable"?"on":""}">${esc(t("ch_stable"))}</button><button data-ch="nightly" class="${x.channel==="nightly"?"on":""}">${esc(t("ch_nightly"))}</button></div>`:""}
        ${x.held?`<button class="btn sm ghost" data-uh="${esc(x.name)}">${esc(t("unhold"))}</button>`:x.previous?`<button class="btn sm ghost" data-rb="${esc(x.name)}">${esc(t("rollback"))}</button>`:""}</div>
      ${waitUntil(x.rate_limit_until)?`<div class="e wait">${esc(waitUntil(x.rate_limit_until))}</div>`:x.error?`<div class="e">${esc(x.error)}</div>`:""}</div>`).join("");
  renderFoot();
}
function renderFoot(){
  const e = S.engines; if (!e.length) return;
  const last = e.map(x=>x.checked).filter(c=>c && !c.startsWith("0001")).sort().pop();
  const u = S.update||{};
  const self = u.status==="available" ? ` <span class="warn">↑ ${esc(t("newer",{v:u.latest}))}</span>` : u.status==="current" ? ` <span class="ok">✓</span>` : "";
  $("#foot").innerHTML = `<span class="chip"><b>İndirgen</b>${esc(S.version||"")}${self}</span>` + e.map(x => {
    let mark;
    if (x.updating) mark = `<span class="warn">…</span>`;
    else if (!x.available) mark = `<span class="bad">${esc(t("not_installed"))}</span>`;
    else if (x.latest && x.latest!==x.version) mark = `<span class="warn">↑ ${esc(t("newer",{v:x.latest}))}</span>`;
    else if (x.latest) mark = `<span class="ok">✓</span>`;
    else mark = "";
    return `<span class="chip" title="${esc(t("src_"+x.source))}${x.verified?" · sha256 ✓":""}"><b>${esc(x.name)}</b>${esc(x.available?x.version||"?":"")} ${mark}</span>`;
  }).join("") + (last?`<span class="when">${esc(t("last_check",{t:ago(last)}))}</span>`:"");
}
$("#foot").onclick = () => openDrawer("s-engines");
$("#engList").addEventListener("click", async e => {
  const ch=e.target.closest("[data-ch]");
  if (ch){ if (ch.classList.contains("on")) return;
    try{ await api("/api/engines/channel",{method:"POST",body:{channel:ch.dataset.ch}}); toast(t("ch_set",{c:t("ch_"+ch.dataset.ch)})); }
    catch(err){ toast(err.message,true); } return; }
  const b=e.target.closest("[data-rb],[data-uh]"); if(!b) return;
  const url = b.dataset.rb ? `/api/engines/${b.dataset.rb}/rollback` : `/api/engines/${b.dataset.uh}/unhold`;
  try{ await api(url,{method:"POST",body:{}}); toast(t(b.dataset.rb?"saved":"update_started")); }catch(err){ toast(err.message,true); } });
$("#engUpdate").onclick = async () => { try{ const r = await api("/api/engines/update",{method:"POST",body:{}}); toast(waitUntil(r.rate_limit_until) || t("update_started")); }catch(e){ toast(e.message,true); } };
$("#engPill").onclick = () => openDrawer("s-engines");

/* --------------------------------------------------------------- drawer */
/* Settings is a full page at #ayarlar: the back button (and Esc) close it. */
function settingsOpen(){ return !$("#drawerRoot").classList.contains("hidden"); }
function showSettings(on){
  $("#drawerRoot").classList.toggle("hidden", !on);
  document.body.classList.toggle("noscroll", on);
  if (on){ renderEngines(); loadCookies(); loadAbout(); loadConn(); loadTG(); loadSettings(); loadQueue(); loadStorage(); }
  else stopTGPoll();
}
async function openDrawer(section){
  if (location.hash !== "#ayarlar") history.pushState(null, "", "#ayarlar");
  if (!settingsOpen()) showSettings(true);
  if (typeof section === "string") requestAnimationFrame(() => document.getElementById(section)?.scrollIntoView({block:"start"}));
}
function closeDrawer(){ if (location.hash === "#ayarlar") history.back(); else showSettings(false); }
window.addEventListener("popstate", () => { const want = location.hash === "#ayarlar"; if (want !== settingsOpen()) showSettings(want); });
$("#settingsBtn").onclick = () => openDrawer(); $("#closeDrawer").onclick = closeDrawer;
let navLock = 0; // after a click the clicked item stays selected, even if the page cannot scroll that far
function markNav(id){ document.querySelectorAll("#setNav button").forEach(b => b.classList.toggle("on", b.dataset.sec === id)); }
$("#setNav").onclick = e => {
  const b = e.target.closest("[data-sec]"); if (!b) return;
  markNav(b.dataset.sec); navLock = Date.now() + 1200;
  document.getElementById(b.dataset.sec).scrollIntoView({behavior:"smooth", block:"start"});
};
// highlight the section in view
const secObs = new IntersectionObserver(es => {
  if (Date.now() < navLock) return;
  for (const x of es) if (x.isIntersecting) markNav(x.target.id);
}, {root: $("#setPage"), rootMargin: "-90px 0px -60% 0px"});
document.querySelectorAll(".setmain section").forEach(sec => secObs.observe(sec));
document.addEventListener("keydown", e => {
  if (!$("#pvRoot").classList.contains("hidden")){
    if (e.key==="Escape") closePreview();
    else if (e.key==="ArrowLeft") stepPreview(-1);
    else if (e.key==="ArrowRight") stepPreview(1);
    return;
  }
  if (e.key!=="Escape") return;
  if (!$("#subRoot").classList.contains("hidden")) closeSubDialog();
  else if (!$("#dirRoot").classList.contains("hidden")) dirClose();
  else if (!$("#browseRoot").classList.contains("hidden")) closeBrowse(); else if (settingsOpen()) closeDrawer(); });

/* --------------------------------------------------------------- archive */
const L = {path:"", q:"", sort:LS.get("libSort","new"), flat:LS.get("libFlat",false), data:null, files:[], ctl:null, pv:-1,
           selecting:false, sel:new Map()}; // sel: path → {size, dir}
const libURL = (p, extra="") => `/api/library/file?path=${encodeURIComponent(p)}${extra}`;
const previewable = k => k==="video" || k==="audio" || k==="image";
const kindIco = k => k==="video" ? "i-film" : k==="audio" ? "i-note" : k==="image" ? "i-img" : "i-file";

function setView(v){
  if (!["jobs","library","subs"].includes(v)) v = "jobs";
  S.view = v; LS.set("view", v);
  setSeg("#viewSeg", v);
  const jobs = v==="jobs", lib = v==="library", subs = v==="subs";
  $("#jobs").classList.toggle("hidden", !jobs); $("#jobTools").classList.toggle("hidden", !jobs); $("#jobCount").classList.toggle("hidden", !jobs);
  $("#libView").classList.toggle("hidden", !lib); $("#libTools").classList.toggle("hidden", !lib);
  $("#subsView").classList.toggle("hidden", !subs);
  if (lib) loadLib();
  if (subs) loadSubs();
}
$("#viewSeg").onclick = e => { const b=e.target.closest("button"); if (b) setView(b.dataset.v); };

async function loadLib(more=false){
  L.ctl?.abort(); // a newer request wins: the older one stops
  const ctl = L.ctl = new AbortController();
  const q = new URLSearchParams({path:L.path, sort:L.sort, offset: more ? L.files.length : 0});
  if (L.q) q.set("q", L.q);
  if (L.flat) q.set("flat","1");
  try{
    const d = await api("/api/library?"+q, {signal: ctl.signal});
    if (ctl.signal.aborted) return; // aborted while the body was read
    L.data = d; L.files = more ? L.files.concat(d.files) : d.files;
    renderLib();
  }catch(e){
    if (ctl.signal.aborted) return;
    if (L.path){ L.path = ""; loadLib(); } // folder vanished: back to the top
    else toast(e.message, true);
  }
}
let libTimer = null;
function reloadLibSoon(){ if (S.view!=="library") return; clearTimeout(libTimer); libTimer = setTimeout(()=>loadLib(), 400); }

function libRow(x, isDir){
  const on = L.sel.has(x.path);
  const ck = `<input type="checkbox" class="ck" data-act="ck" ${on?"checked":""} aria-label="${esc(x.name)}">`;
  if (isDir){
    const meta = x.empty ? [t("lib_empty_tag")] : [t("lib_files",{n:x.files}), fmtBytes(x.size), ago(x.mod)];
    return `<div class="lrow dir${on?" sel":""}" data-dir="${esc(x.path)}">${ck}
      <div class="ic" data-act="open">${icon("i-folder")}</div>
      <div class="nm" data-act="open"><b>${esc(x.name)}</b><span>${meta.map(esc).join("<i>·</i>")}</span></div>
      <div class="acts">
        ${x.files?`<a class="iconbtn" title="${esc(t("lib_zip"))}" href="/api/library/zip?path=${encodeURIComponent(x.path)}">${icon("i-zip")}</a>`:""}
        <button class="iconbtn danger" data-act="del" title="${esc(t("lib_delete"))}">${icon("i-trash")}</button>
      </div></div>`;
  }
  const where = L.data.flat ? x.path.slice(L.path ? L.path.length+1 : 0, -x.name.length-1) : "";
  const ic = x.kind==="image" ? `<img src="${libURL(x.path,"&inline=1")}" alt="" loading="lazy" data-drop>` : icon(kindIco(x.kind));
  return `<div class="lrow${on?" sel":""}" data-file="${esc(x.path)}">${ck}
    <div class="ic" data-act="view">${ic}</div>
    <div class="nm" data-act="view"><b title="${esc(x.name)}">${esc(x.name)}</b>
      <span>${esc(fmtBytes(x.size))}<i>·</i>${esc(ago(x.mod))}${where?`<code title="${esc(where)}">${esc(where)}/</code>`:""}</span></div>
    <div class="acts">
      ${previewable(x.kind)?`<button class="iconbtn" data-act="view" title="${esc(t("lib_play"))}">${icon("i-play")}</button>`:""}
      <a class="iconbtn" title="${esc(t("lib_download"))}" href="${libURL(x.path)}">${icon("i-dl")}</a>
      <button class="iconbtn danger" data-act="del" title="${esc(t("lib_delete"))}">${icon("i-trash")}</button>
    </div></div>`;
}

function renderLib(){
  const d = L.data; if (!d) return;
  setSeg("#libMode", L.flat ? "flat" : "dirs");
  $("#libSort").value = L.sort;
  // breadcrumb
  const parts = d.path ? d.path.split("/") : [];
  $("#libCrumbs").innerHTML = [`<button data-path="">${esc(t("archive"))}</button>`]
    .concat(parts.map((p,i)=>`<i>›</i><button data-path="${esc(parts.slice(0,i+1).join("/"))}">${esc(p)}</button>`)).join("");
  $("#libStat").textContent = [t("lib_files",{n:d.total.files}), fmtBytes(d.total.size), d.free_bytes>=0 ? t("lib_free",{s:fmtBytes(d.free_bytes)}) : ""]
    .filter(Boolean).join(" · ");
  const rows = (d.flat ? "" : d.dirs.map(x=>libRow(x,true)).join("")) + L.files.map(x=>libRow(x,false)).join("");
  const [empty, act, label] = !d.exists || (d.total.files===0 && !d.path && !d.dirs.length) ? [t("lib_empty"), "storage", t("empty_storage")]
    : L.q ? [t("lib_nomatch"), "clear", t("empty_clear")] : [t("lib_empty_dir"), "up", t("empty_up")];
  $("#libList").innerHTML = rows ? `<div class="lib${L.selecting?" selecting":""}">${rows}</div>` : emptyHTML(icon("i-folder"), empty, act, label);
  renderSelBar();
  const left = d.matches - L.files.length;
  $("#libMore").innerHTML = left>0 ? `<button class="btn sm" id="libMoreBtn">${esc(t("lib_more",{n:left}))}</button>` : "";
}

function renderSelBar(){
  $("#libSelBtn").classList.toggle("on", L.selecting);
  $("#selBar").classList.toggle("hidden", !L.selecting);
  if (!L.selecting) return;
  let size = 0; for (const v of L.sel.values()) size += v.size||0;
  $("#selBar").innerHTML = `<b>${esc(t("lib_sel_n",{n:L.sel.size}))}</b>${L.sel.size?`<span class="libstat">${esc(fmtBytes(size))}</span>`:""}
    <div class="grow"></div>
    <button class="btn sm ghost" data-sa="all">${esc(t("lib_sel_all"))}</button>
    <button class="btn sm ghost" data-sa="none" ${L.sel.size?"":"disabled"}>${esc(t("lib_sel_none"))}</button>
    <button class="btn sm danger" data-sa="del" ${L.sel.size?"":"disabled"}>${icon("i-trash")}${esc(t("lib_sel_del"))}</button>
    <button class="btn sm" data-sa="done">${esc(t("lib_sel_done"))}</button>`;
}
function setSelecting(on){ L.selecting = on; if (!on) L.sel.clear(); renderLib(); }
$("#libSelBtn").onclick = () => setSelecting(!L.selecting);
$("#selBar").onclick = async e => {
  const b = e.target.closest("[data-sa]"); if (!b) return;
  const a = b.dataset.sa;
  if (a==="done") return setSelecting(false);
  if (a==="none"){ L.sel.clear(); return renderLib(); }
  if (a==="all"){
    if (!L.data.flat) L.data.dirs.forEach(d => L.sel.set(d.path, {size:d.size, dir:true}));
    L.files.forEach(f => L.sel.set(f.path, {size:f.size}));
    return renderLib();
  }
  if (a==="del"){
    let size = 0; for (const v of L.sel.values()) size += v.size||0;
    if (!confirm(t("lib_del_many",{n:L.sel.size, s:fmtBytes(size)}))) return;
    try{
      const r = await api("/api/library/delete", {method:"POST", body:{paths:[...L.sel.keys()]}});
      toast(r.bytes>0 ? t("lib_deleted",{s:fmtBytes(r.bytes)}) : t("deleted"));
      if (r.failed?.length) toast(t("lib_del_failed",{n:r.failed.length}) + ": " + r.failed.map(f=>f.path).join(", "), true);
      L.sel.clear(); loadLib();
    }catch(err){ toast(err.message, true); }
  }
};
function libGo(path){
  L.path = path; L.q = ""; $("#libQ").value = ""; L.sel.clear(); loadLib();
  const top = $("#viewSeg").getBoundingClientRect().top;
  if (top < 60) window.scrollTo({top: top + scrollY - 70}); // keep the breadcrumb in view
}
$("#libCrumbs").onclick = e => { const b=e.target.closest("[data-path]"); if (b) libGo(b.dataset.path); };
$("#libMore").onclick = e => { if (e.target.closest("#libMoreBtn")) loadLib(true); };
$("#libSort").onchange = e => { L.sort = e.target.value; LS.set("libSort", L.sort); loadLib(); };
$("#libMode").onclick = e => { const b=e.target.closest("button"); if(!b) return; L.flat = b.dataset.v==="flat"; LS.set("libFlat", L.flat); loadLib(); };
let qTimer = null;
$("#libQ").oninput = e => { clearTimeout(qTimer); qTimer = setTimeout(()=>{ L.q = e.target.value.trim(); loadLib(); }, 250); };

$("#libList").addEventListener("click", async e => {
  const a = e.target.closest("[data-act]"); if (!a) return;
  const row = a.closest(".lrow"), dir = row.dataset.dir, file = row.dataset.file;
  if (a.dataset.act==="ck"){
    const p = dir ?? file;
    if (a.checked){
      const x = dir!==undefined ? L.data.dirs.find(d=>d.path===dir) : L.files.find(f=>f.path===file);
      L.sel.set(p, {size:x?.size||0, dir:dir!==undefined});
    } else L.sel.delete(p);
    row.classList.toggle("sel", a.checked); renderSelBar();
    return;
  }
  if (a.dataset.act==="open") return libGo(dir);
  if (a.dataset.act==="view"){
    const i = L.files.findIndex(f=>f.path===file), f = L.files[i];
    if (previewable(f.kind)) openPreview(i); else location.href = libURL(file);
    return;
  }
  if (a.dataset.act==="del"){
    let msg;
    if (dir!==undefined){
      const x = L.data.dirs.find(d=>d.path===dir);
      msg = x.empty ? t("lib_del_empty",{n:x.name}) : t("lib_del_dir",{n:x.name, c:x.files, s:fmtBytes(x.size)});
    } else msg = t("lib_del_file",{n:L.files.find(f=>f.path===file).name});
    if (!confirm(msg)) return;
    try{
      const r = await api(`/api/library?path=${encodeURIComponent(dir ?? file)}`, {method:"DELETE"});
      toast(r.bytes>0 ? t("lib_deleted",{s:fmtBytes(r.bytes)}) : t("deleted"));
      row.remove(); loadLib();
    }catch(err){ toast(err.message, true); }
  }
});

/* --------------------------------------------------------- subscriptions */
const SB = {list:[], intervals:[1,3,6,12,24,72,168], limits:[5,10,25,50,100]};
function fmtSpan(sec){ sec=Math.max(0,sec); return sec<3600 ? t("m_short",{n:Math.max(1,Math.round(sec/60))}) : sec<86400 ? t("h_short",{n:Math.round(sec/3600)}) : t("d_short",{n:Math.round(sec/86400)}); }
const hoursLabel = h => h<24 ? t("h_short",{n:h}) : t("d_short",{n:h/24});
const inT = iso => t("in_t",{t:fmtSpan((new Date(iso)-Date.now())/1000)});
async function loadSubs(){ try{ const r = await api("/api/subs"); SB.list=r.subs; SB.intervals=r.intervals; SB.limits=r.limits; renderSubs(); }catch(e){ toast(e.message,true); } }
function subRow(x){
  const prof = x.opts.sections?.length;
  const thumb = x.thumb ? `<img src="${esc(x.thumb)}" alt="" loading="lazy" referrerpolicy="no-referrer" data-drop>` : icon(prof?"i-img":"i-film");
  let state;
  if (x.running) state = `<span class="okc">${esc(t("sub_running"))}…</span>`;
  else if (x.paused) state = esc(t("sub_paused"));
  else if (!x.last_check) state = esc(t("sub_never"));
  else state = esc(t("sub_checked",{t:ago(x.last_check)})) + " · " + (x.last_error ? `<span class="bad">${esc(x.last_error)}</span>`
    : esc(x.last_marked ? t("sub_marked") : x.last_new ? t("sub_new",{n:x.last_new}) : t("sub_nonew")));
  const meta = [esc(x.host), `<span class="eng ${x.engine==="gallery-dl"?"g":"y"}">${esc(x.engine||"auto")}</span>`,
    prof && esc(x.opts.sections.map(s=>t("sec_"+s)).join(", ")), state,
    !x.paused && !x.running && x.next && esc(t("sub_next",{t:inT(x.next)})), x.total_new && esc(t("sub_total",{n:x.total_new}))].filter(Boolean).join(" · ");
  const sel = (k, list, cur, fmt) => `<select class="q" data-k="${k}">${list.map(v=>`<option value="${v}" ${v===cur?"selected":""}>${esc(fmt(v))}</option>`).join("")}</select>`;
  return `<div class="lrow sub" data-id="${esc(x.id)}">
    <div class="ic">${thumb}</div>
    <div class="nm"><b title="${esc(x.url)}">${esc(x.title)}</b><span>${meta}</span></div>
    <div class="acts">
      ${sel("interval_h", SB.intervals, x.interval_h, intervalLabel)}
      ${sel("limit", SB.limits, x.opts.limit, n=>t("sub_last",{n}))}
      <button class="iconbtn" data-sb="check" title="${esc(t("sub_check"))}" ${x.running||x.paused?"disabled":""}>${icon("i-retry")}</button>
      <button class="iconbtn" data-sb="pause" title="${esc(t(x.paused?"sub_resume":"sub_pause"))}">${icon(x.paused?"i-play":"i-stop")}</button>
      <button class="iconbtn danger" data-sb="del" title="${esc(t("lib_delete"))}">${icon("i-trash")}</button>
    </div></div>`;
}
function renderSubs(){
  if (S.view!=="subs") return;
  $("#subsList").innerHTML = SB.list.length ? `<div class="lib">${SB.list.map(subRow).join("")}</div>`
    : emptyHTML(icon("i-retry"), t("subs_empty"), "paste", t("empty_paste"));
}
$("#subsList").addEventListener("click", async e => {
  const b = e.target.closest("[data-sb]"); if (!b) return;
  const id = b.closest("[data-id]").dataset.id, x = SB.list.find(s=>s.id===id);
  try{
    if (b.dataset.sb==="check") await api(`/api/subs/${id}/check`, {method:"POST", body:{}});
    if (b.dataset.sb==="pause") await api(`/api/subs/${id}`, {method:"PATCH", body:{paused:!x.paused}});
    if (b.dataset.sb==="del"){ if (!confirm(t("sub_del",{n:x.title}))) return; await api(`/api/subs/${id}`, {method:"DELETE"}); }
    loadSubs();
  }catch(err){ toast(err.message,true); }
});
$("#subsList").addEventListener("change", async e => {
  const el = e.target.closest("select[data-k]"); if (!el) return;
  const id = el.closest("[data-id]").dataset.id;
  try{ await api(`/api/subs/${id}`, {method:"PATCH", body:{[el.dataset.k]: +el.value}}); loadSubs(); }catch(err){ toast(err.message,true); }
});
// keep "next in …" fresh
setInterval(() => { if (S.view==="subs" && SB.list.length) renderSubs(); }, 60000);

/* ------------------------------------------------------- phone & extension */
async function loadConn(){ try{ S.conn = await api("/api/connect"); renderConn(); loadDevices(); }catch{} }
function renderConn(){
  const c = S.conn; if (!c) return;
  const port = (c.listen.match(/:(\d+)$/)||[])[1] || "8899";
  const url = (c.urls[0] || `http://${t("conn_ip")}:${port}`) + "/api/add";
  let head;
  if (c.docker) head = `<div class="hint">${esc(t("conn_docker"))}</div>`;
  else if (!c.can_edit) head = `<div class="row"><b>${esc(t("conn_lan"))}</b><span class="tag">${esc(t(c.lan?"conn_on":"conn_off"))}</span></div><div class="hint">${esc(t("conn_fixed"))}</div>`;
  else head = `<div class="row"><b>${esc(t("conn_lan"))}</b><div class="grow"></div>
      <div class="seg" id="lanSeg"><button data-v="off" class="${c.lan?"":"on"}">${esc(t("conn_off"))}</button><button data-v="on" class="${c.lan?"on":""}">${esc(t("conn_on"))}</button></div></div>`;
  const body = c.lan
    ? `<div class="hint">${esc(t("conn_on_d"))}</div><div class="row">${c.urls.length ? c.urls.map(u=>`<code>${esc(u)}</code>`).join("")
        : `<code>http://${esc(t("conn_ip"))}:${esc(port)}</code><span class="hint">${esc(t("conn_ip_d"))}</span>`}</div>
       <div class="row"><b>${esc(t("conn_token"))}</b><code id="tokTxt">••••••••••••</code>
       <button class="btn sm ghost" id="tokShow">${esc(t("conn_show"))}</button><button class="btn sm ghost" id="tokCopy">${esc(t("conn_copy"))}</button></div>`
    : (c.docker ? "" : `<div class="hint">${esc(t("conn_off_d"))}</div>`);
  const steps = ["ios_1","ios_2","ios_3","ios_4","ios_5"].map(k=>`<li>${t(k,{u:esc(url), k:esc(c.lan?c.token:"TOKEN")})}</li>`).join("");
  const dl = ["dl_1","dl_2","dl_3","dl_4","dl_5"].map(k=>`<li>${t(k,{u:esc(url)})}</li>`).join("");
  const pair = `<div class="row"><button class="btn sm primary" id="pairBtn">${esc(t("pair_btn"))}</button><span class="hint">${esc(t("pair_d"))}</span></div><div id="pairBox"></div>`;
  $("#connPanel").innerHTML = head + pair + body + `<div class="hint">${esc(t("conn_ext",{u:`http://127.0.0.1:${port}`}))}${port!=="8899" ? " "+esc(t("conn_ext_port")) : ""}</div>
    <details><summary>${esc(t("ios_title"))}</summary>${c.lan?"":`<div class="hint u-mt6">${esc(t("ios_need"))}</div>`}<ol>${steps}</ol></details>
    <details><summary>${esc(t("dl_title"))}</summary><ol>${dl}</ol></details>
    <div id="devBox"></div>`;
  renderPair();
  renderDevices();
}
// Connected devices: the browsers signed in with the token or a QR code.
const DV = {list:[], current:"", newToken:""};
async function loadDevices(){
  try{ const r = await api("/api/sessions"); DV.list = r.sessions; DV.current = r.current; renderDevices(); }catch{}
}
function renderDevices(){
  const box = $("#devBox"); if (!box) return;
  const rows = DV.list.map(x => `<div class="devrow"><div class="grow"><b>${esc(x.name || t("dev_unknown"))}</b>
      ${x.id===DV.current ? `<span class="tag">${esc(t("dev_this"))}</span>` : ""}
      <div class="hint">${esc([t(x.via==="qr" ? "dev_via_qr" : "dev_via_token"), x.ip, t("dev_seen",{t: ago(x.last_seen)})].join(" · "))}</div></div>
      <button class="btn sm ghost danger" data-sid="${esc(x.id)}">${esc(t("dev_signout"))}</button></div>`).join("");
  const tok = DV.newToken ? `<div class="row"><b>${esc(t("tok_new"))}</b><code>${esc(DV.newToken)}</code><button class="btn sm ghost" id="tokNewCopy">${esc(t("conn_copy"))}</button></div><div class="hint">${esc(t("tok_new_d"))}</div>` : "";
  box.innerHTML = `<div class="row devhead"><b>${esc(t("dev_title"))}</b></div><div class="hint">${esc(t("dev_desc"))}</div>
    ${rows || `<div class="hint">${esc(t("dev_none"))}</div>`}
    <div class="row"><button class="btn sm ghost danger" id="devOthers" ${DV.list.some(x=>x.id!==DV.current) ? "" : "disabled"}>${esc(t("dev_others"))}</button>
    <button class="btn sm ghost danger" id="tokRotate">${esc(t("tok_rotate"))}</button></div>${tok}`;
}
async function deviceAction(e){
  const sid = e.target.closest("[data-sid]")?.dataset.sid;
  try{
    if (sid){
      if (sid===DV.current && !confirm(t("dev_self_confirm"))) return;
      await api(`/api/sessions/${encodeURIComponent(sid)}`, {method:"DELETE"});
      loadDevices();
    } else if (e.target.closest("#devOthers")){
      if (!confirm(t("dev_others_confirm"))) return;
      const r = await api("/api/sessions/revoke-others", {method:"POST", body:{}});
      toast(t("dev_removed",{n:r.removed})); loadDevices();
    } else if (e.target.closest("#tokRotate")){
      if (!confirm(t("tok_rotate_confirm"))) return;
      const r = await api("/api/token/rotate", {method:"POST", body:{}});
      DV.newToken = r.token || ""; // only this computer is shown the new token
      loadConn(); loadDevices();
    } else if (e.target.closest("#tokNewCopy")) copyText(DV.newToken);
  }catch(err){ toast(err.message, true); }
}
// Phone pairing: a one-time link with its QR code (made by the server).
const PR = {links:[], short:"", until:0, idx:0, msg:"", timer:null};
async function newPair(){
  clearInterval(PR.timer);
  PR.msg = "";
  if (!S.conn?.lan){ PR.links = []; PR.msg = t("pair_need_lan"); renderPair(); return; }
  try{
    const r = await api("/api/pair", {method:"POST", body:{}});
    PR.links = r.links; PR.short = r.short; PR.until = Date.parse(r.expires); PR.idx = 0;
    if (!PR.links.length) PR.msg = t("pair_no_addr");
    PR.timer = setInterval(renderPairLeft, 1000);
  }catch(err){ PR.links = []; PR.msg = err.message; }
  renderPair();
}
function renderPair(){
  const box = $("#pairBox"); if (!box) return;
  if (PR.msg){ box.innerHTML = `<div class="hint">${esc(PR.msg)}</div>`; return; }
  const l = PR.links[PR.idx]; if (!l){ box.innerHTML = ""; return; }
  const sel = PR.links.length > 1 ? `<select id="pairSel">${PR.links.map((x,i)=>`<option value="${i}" ${i===PR.idx?"selected":""}>${esc(new URL(x.url).host)}</option>`).join("")}</select>` : "";
  const short = `<div class="pairshort"><div class="hint">${esc(t("pair_short"))}</div><b>${esc(PR.short)}</b></div>`;
  const qr = `<div class="pairqr">${l.svg}</div>`, link = `${sel}<code>${esc(l.url)}</code>`;
  // In the home-screen app the other phone types the short code; the QR is for a camera.
  box.innerHTML = isStandalone()
    ? `<div class="pair pair-app">${short}<div class="hint" id="pairLeft"></div>
        <details><summary>${esc(t("pair_qr"))}</summary><div class="pair">${qr}<div class="pairtxt"><div class="hint">${esc(t("pair_scan"))}</div>${link}</div></div></details></div>`
    : `<div class="pair">${qr}<div class="pairtxt"><div class="hint">${esc(t("pair_scan"))}</div>
        ${link}${short}<div class="hint" id="pairLeft"></div></div></div>`;
  renderPairLeft();
}
function renderPairLeft(){
  const el = $("#pairLeft"); const left = Math.round((PR.until - Date.now())/1000);
  if (left > 0){ if (el) el.textContent = t("pair_left", {t: fmtDur(left)}); return; }
  clearInterval(PR.timer); PR.links = []; PR.msg = t("pair_expired"); renderPair();
}
async function copyText(txt){
  try{ await navigator.clipboard.writeText(txt); }
  catch{ const ta=document.createElement("textarea"); ta.value=txt; ta.style.position="fixed"; ta.style.opacity="0"; document.body.append(ta); ta.select(); document.execCommand("copy"); ta.remove(); }
  toast(t("copied"));
}
$("#connPanel").addEventListener("change", e => { if (e.target.id==="pairSel"){ PR.idx = +e.target.value; renderPair(); } });
$("#connPanel").addEventListener("click", async e => {
  if (e.target.closest("#tokShow")) $("#tokTxt").textContent = S.conn.token;
  if (e.target.closest("#tokCopy")) copyText(S.conn.token);
  if (e.target.closest("#pairBtn")) busy(e.target.closest("#pairBtn"), newPair);
  const db = e.target.closest("#devBox button");
  if (db) return busy(db, () => deviceAction(e));
  const b = e.target.closest("#lanSeg button"); if (!b || b.classList.contains("on")) return;
  try{
    await api("/api/connect", {method:"POST", body:{lan: b.dataset.v==="on"}});
    $("#connPanel").innerHTML = `<div class="hint">${esc(t("conn_restarting"))}</div>`;
    for (let i=0; i<30; i++){ await new Promise(r=>setTimeout(r,1000)); try{ await fetch("/api/session",{cache:"no-store"}); loadConn(); return; }catch{} }
  }catch(err){ toast(err.message,true); }
});

/* -------------------------------------------------------------- telegram */
let tgTimer = null;
async function loadTG(){ try{ S.tg = await api("/api/telegram"); renderTG(); }catch{} }
function stopTGPoll(){ clearInterval(tgTimer); tgTimer = null; }
function renderTG(){
  const g = S.tg; if (!g) return;
  let h;
  if (!g.has_token){
    h = `<ol>${["tg_s1","tg_s2"].map(k=>`<li>${t(k)}</li>`).join("")}</ol>
      <div class="row"><input type="password" id="tgToken" placeholder="123456789:AA…" autocomplete="off" spellcheck="false">
      <button class="btn sm primary" id="tgSave">${esc(t("tg_save"))}</button></div>`;
  } else {
    const st = g.connected ? `<span class="okc">● ${esc(t("tg_on",{b:"@"+(g.bot||"…")}))}</span>`
      : g.error ? `<span class="badc">● ${esc(g.error)}</span>` : `<span class="hint">● ${esc(t("tg_connecting"))}</span>`;
    h = `<div class="row">${st}<div class="grow"></div><button class="btn sm ghost danger" id="tgRemove">${esc(t("tg_remove"))}</button></div>`;
    if (g.pair_code){
      h += `<div class="hint">${esc(t("tg_pair_d"))}</div><div class="row">
        ${g.pair_link?`<a class="btn sm primary" href="${esc(g.pair_link)}" target="_blank" rel="noopener">${esc(t("tg_pair_open"))}</a><span class="hint">${esc(t("tg_pair_or"))}</span>`:""}
        <code class="big">/start ${esc(g.pair_code)}</code></div>`;
    }
    if (g.chats.length){
      h += `<div class="row"><b>${esc(t("tg_chats"))}</b>${g.chats.map(c=>`<span class="tag">${c} <button class="linkbtn" data-rm="${c}" title="${esc(t("lib_delete"))}">✕</button></span>`).join("")}
          ${g.pair_code?"":`<button class="btn sm ghost" id="tgPair">${esc(t("tg_add_chat"))}</button>`}</div>
        <label class="sw"><input type="checkbox" id="tgNotify" ${g.quiet?"":"checked"}>${esc(t("tg_notify"))}</label>
        <label class="sw"><input type="checkbox" id="tgFiles" ${g.no_files?"":"checked"}>${esc(t("tg_files"))}</label>
        <div class="row"><button class="btn sm" id="tgTest">${esc(t("tg_test"))}</button></div>
        <div class="hint">${esc(t("tg_usage"))}</div>`;
    } else if (!g.pair_code) h += `<div class="row"><button class="btn sm primary" id="tgPair">${esc(t("tg_pair_start"))}</button></div>`;
  }
  $("#tgPanel").innerHTML = h;
  // While a code waits (or the bot is still connecting), watch for changes.
  const waiting = g.pair_code || (g.has_token && !g.connected && !g.error);
  if (waiting && !tgTimer){
    tgTimer = setInterval(async () => {
      const before = (S.tg?.chats||[]).length;
      await loadTG();
      if ((S.tg?.chats||[]).length > before) toast(t("tg_paired"));
    }, 3000);
  }
  if (!waiting) stopTGPoll();
}
$("#tgPanel").addEventListener("click", async e => {
  const b = e.target.closest("button"); if (!b) return;
  try{
    if (b.id==="tgSave"){
      const v = $("#tgToken").value.trim(); if (!v) return;
      b.disabled = true;
      S.tg = await api("/api/telegram", {method:"POST", body:{token:v}});
    } else if (b.id==="tgPair") S.tg = await api("/api/telegram/pair", {method:"POST", body:{}});
    else if (b.id==="tgTest"){ await api("/api/telegram/test", {method:"POST", body:{}}); toast(t("tg_test_ok")); return; }
    else if (b.id==="tgRemove"){ if (!confirm(t("tg_remove_q"))) return; S.tg = await api("/api/telegram", {method:"POST", body:{disconnect:true}}); }
    else if (b.dataset.rm) S.tg = await api("/api/telegram", {method:"POST", body:{remove_chat:+b.dataset.rm}});
    else return;
    renderTG();
  }catch(err){ toast(err.message, true); renderTG(); }
});
$("#tgPanel").addEventListener("change", async e => {
  const k = e.target.id==="tgNotify" ? "quiet" : e.target.id==="tgFiles" ? "no_files" : ""; if (!k) return;
  try{ S.tg = await api("/api/telegram", {method:"POST", body:{[k]: !e.target.checked}}); toast(t("saved")); }catch(err){ toast(err.message, true); }
  renderTG();
});

/* preview: video, audio and images of the current list; ← → step through */
function openPreview(i){
  const f = L.files[i]; if (!f) return;
  L.pv = i;
  $("#pvTitle").textContent = f.name; $("#pvTitle").title = f.path;
  $("#pvDl").href = libURL(f.path);
  const src = libURL(f.path, "&inline=1");
  const media = f.kind==="video" ? `<video src="${src}" controls autoplay playsinline preload="metadata"></video>`
    : f.kind==="audio" ? `<audio src="${src}" controls autoplay></audio>` : `<img src="${src}" alt="">`;
  const nav = (L.files.findIndex((x,j)=>j<i && previewable(x.kind))>=0 ? `<button class="pvnav l" data-step="-1">‹</button>` : "")
    + (L.files.findIndex((x,j)=>j>i && previewable(x.kind))>=0 ? `<button class="pvnav r" data-step="1">›</button>` : "");
  $("#pvBody").innerHTML = media + nav;
  $("#pvBody").querySelector("video,audio,img").addEventListener("error", () => {
    $("#pvBody").innerHTML = `<div class="msg">${esc(t("lib_noplay"))}</div>` + nav; }, {once:true});
  $("#pvRoot").classList.remove("hidden");
}
// One file outside the archive list (e.g. an earlier download on a link card).
function previewOne(name, url, kind){
  L.pv = -1;
  $("#pvTitle").textContent = name; $("#pvTitle").title = name;
  $("#pvDl").href = url;
  const src = url + (url.includes("?")?"&":"?") + "inline=1";
  $("#pvBody").innerHTML = kind==="video" ? `<video src="${esc(src)}" controls autoplay playsinline preload="metadata"></video>`
    : kind==="audio" ? `<audio src="${esc(src)}" controls autoplay></audio>` : `<img src="${esc(src)}" alt="">`;
  $("#pvBody").firstElementChild.addEventListener("error", () => { $("#pvBody").innerHTML = `<div class="msg">${esc(t("lib_noplay"))}</div>`; }, {once:true});
  $("#pvRoot").classList.remove("hidden");
}
function stepPreview(dir){
  if (L.pv < 0) return;
  for (let j=L.pv+dir; j>=0 && j<L.files.length; j+=dir) if (previewable(L.files[j].kind)) return openPreview(j);
}
function closePreview(){ $("#pvRoot").classList.add("hidden"); $("#pvBody").innerHTML = ""; L.pv = -1; }
$("#pvClose").onclick = closePreview; $("#pvScrim").onclick = closePreview;
$("#pvBody").onclick = e => { const b=e.target.closest("[data-step]"); if (b) stepPreview(+b.dataset.step); };
/* ----------------------------------------------------------------- about */
async function loadAbout(){ try{ S.about = await api("/api/about"); S.update = S.about.update; renderAbout(); renderFoot(); }catch{} }
// Changelog lines are plain text with `code` spans.
function mdLine(s){ return esc(s).replace(/`([^`]+)`/g, "<code>$1</code>"); }
function diagText(){
  const a=S.about; if(!a) return "";
  const b=a.build, eng=(S.engines.length?S.engines:a.engines);
  return [`${a.project.display} ${b.version}` + (b.commit?` (${b.commit}${b.dirty?"-dirty":""}${b.date?", "+b.date:""})`:""),
    `Platform: ${b.platform} · ${b.go}`,
    `Data: ${a.data}`, `Library: ${a.library}`,
    `Engines: ` + eng.map(x=>`${x.name} ${x.version||"-"} (${x.source}${x.verified?", sha256":""}${x.held?", held":""})`).join(" · "),
    `Browser: ${navigator.userAgent}`].join("\n");
}
const busy0 = u => u.checking || u.installing || S.restarting;
function updBlock(){
  const u=S.update||{}, a=S.about;
  let cls=u.status||"unknown", title, sub="", act="";
  const checked = u.checked ? t("upd_checked",{t:ago(u.checked)}) : "";
  const inst = a.install||{};
  if (S.restarting){ title=t("upd_restarting"); cls="checking"; }
  else if (u.installing){ title=t("upd_installing",{v:u.latest}); cls="checking"; }
  else if (u.checking){ title=t("upd_checking"); cls="checking"; }
  else switch(u.status){
    case "available": title=t("upd_available",{v:u.latest});
      sub = u.error ? t("upd_failed",{e:u.error}) : inst.reason==="docker" ? t("upd_docker")
          : inst.reason==="readonly" ? t("upd_readonly",{p:inst.path||""}) : t("upd_available_d",{c:a.build.version});
      act = (inst.can ? `<button class="btn sm primary" id="updInstall">${icon("i-dl")}${esc(t("upd_install"))}</button>` : "")
          + `<a class="btn sm${inst.can?" ghost":" primary"}" href="${esc(u.url)}" target="_blank" rel="noopener">${esc(t("upd_release"))}</a>`; break;
    case "current": title=t("upd_current"); sub=t("upd_current_d"); break;
    case "unpublished": title=t("upd_unpublished"); sub=t("upd_unpublished_d"); break;
    case "error": title=t("upd_error"); sub=u.error||""; break;
    default: title=t("upd_unknown"); sub=a.update_check==="off"?t("upd_off"):"";
  }
  const wait = !busy0(u) && waitUntil(u.rate_limit_until); // the last result stands; the next check waits
  if (wait){ sub = wait; if (cls==="error"||cls==="unknown"){ title=t("upd_waiting"); } cls += " wait"; }
  const busy = busy0(u);
  const dot = busy ? "warn pulse" : u.status==="available" || wait ? "warn" : u.status==="current" ? "ok" : u.status==="error" ? "bad" : "";
  return `<div class="ab-upd ${cls}"><i class="dot ${dot}"></i>
    <div class="txt"><b>${esc(title)}</b><span>${esc([sub,checked].filter(Boolean).join(" · "))}</span></div>
    <div class="acts">${busy?"":act}<button class="btn sm${act?" ghost":""}" id="updCheck" ${busy?"disabled":""}>${icon("i-retry")}${esc(t("upd_check"))}</button></div></div>`;
}
function renderAbout(){
  const a=S.about; if(!a) return;
  const p=a.project, b=a.build, repo=`https://github.com/${p.repo}`;
  const kv=[[t("version"),`<code>v${esc(b.version)}</code>`],
    [t("commit"), b.commit?`<code>${esc(b.commit)}${b.dirty?"-dirty":""}</code>`:esc(t("unknown"))],
    [t("built"), esc(b.date||t("unknown"))],
    [t("platform"),`<code>${esc(b.platform)} · ${esc(b.go)}</code>`],
    [t("library"),`<code>${esc(a.library)}</code>`],[t("free"),esc(a.free_bytes>=0?fmtBytes(a.free_bytes):t("unknown"))],
    [t("retention"),esc(t("hours",{n:a.temp_retention}))],[t("data"),`<code>${esc(a.data)}</code>`]]
    .map(([k,v])=>`<span>${esc(k)}</span><span>${v}</span>`).join("");
  const log = (a.changelog||[]).map(e=>`<div class="v">v${esc(e.version)}${e.date?" · "+esc(e.date):""}</div>
      <ul>${e.items.map(i=>`<li>${mdLine(i)}</li>`).join("")}</ul>`).join("");
  const issue = `${repo}/issues/new?body=${encodeURIComponent("\n\n---\n"+diagText())}`;
  $("#aboutPanel").innerHTML = `
    <div class="ab-id"><svg><use href="#logo"/></svg><div>
      <div class="ab-name">${esc(p.display)} <code class="ver">v${esc(b.version)}</code></div>
      <div class="ab-mean">${esc(p.meaning[lang]||p.meaning.tr)}</div></div></div>
    <div class="ab-desc">${esc(p.description[lang]||p.description.tr)}</div>
    ${updBlock()}
    <div class="kv">${kv}</div>
    ${log?`<div class="ab-log"><div class="hd"><b>${esc(t("changelog"))}</b>
      <a class="link" href="${repo}/blob/main/CHANGELOG.md" target="_blank" rel="noopener">${esc(t("changelog_all"))} →</a></div>${log}</div>`:""}
    <div class="ab-links">
      <a href="${esc(p.site)}" target="_blank" rel="noopener">${esc(t("l_site"))}</a>
      <a href="${repo}" target="_blank" rel="noopener">${esc(t("l_repo"))}</a>
      <a href="${esc(issue)}" target="_blank" rel="noopener">${esc(t("l_issue"))}</a>
      <a href="${repo}/blob/main/LICENSE" target="_blank" rel="noopener">${esc(t("l_license",{l:p.license}))}</a>
      <span class="grow"></span>
      <button class="btn sm ghost" id="copyDiag">${esc(t("copy_diag"))}</button>
    </div>`;
}
$("#aboutPanel").addEventListener("click", async e => {
  if (e.target.closest("#updCheck")){
    S.update = {...(S.update||{}), checking:true}; renderAbout();
    try{ S.update = await api("/api/about/check",{method:"POST",body:{}}); }catch(err){ toast(err.message,true); S.update={...S.update,checking:false}; }
    renderAbout(); renderFoot();
  } else if (e.target.closest("#updInstall")){
    const running = [...S.jobs.values()].filter(j=>j.status==="running").length;
    if (running && !confirm(t("upd_confirm",{n:running}))) return;
    S.update = {...S.update, installing:true, error:""}; renderAbout();
    try{
      const r = await api("/api/about/install",{method:"POST",body:{}});
      S.restarting = true; renderAbout();
      // The server restarts with the new binary: reload once it answers with it.
      for (let i=0; i<90; i++){
        await new Promise(res=>setTimeout(res,1000));
        try{ const s = await (await fetch("/api/session",{cache:"no-store"})).json(); if (s.version===r.version){ location.reload(); return; } }catch{}
      }
      S.restarting = false; renderAbout();
    }catch(err){ S.update = {...S.update, installing:false, error:err.message}; renderAbout(); toast(err.message,true); }
  } else if (e.target.closest("#copyDiag")){
    const txt = diagText();
    try{ await navigator.clipboard.writeText(txt); toast(t("copied")); }
    catch{ // http on a LAN address: no async clipboard, fall back to a hidden textarea
      const ta=document.createElement("textarea"); ta.value=txt; ta.style.position="fixed"; ta.style.opacity="0";
      document.body.append(ta); ta.select(); document.execCommand("copy"); ta.remove(); toast(t("copied")); }
  }
});
async function openAbout(){ openDrawer("s-about"); }
$("#appVer").addEventListener("click", e => { e.preventDefault(); e.stopPropagation(); openAbout(); });
/* --------------------------------------------------------------- cookies */
const CK = {content:"", analysis:null, sel:new Set(), refresh:null};
function fmtDate(unix){ return new Date(unix*1000).toLocaleDateString(lang==="tr"?"tr-TR":"en-GB",{day:"numeric",month:"short",year:"numeric"}); }
// status of one site: [css class, text]
function ckStatus(x){
  const days = x.expires ? Math.floor((x.expires*1000 - Date.now())/86400000) : null;
  if (x.known && x.has_login){
    if (x.expired) return ["st-bad", t("ck_login_expired")];
    if (!x.expires) return ["st-ok", t("ck_login_session")];
    if (days < 7) return ["st-warn", t("ck_login_days",{n:Math.max(days,0)})];
    return ["st-ok", t("ck_login_until",{d:fmtDate(x.expires)})];
  }
  if (x.known) return ["st-warn", t("ck_no_login")];
  if (x.expired) return ["st-bad", t("ck_expired")];
  return ["", x.expires ? t("ck_until",{d:fmtDate(x.expires)}) : ""];
}
function ckDot(cls){ return cls==="st-ok" ? "ok" : cls==="st-warn" ? "warn" : cls==="st-bad" ? "bad" : ""; }
async function loadBrowserCookies(){
  try{ S.bc = await api("/api/cookies/browser"); }catch{ return; }
  const b = S.bc, el = $("#bcRow");
  if (!b.available){ el.innerHTML = `<span class="hint">${esc(t("bc_docker"))}</span>`; return; }
  const prof = b.profile ? t("bc_found",{p:b.profile.dir, k:b.profile.install}) : t("bc_none");
  el.innerHTML = `<label class="chkrow"><input type="checkbox" id="bcOn" ${b.enabled?"checked":""} ${b.profile||b.enabled?"":"disabled"}><span>${esc(t("bc_label"))}</span></label>
    <span class="hint">${esc(t("bc_hint"))}</span><span class="hint">${b.profile?`<code>${esc(b.profile.dir)}</code> · ${esc(b.profile.install)}`:esc(prof)}</span>`;
  $("#bcOn").onchange = async e => {
    try{ S.bc = await api("/api/cookies/browser", {method:"POST", body:{enabled:e.target.checked}}); toast(t("saved")); }
    catch(err){ toast(err.message, true); }
    loadBrowserCookies();
  };
}
async function loadCookies(){
  loadBrowserCookies();
  try{
    const list = await api("/api/cookies");
    $("#cookieList").innerHTML = list.length ? list.map(x => {
      const [cls, txt] = ckStatus(x);
      return `<div class="ckrow"><i class="dot ${ckDot(cls)}"></i>
        <div class="nm">${esc(x.label||x.site)}${x.label?`<code>${esc(x.site)}</code>`:""}</div>
        <div class="actions"><button class="btn sm ghost" data-ckref="${esc(x.site)}">${icon("i-retry")}${esc(t("ck_refresh"))}</button>
          <button class="iconbtn" data-ckdel="${esc(x.site)}" title="✕">${icon("i-x")}</button></div>
        <div class="sub">${txt?`<span class="${cls}">${esc(txt)}</span>`:""}<span>${esc(t("ck_count",{n:x.count}))}</span>
          ${x.updated && !x.updated.startsWith("0001") ? `<span>${esc(t("ck_updated",{t:ago(x.updated)}))}</span>` : ""}</div></div>`;
    }).join("") : `<div class="crow hint">${esc(t("no_cookies"))}</div>`;
  }catch{}
}
$("#cookieList").addEventListener("click", async e => {
  const del = e.target.closest("[data-ckdel]"), ref = e.target.closest("[data-ckref]");
  if (del){
    const site = del.dataset.ckdel;
    if (!confirm(t("ck_del_confirm",{s:site}))) return;
    try{ await api(`/api/cookies/${encodeURIComponent(site)}`,{method:"DELETE"}); toast(t("deleted")); loadCookies(); }catch(err){ toast(err.message,true); }
  } else if (ref){
    CK.refresh = ref.dataset.ckref; $("#cookieRefreshFile").value = ""; $("#cookieRefreshFile").click();
  }
});
// "Refresh" on a row: replace only that site's cookies from a new export
$("#cookieRefreshFile").onchange = async e => {
  const f = e.target.files[0], site = CK.refresh; CK.refresh = null; if (!f || !site) return;
  try{
    const content = await f.text();
    const a = await api("/api/cookies/analyze",{method:"POST",body:{content}});
    if (!a.sites.some(x=>x.site===site)){ toast(t("ck_not_in_file",{s:site}), true); return; }
    await api("/api/cookies",{method:"POST",body:{content, sites:[site]}});
    toast(t("ck_refreshed",{s:site})); loadCookies();
  }catch(err){ toast(err.message,true); }
};
async function analyzeCookies(){
  const content = $("#cookieText").value.trim(); if (!content) return;
  try{
    const a = await api("/api/cookies/analyze",{method:"POST",body:{content}});
    CK.content = content; CK.analysis = a;
    // pre-select: a single-site file, or sites already stored (refresh); never unknown extras
    CK.sel = new Set(a.sites.length===1 ? [a.sites[0].site] : a.sites.filter(x=>x.stored).map(x=>x.site));
    renderCookiePreview();
  }catch(err){ toast(err.message,true); }
}
function ckPickRow(x){
  const [cls, txt] = ckStatus(x);
  return `<label><input type="checkbox" data-cks="${esc(x.site)}" ${CK.sel.has(x.site)?"checked":""}>
    <span>${esc(x.label||x.site)}${x.label?`<code>${esc(x.site)}</code>`:""}${x.stored?` <span class="tag">${esc(t("ck_stored"))}</span>`:""}</span>
    <span class="meta">${txt?`<span class="${cls}">${esc(txt)}</span> ·`:""} ${esc(t("ck_count",{n:x.count}))}</span></label>`;
}
function renderCookiePreview(){
  const a = CK.analysis, box = $("#cookiePreview");
  if (!a){ box.classList.add("hidden"); box.innerHTML=""; return; }
  const known = a.sites.filter(x=>x.known), other = a.sites.filter(x=>!x.known);
  const otherSel = other.some(x=>CK.sel.has(x.site));
  box.innerHTML = `<div class="hint">${esc(t("ck_found",{n:a.total, m:a.sites.length, f:a.format==="json"?"JSON":"Netscape"}))}</div>
    ${a.sites.length>1 ? `<div class="warnbox">🔒 ${esc(t("ck_safety"))}</div>` : ""}
    ${known.length ? `<div class="ckpick"><div class="hint u-px2">${esc(t("ck_known"))}</div>${known.map(ckPickRow).join("")}</div>` : ""}
    ${other.length ? (known.length ? `<details ${otherSel?"open":""}><summary>${esc(t("ck_other",{n:other.length}))}</summary><div class="ckpick">${other.map(ckPickRow).join("")}</div></details>`
                                   : `<div class="ckpick">${other.map(ckPickRow).join("")}</div>`) : ""}
    <div class="u-flex u-gap8"><div class="grow"></div>
      <button class="btn sm ghost" id="ckCancel">${esc(t("ck_cancel"))}</button>
      <button class="btn sm primary" id="ckSave" ${CK.sel.size?"":"disabled"}>${esc(t("ck_save_sel",{n:CK.sel.size}))}</button></div>`;
  box.classList.remove("hidden");
}
$("#cookiePreview").addEventListener("change", e => {
  const s = e.target.dataset.cks; if (!s) return;
  e.target.checked ? CK.sel.add(s) : CK.sel.delete(s);
  const b = $("#ckSave"); b.disabled = !CK.sel.size; b.textContent = t("ck_save_sel",{n:CK.sel.size});
});
$("#cookiePreview").addEventListener("click", async e => {
  if (e.target.closest("#ckCancel")){ resetCookieForm(); return; }
  if (!e.target.closest("#ckSave")) return;
  if (!CK.sel.size){ toast(t("ck_none_sel")); return; }
  try{
    const r = await api("/api/cookies",{method:"POST",body:{content:CK.content, sites:[...CK.sel]}});
    toast(t("ck_saved",{n:r.saved.length})); resetCookieForm(); loadCookies();
  }catch(err){ toast(err.message,true); }
});
function resetCookieForm(){ CK.content=""; CK.analysis=null; CK.sel.clear(); $("#cookieText").value=""; renderCookiePreview(); }
$("#cookieAnalyze").onclick = analyzeCookies;
$("#cookieFile").onchange = async e => { const f=e.target.files[0]; if(!f) return; $("#cookieText").value = await f.text(); e.target.value=""; analyzeCookies(); };
$("#cookieText").addEventListener("paste", () => setTimeout(analyzeCookies, 0));
$("#langSeg").onclick = e => { const b=e.target.closest("button"); if(!b) return; lang=b.dataset.v; LS.set("lang",lang); applyI18n(); renderAll(); };
function applyTheme(v){ if (v==="auto") document.documentElement.removeAttribute("data-theme"); else document.documentElement.dataset.theme=v; setSeg("#themeSeg", v); }
$("#themeSeg").onclick = e => { const b=e.target.closest("button"); if(!b) return; LS.set("theme",b.dataset.v); applyTheme(b.dataset.v); };
$("#logoutBtn").onclick = async () => { await api("/api/logout",{method:"POST",body:{}}).catch(()=>{}); location.reload(); };

/* ---------------------------------------------------------- bot settings */
async function loadSettings(){
  try{ S.settings = await api("/api/settings"); renderPace(); }catch{}
}
function paceValues(v){
  const st=S.settings; if(!st) return {r:"?",f:"?"};
  if (v==="custom") return {r:st.custom.sleep_request, f:st.custom.sleep_file};
  const p=st.presets[v]; return {r:p.sleep_request, f:p.sleep_file};
}
let paceSel = null;
function renderPace(){
  const st=S.settings; if(!st) return;
  paceSel = paceSel || st.pacing.pace;
  setSeg("#paceSeg", paceSel);
  const v = paceValues(paceSel);
  $("#paceDesc").textContent = t("pace_desc",{r:v.r, f:v.f});
  $("#paceCustom").classList.toggle("hidden", paceSel!=="custom");
  if (document.activeElement!==$("#pReq")) $("#pReq").value = st.custom.sleep_request;
  if (document.activeElement!==$("#pFile")) $("#pFile").value = st.custom.sleep_file;
  if (document.activeElement!==$("#pMax")) $("#pMax").value = st.pacing.max_items;
}
$("#paceSeg").onclick = e => { const b=e.target.closest("button"); if(!b) return; paceSel=b.dataset.v; renderPace(); };
$("#paceSave").onclick = async () => {
  try{
    await api("/api/settings",{method:"POST",body:{pace:paceSel, sleep_request:$("#pReq").value.trim(),
      sleep_file:$("#pFile").value.trim(), max_items:+$("#pMax").value||0}});
    paceSel=null; await loadSettings(); toast(t("saved"));
  }catch(e){ toast(e.message,true); }
};

/* ---------------------------------------------------------------- browse */
const BR = {card:null, user:"", url:"", tab:"posts", data:{}, sel:new Map(), timer:null, lastIdx:-1};
const PAGE = 48;
function openBrowse(c){
  const p = c.res.profile;
  if (BR.user !== p.user){ BR.data = {}; BR.sel = new Map(); }
  BR.card=c; BR.user=p.user; BR.url=c.res.url;
  BR.tab = c.opts.sections.find(s=>p.browsable.includes(s)) || "posts";
  $("#bTitle").textContent = "@"+p.user;
  $("#bTabs").innerHTML = p.browsable.map(s=>`<button data-s="${s}">${esc(t("sec_"+s))}</button>`).join("");
  $("#browseRoot").classList.remove("hidden");
  if (!S.settings) loadSettings().then(renderBrowse);
  showTab(BR.tab);
}
function closeBrowse(){ $("#browseRoot").classList.add("hidden"); }
function sec(){ return BR.data[BR.tab] || (BR.data[BR.tab] = {items:[], more:true, loading:false, error:null, loaded:false}); }
function showTab(s){
  BR.tab = s; BR.lastIdx = -1;
  document.querySelectorAll("#bTabs button").forEach(b=>b.classList.toggle("on", b.dataset.s===s));
  if (!sec().loaded && !sec().loading) loadPage(); else renderBrowse();
}
async function loadPage(){
  const st = sec(), tab = BR.tab, user = BR.user;
  st.loading = true; st.error = null; st.t0 = Date.now(); renderBrowse();
  clearInterval(BR.timer); BR.timer = setInterval(()=>{ if (BR.tab===tab) renderBrowseInfo(); }, 1000);
  try{
    const r = await api("/api/browse",{method:"POST",body:{url:BR.url, section:tab, start:st.items.length+1, count:PAGE}});
    if (BR.user!==user) return;
    const seen = new Set(st.items.map(i=>i.url));
    for (const it of r.items) if (!seen.has(it.url)) st.items.push(it);
    st.more = r.more; st.loaded = true;
  }catch(e){ st.error = e.message; }
  finally{ st.loading=false; clearInterval(BR.timer); if (BR.tab===tab) renderBrowse(); }
}
function tileHTML(it, i){
  const on = BR.sel.has(it.url);
  const badge = it.type==="carousel" ? `⧉ ${it.count}` : it.type==="video" ? "▶" : "";
  const tip = [it.date, it.likes ? "♥ "+it.likes : "", it.caption].filter(Boolean).join(" · ");
  return `<div class="tile${on?" on":""}" role="checkbox" aria-checked="${on}" tabindex="0" data-i="${i}" title="${esc(tip)}">${icon(it.type==="video"?"i-film":"i-img")}${
    it.thumb?`<img src="${esc(it.thumb)}" alt="" loading="lazy" referrerpolicy="no-referrer" data-drop>`:""}${
    badge?`<span class="tb">${esc(badge)}</span>`:""}<i class="tick">✓</i></div>`;
}
function renderBrowseInfo(){
  const st = sec();
  if (st.loading){
    const secs = Math.round((Date.now()-st.t0)/1000), r = S.settings ? S.settings.pacing.sleep_request : "…";
    $("#bInfo").textContent = t("b_loading",{s:secs}) + " " + t("b_loading_note",{r});
  } else $("#bInfo").textContent = st.loaded ? t("b_loaded",{n:st.items.length}) : "";
}
function renderBrowse(){
  const st = sec(), g = $("#bGrid");
  let html = st.items.map(tileHTML).join("");
  if (st.loading) html += Array.from({length: st.items.length ? 6 : 18}, ()=>`<div class="tile skel"><div class="skel u-skel-fill"></div></div>`).join("");
  if (st.error) html += `<div class="bstate">${errHTML(st.error)}<button class="btn sm u-mt10" id="bRetry">${icon("i-retry")}${esc(t("retry"))}</button></div>`;
  else if (st.loaded && !st.items.length) html += `<div class="bstate">${esc(t("b_empty"))}</div>`;
  g.innerHTML = html;
  $("#bRetry")?.addEventListener("click", loadPage);
  $("#bMore").classList.toggle("hidden", !(st.loaded && st.more) || st.loading);
  $("#bPace").textContent = S.settings ? t("pace_desc",{r:S.settings.pacing.sleep_request, f:S.settings.pacing.sleep_file}) : "";
  renderBrowseInfo(); renderSel();
}
const selectItem = it => BR.sel.set(it.url, {count:it.count||1, thumb:it.thumb});
function renderSel(){
  let n=0, m=0; for (const v of BR.sel.values()){ n++; m += v.count||1; }
  $("#bSel").textContent = n ? t("b_sel",{n, m}) : "";
  $("#bSend").disabled = !n;
}
$("#bTabs").onclick = e => { const b=e.target.closest("button"); if (b) showTab(b.dataset.s); };
$("#bGrid").onclick = e => {
  const b = e.target.closest(".tile[data-i]"); if (!b) return;
  const items = sec().items, i = +b.dataset.i;
  const range = e.shiftKey && BR.lastIdx>=0 ? [Math.min(BR.lastIdx,i), Math.max(BR.lastIdx,i)] : [i,i];
  const turnOn = !BR.sel.has(items[i].url);
  for (let k=range[0]; k<=range[1]; k++){
    const it = items[k];
    if (turnOn) selectItem(it); else BR.sel.delete(it.url);
    const el = $(`#bGrid .tile[data-i="${k}"]`); if (el){ el.classList.toggle("on", turnOn); el.setAttribute("aria-checked", turnOn); }
  }
  BR.lastIdx = i; renderSel();
};
$("#bGrid").addEventListener("keydown", e => { if ((e.key===" "||e.key==="Enter") && e.target.matches(".tile[data-i]")){ e.preventDefault(); e.target.click(); } });
$("#bAll").onclick = () => { sec().items.forEach(selectItem); renderBrowse(); };
$("#bNone").onclick = () => { BR.sel.clear(); renderBrowse(); };
$("#bMore").onclick = loadPage;
$("#bClose").onclick = closeBrowse; $("#bScrim").onclick = closeBrowse;
$("#bSend").onclick = async () => {
  if (!BR.sel.size){ toast(t("b_none_sel")); return; }
  const items = [...BR.sel.keys()], first = BR.sel.values().next().value;
  let expected = 0; for (const v of BR.sel.values()) expected += v.count||1;
  try{
    const r = await api("/api/jobs",{method:"POST",body:{jobs:[{url:BR.url, engine:"gallery-dl",
      title:`@${BR.user} · ${t("n_posts",{n:items.length})}`, thumb:first?.thumb||"", expected,
      opts:{mode:S.dest, items, new_only:false}}]}});
    (r.added||[]).forEach(j => S.jobs.has(j.id) || upsertJob(j)); renderJobs(); // a live event may already be newer
    if (r.errors?.length) toast(r.errors.join("\n"), true); else toast(t("added",{n:1}));
    BR.sel.clear(); renderBrowse(); closeBrowse();
  }catch(e){ toast(e.message,true); }
};

/* ------------------------------------------------- clipboard suggestion */
// When the page gets focus and the clipboard holds a link that is not
// already here, offer it. Chrome asks once for permission (the "From
// clipboard" button); browsers without async clipboard read (Firefox, or
// İndirgen opened over plain http from another device) just keep Ctrl+V.
const CL = {url:"", seen: new Set(LS.get("clipSeen", []))};
function clipSeen(u){ CL.seen.add(u); LS.set("clipSeen", [...CL.seen].slice(-40)); }
function clipKnown(u){
  return CL.seen.has(u) || $("#urls").value.includes(u) || S.cards.some(c => c.url===u || c.res?.url===u) ||
    [...S.jobs.values()].some(j => j.url===u);
}
async function checkClip(manual){
  if (!window.isSecureContext || !navigator.clipboard?.readText) return;
  if (!manual){
    let state = "";
    try{ state = (await navigator.permissions.query({name:"clipboard-read"})).state; }catch{ return; }
    $("#clipAsk").classList.toggle("hidden", state!=="prompt");
    if (state!=="granted") return;
  }
  let text = "";
  try{ text = await navigator.clipboard.readText(); }catch{ return; }
  $("#clipAsk").classList.add("hidden");
  const u = extractUrls(text)[0] || "";
  CL.url = u && !clipKnown(u) ? u : "";
  if (manual && !CL.url) toast(t("clip_none"));
  renderClip();
}
function renderClip(){
  $("#clip").classList.toggle("hidden", !CL.url);
  if (!CL.url) return;
  let shown = CL.url; try{ const x = new URL(CL.url); shown = x.host.replace(/^www\./,"") + x.pathname + x.search; }catch{}
  $("#clipUrl").textContent = shown.length > 70 ? shown.slice(0, 69) + "…" : shown;
  $("#clipUrl").title = CL.url;
}
$("#clipAdd").onclick = () => { const u = CL.url; clipSeen(u); CL.url = ""; renderClip(); intake(u); };
$("#clipX").onclick = () => { clipSeen(CL.url); CL.url = ""; renderClip(); };
$("#clipAsk").onclick = () => checkClip(true);

/* -------------------------------------------------- desktop notifications */
const canNotify = () => window.isSecureContext && "Notification" in window;
function notifyEnd(prev, j){
  if (!LS.get("notify", false) || !canNotify() || Notification.permission!=="granted" || !document.hidden) return;
  if (!prev || !ACTIVE.has(prev.status) || (j.status!=="done" && j.status!=="error")) return;
  const files = (j.file_names||[]).length;
  const body = j.status==="done"
    ? [j.title, files>1 ? t("n_files",{n:files}) : "", j.total ? fmtBytes(j.total) : ""].filter(Boolean).join(" · ")
    : [j.title, (j.error||"").split("\n")[0]].filter(Boolean).join("\n");
  try{
    const n = new Notification((j.status==="done" ? "✓ " + t("nt_done") : "✕ " + t("nt_err")) + " · İndirgen",
      {body, tag:j.id, icon:"/icon.svg", silent: j.status==="done"});
    n.onclick = () => { window.focus(); setView("jobs"); n.close(); };
  }catch{}
}

/* ------------------------------------------------- queue: banner, settings */
function renderBanner(){
  const q = S.queue, el = $("#qBanner"); if (!q){ el.classList.add("hidden"); return; }
  const rows = [];
  if (q.disk_low) rows.push(`<div class="disk"><span>⚠ ${t("qb_disk",{f:esc(fmtBytes(Math.max(q.free,0))), m:esc(fmtBytes(q.min_free_mb*1048576))})}</span>
    <button class="btn sm" data-qb="lib">${esc(t("qb_lib"))}</button><button class="btn sm ghost" data-qb="set">${esc(t("qb_set"))}</button></div>`);
  if (q.waiting > 0 && !q.night) rows.push(`<div><span>${esc(t("qb_night",{n:q.waiting, t:q.night_start}))}</span>
    <button class="btn sm ghost" data-qb="set">${esc(t("qb_set"))}</button></div>`);
  el.innerHTML = rows.join("");
  el.classList.toggle("hidden", !rows.length);
}
$("#qBanner").onclick = e => { const b=e.target.closest("[data-qb]"); if (!b) return;
  if (b.dataset.qb==="lib") setView("library"); else openDrawer("s-queue"); };

const RATES = [0,1,2,5,10,20,50,100], MINS = [512,1024,2048,5120,10240,20480,51200];
async function loadQueue(){ try{ S.queue = await api("/api/queue"); renderQueue(); renderBanner(); }catch{} }
function renderQueue(){
  const q = S.queue; if (!q || !$("#qRate")) return;
  const opt = (v, label, cur) => `<option value="${v}" ${v===cur?"selected":""}>${esc(label)}</option>`;
  if (document.activeElement!==$("#qRate")){
    const rates = RATES.includes(q.rate_limit_mb) ? RATES : [...RATES, q.rate_limit_mb].sort((a,b)=>a-b);
    $("#qRate").innerHTML = rates.map(v => opt(v, v ? v+" MB/s" : t("q_unlimited"), q.rate_limit_mb)).join("");
  }
  if (document.activeElement!==$("#qMin")){
    const mins = MINS.includes(q.min_free_mb) ? MINS : [...MINS, q.min_free_mb].sort((a,b)=>a-b);
    $("#qMin").innerHTML = mins.map(v => opt(v, fmtBytes(v*1048576), q.min_free_mb)).join("");
  }
  if (document.activeElement!==$("#qNightS")) $("#qNightS").value = q.night_start;
  if (document.activeElement!==$("#qNightE")) $("#qNightE").value = q.night_end;
  $("#qFree").textContent = q.free >= 0 ? t("q_free",{f:fmtBytes(q.free)}) : "";
  const ok = canNotify();
  $("#qNotify").disabled = !ok;
  $("#qNotify").checked = ok && LS.get("notify", false) && Notification.permission==="granted";
  $("#qNotifyHint").textContent = !ok ? t("q_notify_no") : Notification.permission==="denied" ? t("q_notify_denied") : t("q_notify_h");
}
$("#qNotify").onchange = async e => {
  if (!e.target.checked){ LS.set("notify", false); return renderQueue(); }
  let p = Notification.permission;
  if (p === "default") p = await Notification.requestPermission();
  LS.set("notify", p === "granted");
  if (p !== "granted") toast(t("q_notify_denied"), true);
  renderQueue();
};
$("#qSave").onclick = async () => {
  try{
    S.queue = await api("/api/queue", {method:"POST", body:{rate_limit_mb:+$("#qRate").value, night_start:$("#qNightS").value,
      night_end:$("#qNightE").value, min_free_mb:+$("#qMin").value}});
    renderQueue(); renderBanner(); rerenderJobs(); toast(t("saved"));
  }catch(e){ toast(e.message, true); }
};

/* ------------------------------------------------- archive folder, picker */
async function loadStorage(){ try{ S.storage = await api("/api/storage"); renderStorage(); }catch{} }
function renderStorage(){
  const st = S.storage; if (!st) return;
  $("#stPath").textContent = st.library;
  $("#stPick").classList.toggle("hidden", !st.can_edit);
  const h = [];
  if (st.reason) h.push(t("st_"+st.reason));
  if (st.pending) h.push(t("st_pending",{p:st.pending}));
  if (st.free >= 0) h.push(t("st_free",{f:fmtBytes(st.free)}));
  if (st.can_edit && !st.pending) h.push(t("st_restart_h"));
  $("#stHint").textContent = h.join(" ");
  setSeg("#stLayout", st.layout);
}
$("#stLayout").onclick = async e => {
  const b = e.target.closest("button"); if (!b || b.dataset.v === S.storage?.layout) return;
  try{ const r = await api("/api/storage", {method:"POST", body:{layout:b.dataset.v}}); S.storage = r.storage; renderStorage(); toast(t("saved")); }
  catch(err){ toast(err.message, true); }
};
const DP = {cur:null};
async function dirGo(path){
  try{ DP.cur = await api("/api/dirs" + (path ? "?path=" + encodeURIComponent(path) : "")); }catch(e){ toast(e.message, true); return; }
  const d = DP.cur;
  $("#dirPath").textContent = d.path;
  $("#dirUp").disabled = !d.parent;
  $("#dirPlaces").innerHTML = d.places.map(p => `<button data-p="${esc(p.path)}" title="${esc(p.path)}">${esc(p.name)}</button>`).join("");
  $("#dirList").innerHTML = d.dirs.length ? d.dirs.map(n => `<button data-n="${esc(n)}">${icon("i-folder")}${esc(n)}</button>`).join("")
    : `<div class="msg">${esc(t("st_empty"))}</div>`;
  $("#dirChoose").disabled = !d.writable;
  $("#dirChoose").title = d.writable ? "" : t("st_ro");
}
function dirOpen(){ $("#dirRoot").classList.remove("hidden"); dirGo(S.storage?.pending || S.storage?.library); }
function dirClose(){ $("#dirRoot").classList.add("hidden"); }
$("#stPick").onclick = dirOpen;
$("#dirClose").onclick = dirClose; $("#dirCancel").onclick = dirClose; $("#dirScrim").onclick = dirClose;
$("#dirUp").onclick = () => DP.cur?.parent && dirGo(DP.cur.parent);
$("#dirPlaces").onclick = e => { const b = e.target.closest("[data-p]"); if (b) dirGo(b.dataset.p); };
$("#dirList").onclick = e => { const b = e.target.closest("[data-n]"); if (b) dirGo(DP.cur.path.replace(/\/$/, "") + "/" + b.dataset.n); };
$("#dirNew").onclick = async () => {
  const name = (prompt(t("st_new_q")) || "").trim(); if (!name) return;
  try{ DP.cur = await api("/api/dirs", {method:"POST", body:{path:DP.cur.path, name}}); dirGo(DP.cur.path); }catch(e){ toast(e.message, true); }
};
$("#dirChoose").onclick = async () => {
  const p = DP.cur?.path; if (!p) return;
  if (p === S.storage.library && !S.storage.pending){ dirClose(); return; }
  if (p !== S.storage.library && !confirm(t("st_confirm",{p}))) return;
  try{
    const r = await api("/api/storage", {method:"POST", body:{library:p}});
    S.storage = r.storage; renderStorage(); dirClose();
    if (r.restart){ toast(t("st_done")); setTimeout(() => location.reload(), 3000); } else toast(t("saved"));
  }catch(e){ toast(e.message, true); }
};

/* ---------------------------------------------------------------- backup */
$("#bkFile").onchange = async e => {
  const f = e.target.files[0]; e.target.value = ""; if (!f) return;
  if (!confirm(t("bk_confirm"))) return;
  try{
    const r = await fetch("/api/backup", {method:"POST", credentials:"same-origin", headers:{"Content-Type":"application/zip"}, body:f});
    const j = await r.json().catch(()=>({}));
    if (!r.ok) throw new Error(j.error || r.statusText);
    toast(t("bk_done",{n:(j.files||[]).length}));
    if (j.restart) setTimeout(() => location.reload(), 3000);
  }catch(err){ toast(err.message, true); }
};

/* ---------------------------------------------------------------- events */
let es = null, esLast = 0;
function connect(){
  es?.close();
  esLast = Date.now();
  es = new EventSource("/api/events");
  // Any message proves the line is alive; the server pings every 20 s.
  for (const ev of ["snapshot","job","removed","queue","engines","library","subs","update","sessions","ping"])
    es.addEventListener(ev, () => { esLast = Date.now(); });
  es.addEventListener("snapshot", e => { const d=JSON.parse(e.data); S.jobs.clear(); d.jobs.forEach(upsertJob); S.engines=d.engines; S.queue=d.queue; renderBanner(); rerenderJobs(); renderEngines(); });
  es.addEventListener("job", e => { const j=JSON.parse(e.data); notifyEnd(S.jobs.get(j.id), j); upsertJob(j); renderJobs(); });
  es.addEventListener("queue", e => { S.queue = JSON.parse(e.data); renderBanner(); rerenderJobs(); if (settingsOpen()) renderQueue(); });
  es.addEventListener("removed", e => { S.jobs.delete(JSON.parse(e.data).id); renderJobs(); });
  es.addEventListener("engines", e => { S.engines = JSON.parse(e.data); renderEngines(); });
  es.addEventListener("library", () => reloadLibSoon());
  es.addEventListener("sessions", () => { if (settingsOpen()) loadDevices(); });
  es.addEventListener("subs", e => { SB.list = JSON.parse(e.data)||[]; renderSubs(); });
  es.addEventListener("update", e => { S.update = JSON.parse(e.data); renderFoot(); if (S.about) renderAbout(); });
  es.onopen = () => { esLast = Date.now(); console.info("[İndirgen] canlı bağlantı açık"); };
  es.onerror = async () => {
    console.warn("[İndirgen] canlı bağlantı hatası, durum:", ["bağlanıyor","açık","kapalı"][es.readyState]);
    try{ const s = await (await fetch("/api/session")).json(); if (!s.authed){ es.close(); showLogin(); return; } }catch{}
    if (es.readyState === EventSource.CLOSED) setTimeout(connect, 3000); // the browser gave up: we do not
  };
}
// A connection can die without an error (laptop sleep, Wi-Fi change, a frozen
// background tab): the page would then show an old state until reloaded.
// Reconnecting sends a fresh snapshot.
function esStale(ms){ return !es || es.readyState === EventSource.CLOSED || Date.now() - esLast > ms; }
setInterval(() => { if (esStale(50000)) connect(); }, 10000);
// Fallback: while the live line is not open (Firefox closes it for good on
// some errors), read the job list every 3 s so the page never sits on an
// old state.
setInterval(() => { if (!document.hidden && (!es || es.readyState !== EventSource.OPEN)) resync(); }, 3000);
document.addEventListener("visibilitychange", () => { if (!document.hidden && esStale(25000)) connect(); });
window.addEventListener("online", connect);
window.addEventListener("pageshow", e => { if (e.persisted) connect(); }); // back/forward cache
// After a button press, show the server's answer without waiting for events.
async function resync(){
  try{
    const list = await api("/api/jobs");
    // An event may have brought a newer state while the list was on its way: keep that one.
    const old = new Map(S.jobs); S.jobs.clear();
    list.forEach(j => { const o = old.get(j.id); upsertJob(o?.rev > j.rev ? o : j); });
    renderJobs();
  }catch{}
}
function renderAll(){ renderStorage(); renderBanner(); renderQueue(); renderClip(); if (CK.analysis) renderCookiePreview(); loadCookies(); if (!$("#browseRoot").classList.contains("hidden")) renderBrowse(); renderPace(); renderCards(); rerenderJobs(); renderEngines(); renderAbout(); renderLib(); renderSubs(); renderConn(); renderTG(); }

/* ----------------------------------------------------------------- boot */
$("#fetchBtn").onclick = () => intake($("#urls").value);
$("#urls").addEventListener("keydown", e => { if (e.key==="Enter" && (e.ctrlKey||e.metaKey)){ e.preventDefault(); intake($("#urls").value); } });
$("#urls").addEventListener("paste", () => setTimeout(() => { const v=$("#urls").value; if (extractUrls(v).length && !/\n\s*$/.test(v) && v.split("\n").filter(Boolean).length===1) intake(v); }, 0));
$("#destSeg").onclick = e => { const b=e.target.closest("button"); if(!b) return; S.dest=b.dataset.v; LS.set("dest",S.dest); setSeg("#destSeg",S.dest); };
$("#sendAll").onclick = () => send(S.cards);
$("#prepClear").onclick = () => { S.cards = S.cards.filter(c=>c.inflight); renderCards(); };

// Opened from the home screen ("Add to Home Screen"): an app with its own cookies.
const isStandalone = () => matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;
// The login screen takes the token or a pairing code; a home-screen app
// starts on the code, since it cannot open the QR link itself.
let loginMode = isStandalone() ? "pair" : "token";
function setLoginMode(m){
  loginMode = m; setSeg("#loginSeg", m);
  const pair = m === "pair", l = $("#loginLabel");
  $("#tokenIn").classList.toggle("hidden", pair); $("#tokenIn").required = !pair;
  $("#pairIn").classList.toggle("hidden", !pair); $("#pairIn").required = pair;
  l.htmlFor = pair ? "pairIn" : "tokenIn"; l.dataset.t = pair ? "login_pair_hint" : "login_hint"; l.textContent = t(l.dataset.t);
}
function showLogin(){ $("#app").classList.add("hidden"); $("#login").classList.remove("hidden"); setLoginMode(loginMode); $(loginMode==="pair" ? "#pairIn" : "#tokenIn").focus(); }
$("#loginSeg").onclick = e => { const b = e.target.closest("button"); if (!b) return; setLoginMode(b.dataset.v); $(loginMode==="pair" ? "#pairIn" : "#tokenIn").focus(); };
$("#loginForm").onsubmit = async e => {
  e.preventDefault();
  try{
    if (loginMode === "pair"){
      // POST /pair answers a good code with a redirect (and the cookie).
      const r = await fetch("/pair", {method:"POST", redirect:"manual", body:new URLSearchParams({code:$("#pairIn").value})});
      if (r.type !== "opaqueredirect") throw new Error(t(r.status===429 ? "login_pair_many" : "login_pair_bad"));
    } else await api("/api/login",{method:"POST",body:{token:$("#tokenIn").value}});
    location.reload();
  }catch(err){ toast(err.message,true); }
};

// Buttons that wait for the server take no second click meanwhile.
for (const id of ["paceSave", "qSave", "engUpdate", "sendAll"]){
  const b = $("#" + id), h = b.onclick;
  b.onclick = e => busy(b, () => h.call(b, e));
}
{ const f = $("#loginForm"), h = f.onsubmit;
  f.onsubmit = e => { e.preventDefault(); busy(f.querySelector("button[data-t=login_btn]"), () => h.call(f, e)); }; }

(async function boot(){
  applyTheme(LS.get("theme","auto"));
  applyI18n();
  setSeg("#destSeg", S.dest);
  let s; try{ s = await (await fetch("/api/session")).json(); }catch{ s={authed:true}; }
  if (!s.authed){ showLogin(); return; }
  S.version = s.version; $("#appVer").textContent = "v" + s.version; document.title = "İndirgen " + s.version;
  $("#app").classList.remove("hidden");
  $("#logoutRow").classList.toggle("hidden", !s.auth_required);
  connect();
  renderJobs();
  setView(S.view);
  if (location.hash === "#ayarlar") showSettings(true);
  // PWA share target / ?url= deep link
  const q = new URLSearchParams(location.search);
  const shared = [q.get("url"), q.get("text"), q.get("title")].filter(Boolean).join("\n");
  if (shared){ history.replaceState(null,"","/"); intake(shared); }
  setInterval(()=>{ if (S.jobs.size) rerenderJobs(); }, 30000);
  checkClip();
  window.addEventListener("focus", () => checkClip());
  document.addEventListener("visibilitychange", () => { if (!document.hidden) checkClip(); });
})();

// Thumbnails that fail to load (expired CDN links) leave no broken image.
document.addEventListener("error", e => { if (e.target instanceof HTMLImageElement && e.target.hasAttribute("data-drop")) e.target.remove(); }, true);
