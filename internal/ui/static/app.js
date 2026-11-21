"use strict";
// Relay dashboard: passkey login, a read-only calendar, the drafts list and a full post preview.
// Every value from the API is rendered with textContent (never innerHTML).

const $ = (id) => document.getElementById(id);
const FR = (navigator.language || "").toLowerCase().startsWith("fr");
const T = FR
  ? { signIn: "Se connecter", addKey: "Ajouter une clé", token: "Jeton API", create: "Créer", week: "Semaine", day: "Jour",
      empty: "Rien de prévu", cancelled: "Annulé.", failed: "Échec", unsupported: "Ce navigateur ne gère pas les passkeys.",
      added: "Clé ajoutée.", dup: "Cette clé existe déjà.", open: "Ouvrir",
      drafts: "Brouillons", draft: "Brouillon", noDrafts: "Aucun brouillon", noDate: "Sans date", plannedFor: "Prévu le",
      scheduledFor: "Programmé le", publishedAt: "Publié le", failedAt: "Échec, prévu le", publishing: "Publication en cours",
      scheduled: "Programmé", published: "Publié", failedS: "Échec", problems: "À corriger avant publication", error: "Erreur",
      attempts: "tentatives", nextTry: "prochain essai", media: "Médias", options: "Options", details: "Détails", more: "plus",
      comment: "Commentaire", title: "Titre", description: "Description", caption: "Légende", local: "heure locale",
      created: "Créé", updated: "Modifié", post: "Post", group: "Groupe", missing: "média introuvable", close: "Fermer",
      readOnly: "Lecture seule · approbation par approve_post", yes: "oui", no: "non", load: "Chargement impossible" }
  : { signIn: "Sign in", addKey: "Add a key", token: "API token", create: "Create", week: "Week", day: "Day",
      empty: "Nothing planned", cancelled: "Cancelled.", failed: "Failed", unsupported: "This browser doesn't support passkeys.",
      added: "Key added.", dup: "This key already exists.", open: "Open",
      drafts: "Drafts", draft: "Draft", noDrafts: "No drafts", noDate: "No date", plannedFor: "Planned for",
      scheduledFor: "Scheduled for", publishedAt: "Published", failedAt: "Failed, was due", publishing: "Publishing",
      scheduled: "Scheduled", published: "Published", failedS: "Failed", problems: "Fix before publishing", error: "Error",
      attempts: "attempts", nextTry: "next try", media: "Media", options: "Options", details: "Details", more: "more",
      comment: "Comment", title: "Title", description: "Description", caption: "Caption", local: "your time",
      created: "Created", updated: "Updated", post: "Post", group: "Group", missing: "media not found", close: "Close",
      readOnly: "Read-only · approve with approve_post", yes: "yes", no: "no", load: "Could not load" };
const NET_NAMES = { x: "X", instagram: "Instagram", facebook: "Facebook", threads: "Threads", "linkedin-page": "LinkedIn",
  youtube: "YouTube", tiktok: "TikTok", website: "Website" };

// Filled 24×24 glyphs, one per network.
const ICONS = {
  x: "M4 4h4.6l4 5.4L17.2 4h2.5l-5.6 6.9L20.2 20h-4.6l-4.3-5.8L6.4 20H3.9l6.1-7.3z",
  instagram: "M7 2h10a5 5 0 0 1 5 5v10a5 5 0 0 1-5 5H7a5 5 0 0 1-5-5V7a5 5 0 0 1 5-5zm0 2a3 3 0 0 0-3 3v10a3 3 0 0 0 3 3h10a3 3 0 0 0 3-3V7a3 3 0 0 0-3-3zm5 3.5a4.5 4.5 0 1 1 0 9 4.5 4.5 0 0 1 0-9zm0 2a2.5 2.5 0 1 0 0 5 2.5 2.5 0 0 0 0-5zm5.3-4a1.2 1.2 0 1 1 0 2.4 1.2 1.2 0 0 1 0-2.4z",
  youtube: "M21.6 7.2a2.7 2.7 0 0 0-1.9-1.9C18 4.8 12 4.8 12 4.8s-6 0-7.7.5a2.7 2.7 0 0 0-1.9 1.9A28 28 0 0 0 2 12a28 28 0 0 0 .4 4.8 2.7 2.7 0 0 0 1.9 1.9c1.7.5 7.7.5 7.7.5s6 0 7.7-.5a2.7 2.7 0 0 0 1.9-1.9A28 28 0 0 0 22 12a28 28 0 0 0-.4-4.8zM10 15V9l5.2 3z",
  tiktok: "M16.5 2h-3.3v13.2a2.9 2.9 0 1 1-2.9-2.9c.3 0 .6 0 .9.1V9a6.3 6.3 0 1 0 5.3 6.2V8.6a7.9 7.9 0 0 0 4.5 1.4V6.7a4.6 4.6 0 0 1-4.5-4.7z",
  facebook: "M22 12a10 10 0 1 0-11.6 9.9v-7H7.9V12h2.5V9.8c0-2.5 1.5-3.9 3.8-3.9 1.1 0 2.2.2 2.2.2v2.5h-1.3c-1.2 0-1.6.8-1.6 1.6V12h2.8l-.4 2.9h-2.3v7A10 10 0 0 0 22 12z",
  website: "M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zm6.9 9h-3a15 15 0 0 0-1.3-5.6A8 8 0 0 1 18.9 11zM12 4c.9 1.2 1.8 3.6 1.9 7h-3.8c.1-3.4 1-5.8 1.9-7zM9.4 5.4A15 15 0 0 0 8.1 11h-3a8 8 0 0 1 4.3-5.6zM5.1 13h3a15 15 0 0 0 1.3 5.6A8 8 0 0 1 5.1 13zM12 20c-.9-1.2-1.8-3.6-1.9-7h3.8c-.1 3.4-1 5.8-1.9 7zm2.6-1.4a15 15 0 0 0 1.3-5.6h3a8 8 0 0 1-4.3 5.6z",
};
const KEY_ICON = "M15 7a3 3 0 1 1-6 0 3 3 0 0 1 6 0zM12 10v11m0-4h3m-3-3h2";
const EXT_ICON = "M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5";
const CLOSE_ICON = "M6 6l12 12M18 6L6 18";
const WARN_ICON = "M12 9v4m0 4h.01M10.3 3.9L2.4 18a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z";

const mobile = matchMedia("(max-width: 760px)");
const state = { view: "week", day: startOfDay(new Date()), posts: [], loadedFrom: null, drafts: [] };

// ---------- helpers ----------

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}
function svg(d, { filled = false, evenodd = false } = {}) {
  const s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  s.setAttribute("viewBox", "0 0 24 24");
  s.setAttribute("aria-hidden", "true");
  const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
  p.setAttribute("d", d);
  if (filled) { s.style.fill = "currentColor"; s.style.stroke = "none"; }
  if (evenodd) p.setAttribute("fill-rule", "evenodd");
  s.append(p);
  return s;
}
function netIcon(p) {
  const s = svg(ICONS[p.provider] || ICONS.website, { filled: true, evenodd: true });
  s.setAttribute("aria-label", p.provider || "");
  return s;
}
const netColor = (p) => (ICONS[p.provider] ? `var(--${p.provider})` : "var(--muted)");

function b64uToBuf(s) {
  s = s.replace(/-/g, "+").replace(/_/g, "/");
  while (s.length % 4) s += "=";
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}
function bufToB64u(buf) {
  let bin = "";
  for (const b of new Uint8Array(buf)) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function api(path, { method = "GET", body, token } = {}) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (token) headers["Authorization"] = "Bearer " + token;
  const res = await fetch("/ui/api/" + path, {
    method, headers, credentials: "same-origin", cache: "no-store",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) { const e = new Error(data.error || res.statusText); e.status = res.status; throw e; }
  return data;
}

function say(text, err) { const m = $("login-msg"); m.textContent = text; m.className = "msg" + (err ? " err" : ""); }

// ---------- login / registration ----------

async function login() {
  if (!window.PublicKeyCredential) return say(T.unsupported, true);
  say("");
  try {
    const opts = await api("login/begin", { method: "POST", body: {} });
    const pk = opts.publicKey;
    pk.challenge = b64uToBuf(pk.challenge);
    (pk.allowCredentials || []).forEach((c) => (c.id = b64uToBuf(c.id)));
    const cred = await navigator.credentials.get({ publicKey: pk });
    const r = cred.response;
    await api("login/finish", { method: "POST", body: {
      id: cred.id, rawId: bufToB64u(cred.rawId), type: cred.type,
      response: { authenticatorData: bufToB64u(r.authenticatorData), clientDataJSON: bufToB64u(r.clientDataJSON),
        signature: bufToB64u(r.signature), userHandle: r.userHandle ? bufToB64u(r.userHandle) : "" },
    } });
    showApp();
  } catch (e) {
    say(e.name === "NotAllowedError" ? T.cancelled : `${T.failed}: ${e.message}`, e.name !== "NotAllowedError");
  }
}

async function register(ev) {
  ev.preventDefault();
  const tokenEl = $("reg-token");
  const token = tokenEl.value.trim();
  tokenEl.value = ""; // never keep the token around
  if (!token) return;
  try {
    const opts = await api("register/begin", { method: "POST", body: { name: navigator.platform || "passkey" }, token });
    const pk = opts.publicKey;
    pk.challenge = b64uToBuf(pk.challenge);
    pk.user.id = b64uToBuf(pk.user.id);
    (pk.excludeCredentials || []).forEach((c) => (c.id = b64uToBuf(c.id)));
    const cred = await navigator.credentials.create({ publicKey: pk });
    const r = cred.response;
    await api("register/finish", { method: "POST", token, body: {
      id: cred.id, rawId: bufToB64u(cred.rawId), type: cred.type,
      response: { attestationObject: bufToB64u(r.attestationObject), clientDataJSON: bufToB64u(r.clientDataJSON),
        transports: r.getTransports ? r.getTransports() : [] },
    } });
    $("register").hidden = true;
    say(T.added);
  } catch (e) {
    say(e.name === "InvalidStateError" ? T.dup : e.name === "NotAllowedError" ? T.cancelled : `${T.failed}: ${e.message}`, true);
  }
}

async function logout() {
  await api("logout", { method: "POST", body: {} }).catch(() => {});
  location.reload();
}

// ---------- dates ----------

function startOfDay(d) { const x = new Date(d); x.setHours(0, 0, 0, 0); return x; }
function addDays(d, n) { const x = new Date(d); x.setDate(x.getDate() + n); return x; }
function startOfWeek(d) { const x = startOfDay(d); return addDays(x, -((x.getDay() + 6) % 7)); }
function sameDay(a, b) { return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate(); }
const L = navigator.language || "en";
const fTime = new Intl.DateTimeFormat(L, { hour: "numeric", minute: "2-digit" });
const fHour = new Intl.DateTimeFormat(L, { hour: "numeric" });
const fWd = new Intl.DateTimeFormat(L, { weekday: "short" });
const fWdNarrow = new Intl.DateTimeFormat(L, { weekday: "narrow" });
const fDay = new Intl.DateTimeFormat(L, { weekday: "short", day: "numeric", month: "short" });
const fRange = new Intl.DateTimeFormat(L, { day: "numeric", month: "short" });
const fLong = new Intl.DateTimeFormat(L, { weekday: "long", day: "numeric", month: "long", hour: "numeric", minute: "2-digit" });

const showDay = () => mobile.matches || state.view === "day";

// ---------- data ----------

async function loadDrafts() {
  try {
    const data = await api("drafts");
    state.drafts = data.groups || [];
  } catch (e) {
    if (e.status === 401) return showLogin();
    state.drafts = [];
  }
  const n = state.drafts.length;
  $("drafts-count").hidden = !n;
  $("drafts-count").textContent = String(n);
  if (state.view === "drafts") render();
}

async function load() {
  if (state.view === "drafts") return render();
  const from = startOfWeek(state.day);
  if (state.loadedFrom && +state.loadedFrom === +from) return render();
  const to = addDays(from, 7);
  try {
    const data = await api(`calendar?from=${encodeURIComponent(from.toISOString())}&to=${encodeURIComponent(to.toISOString())}`);
    state.posts = (data.posts || []).map((p) => ({ ...p, date: new Date(p.at) })).sort((a, b) => a.date - b.date);
    state.loadedFrom = from;
  } catch (e) {
    if (e.status === 401) return showLogin();
    state.posts = [];
  }
  render();
}

// ---------- rendering ----------

function thumb(p) {
  const m = p.media.find((m) => m.mime.startsWith("image/")) || p.media[0];
  if (!m) return null;
  if (m.mime.startsWith("video/")) {
    const v = el("video", "th");
    v.src = m.url + "#t=0.1"; v.muted = true; v.preload = "metadata"; v.playsInline = true;
    return v;
  }
  const i = el("img", "th");
  i.src = m.url; i.alt = ""; i.loading = "lazy"; i.decoding = "async";
  return i;
}

function postCard(p, inHour) {
  const status = p.source === "imported" ? "published" : p.status;
  const c = el("button", "post " + status);
  c.type = "button";
  c.style.setProperty("--net", netColor(p));
  const body = el("div");
  const meta = el("div", "meta");
  meta.append(netIcon(p));
  // in the day timeline the hour gutter already says the time; only show it when it adds minutes
  if (!inHour || p.date.getMinutes()) meta.append(el("b", "", fTime.format(p.date)));
  if (p.parts.length > 1) meta.append(el("span", "", `· ${p.parts.length}`));
  if (status === "draft") meta.append(el("span", "tag", T.draft));
  if (status === "failed") meta.append(el("i", "flag"));
  body.append(meta, el("div", "txt", p.title || p.parts[0] || ""));
  c.append(body);
  const t = thumb(p);
  if (t) c.append(t);
  c.addEventListener("click", () => openPreview(p));
  return c;
}

function renderHeader() {
  const drafts = state.view === "drafts";
  $("v-drafts").setAttribute("aria-pressed", drafts);
  $("prev").hidden = $("next").hidden = drafts;
  $("strip").hidden = drafts;
  $("v-week").setAttribute("aria-selected", state.view === "week");
  $("v-day").setAttribute("aria-selected", state.view === "day");
  if (drafts) { $("range").textContent = T.drafts; $("range").classList.remove("away"); return; }
  const from = startOfWeek(state.day);
  const to = addDays(from, 6);
  const range = $("range");
  if (!mobile.matches && state.view === "day") range.textContent = fDay.format(state.day);
  else range.textContent = fRange.formatRange ? fRange.formatRange(from, to) : `${fRange.format(from)} – ${fRange.format(to)}`;
  const today = new Date();
  const away = showDay() ? !sameDay(state.day, today) : +startOfWeek(today) !== +from;
  range.classList.toggle("away", away);
  range.title = away ? "↺" : "";
}

function renderStrip() {
  const strip = $("strip");
  strip.replaceChildren();
  const from = startOfWeek(state.day);
  const today = new Date();
  for (let i = 0; i < 7; i++) {
    const d = addDays(from, i);
    const b = el("button", "day-cell" + (sameDay(d, state.day) ? " sel" : "") + (sameDay(d, today) ? " today" : ""));
    b.type = "button";
    const dots = el("span", "dots");
    const nets = [...new Set(state.posts.filter((p) => sameDay(p.date, d)).map(netColor))].slice(0, 4);
    nets.forEach((c) => { const i = el("i"); i.style.setProperty("--net", c); dots.append(i); });
    b.append(el("span", "wd", fWdNarrow.format(d)), el("span", "n", String(d.getDate())), dots);
    b.addEventListener("click", () => { state.day = d; render(); });
    strip.append(b);
  }
}

function render() {
  renderHeader();
  if (state.view === "drafts") return $("cal").replaceChildren(renderDrafts());
  renderStrip();
  $("cal").replaceChildren(showDay() ? renderDay() : renderWeek());
}

// ---------- drafts ----------

function renderDrafts() {
  const box = el("div", "drafts");
  if (!state.drafts.length) { box.append(el("div", "empty", T.noDrafts)); return box; }
  for (const g of state.drafts) {
    const c = el("button", "dcard");
    c.type = "button";
    const head = el("div", "dc-head");
    head.append(el("span", g.planned_at ? "dc-when" : "dc-when undated", g.planned_at ? fLong.format(new Date(g.planned_at)) : T.noDate));
    const probs = g.posts.reduce((n, p) => n + p.problems, 0);
    if (probs) { const w = el("span", "warn"); w.append(svg(WARN_ICON), document.createTextNode(String(probs))); head.append(w); }
    const nets = el("div", "dc-nets");
    for (const p of g.posts) {
      const chip = el("span", "chip" + (p.problems ? " bad" : ""));
      chip.style.setProperty("--net", netColor(p));
      chip.append(netIcon(p), el("span", "", p.handle ? "@" + p.handle : NET_NAMES[p.provider] || p.provider));
      nets.append(chip);
    }
    const lead = g.posts[0];
    const body = el("div", "dc-body");
    const txt = el("div", "txt");
    if (g.title) txt.append(el("b", "", g.title + " "));
    txt.append(document.createTextNode(lead.text || ""));
    body.append(txt);
    const m = g.posts.map((p) => p.media).find(Boolean);
    if (m) body.append(thumb({ media: [m] }));
    c.append(head, nets, body);
    c.addEventListener("click", () => openPreview({ id: lead.id }));
    box.append(c);
  }
  return box;
}

function renderWeek() {
  const grid = el("div", "week");
  const from = startOfWeek(state.day);
  const today = new Date();
  for (let i = 0; i < 7; i++) {
    const d = addDays(from, i);
    const col = el("section", "col");
    const head = el("div", "col-head" + (sameDay(d, today) ? " today" : ""));
    head.append(el("span", "", fWd.format(d)), el("span", "n", String(d.getDate())));
    head.addEventListener("click", () => { state.day = d; state.view = "day"; render(); });
    col.append(head);
    const ps = state.posts.filter((p) => sameDay(p.date, d));
    if (!ps.length) col.append(el("div", "none", "·"));
    ps.forEach((p) => col.append(postCard(p)));
    grid.append(col);
  }
  return grid;
}

function renderDay() {
  const box = el("div", "day");
  const ps = state.posts.filter((p) => sameDay(p.date, state.day));
  if (!ps.length) { box.append(el("div", "empty", T.empty)); return box; }
  const now = new Date();
  const isToday = sameDay(state.day, now);
  let nowDrawn = !isToday;
  const hours = new Map();
  ps.forEach((p) => { const h = p.date.getHours(); if (!hours.has(h)) hours.set(h, []); hours.get(h).push(p); });
  for (const [h, list] of hours) {
    if (!nowDrawn && h > now.getHours()) { const n = el("div", "now"); n.append(el("span"), el("i")); box.append(n); nowDrawn = true; }
    const row = el("div", "hour");
    const at = new Date(state.day); at.setHours(h, 0, 0, 0);
    const t = el("time", "", fHour.format(at));
    const col = el("div", "col");
    list.forEach((p) => col.append(postCard(p, true)));
    row.append(t, col);
    box.append(row);
  }
  if (!nowDrawn) { const n = el("div", "now"); n.append(el("span"), el("i")); box.append(n); }
  return box;
}

function safeURL(u) {
  try { const x = new URL(u); return x.protocol === "https:" || x.protocol === "http:" ? x.href : null; } catch { return null; }
}

// ---------- preview ----------
// The full preview of a post group: every account, complete text, media, options, pre-flight problems.
// Layouts mimic how each network arranges a post (thread, caption cut, title...) with neutral styling.

function fmtIn(tz, d, opts) {
  try { return new Intl.DateTimeFormat(L, { ...opts, timeZone: tz }).format(d); } catch { return new Intl.DateTimeFormat(L, opts).format(d); }
}
const LONG_OPTS = { weekday: "long", day: "numeric", month: "long", year: "numeric", hour: "numeric", minute: "2-digit" };

// whenLine: "Planned for Tuesday 3 December 2026, 10:00 (Europe/Paris)" and the viewer's time when it differs.
function whenLine(post, tz) {
  const [label, at] =
    post.status === "draft" ? [T.plannedFor, post.planned_at] :
    post.status === "published" ? [T.publishedAt, post.published_at] :
    post.status === "failed" ? [T.failedAt, post.scheduled_at] :
    [T.scheduledFor, post.scheduled_at];
  const box = el("div", "pv-when");
  if (!at) { box.append(el("span", "", post.status === "draft" ? T.noDate : "")); return box; }
  const d = new Date(at);
  box.append(el("span", "", `${label} ${fmtIn(tz, d, LONG_OPTS)}`), el("span", "tz", tz));
  const mine = Intl.DateTimeFormat().resolvedOptions().timeZone;
  if (mine && mine !== tz && fmtIn(mine, d, LONG_OPTS) !== fmtIn(tz, d, LONG_OPTS)) {
    box.append(el("span", "alt-time", `${fmtIn(mine, d, LONG_OPTS)} (${T.local})`));
  }
  return box;
}

function statusPill(status) {
  const label = { draft: T.draft, scheduled: T.scheduled, publishing: T.publishing, published: T.published, failed: T.failedS }[status] || status;
  return el("span", "pill " + status, label);
}

function counter(t) {
  if (!t.limit && t.unit !== "bytes") return el("span", "count", String(t.count));
  const c = el("span", "count" + (t.limit && t.count > t.limit ? " over" : ""), t.limit ? `${t.count} / ${t.limit}` : String(t.count));
  if (t.unit === "bytes") c.append(document.createTextNode(" B"));
  return c;
}

// foldedText shows what the network displays before its "more" cut, with a toggle to the full text.
function foldedText(t, cls) {
  const box = el("div", cls);
  const chars = Array.from(t.text);
  // The cut comes at the fold or at the first line break before it, whichever is first.
  let cut = t.fold || 0;
  const nl = chars.indexOf("\n");
  if (cut && nl >= 0 && nl < cut) cut = nl;
  if (cut >= chars.length) cut = 0;
  if (cut && cut === t.fold) { // end on a word, like the networks do
    const sp = chars.slice(0, cut).lastIndexOf(" ");
    if (sp > cut - 25) cut = sp;
  }
  if (!cut) { box.textContent = t.text; return box; }
  const short = el("span", "", chars.slice(0, cut).join("").trimEnd());
  const more = el("button", "more", `… ${T.more}`);
  more.type = "button";
  more.addEventListener("click", () => { box.replaceChildren(document.createTextNode(t.text)); });
  box.append(short, more);
  return box;
}

function mediaEl(m, { controls = true } = {}) {
  if (m.missing) return el("div", "m-missing", T.missing);
  if ((m.mime || "").startsWith("video/")) {
    const v = el("video"); v.src = m.path; v.controls = controls; v.preload = "metadata"; v.playsInline = true;
    return v;
  }
  const i = el("img"); i.src = m.path; i.alt = m.filename || ""; i.decoding = "async";
  return i;
}

function carousel(media) {
  const g = el("div", "pv-media");
  media.forEach((m, i) => {
    const f = el("figure");
    f.append(mediaEl(m));
    if (media.length > 1) f.append(el("span", "idx", `${i + 1}/${media.length}`));
    g.append(f);
  });
  return g;
}

function who(acc) {
  const w = el("div", "mk-who");
  const av = el("span", "av", ((acc && (acc.name || acc.handle)) || "?").slice(0, 1).toUpperCase());
  const n = el("div", "names");
  n.append(el("b", "", (acc && acc.name) || "?"));
  if (acc && acc.handle) n.append(el("span", "", "@" + acc.handle));
  w.append(av, n);
  return w;
}

function textBlock(t, cls, label) {
  const b = el("div", "mk-block");
  const head = el("div", "mk-label");
  head.append(el("span", "", label), counter(t));
  b.append(head, t.fold ? foldedText(t, cls) : el("div", cls, t.text));
  return b;
}

function mock(pp) {
  const acc = pp.account;
  const m = el("div", "mock " + pp.layout);
  const labels = { post: T.post, caption: T.caption, title: T.title, description: T.description, comment: T.comment, body: "" };
  switch (pp.layout) {
    case "thread":
      pp.texts.forEach((t, i) => {
        const item = el("div", "mk-item");
        item.append(who(acc));
        item.append(textBlock(t, "mk-text", pp.texts.length > 1 ? `${i + 1}/${pp.texts.length}` : ""));
        if (i === 0 && pp.media.length) item.append(carousel(pp.media));
        m.append(item);
      });
      break;
    case "video":
      if (pp.media.length) m.append(carousel(pp.media));
      if (pp.title) m.append(textBlock(pp.title, "mk-title", labels.title));
      pp.texts.forEach((t) => m.append(textBlock(t, "mk-text", labels.description)));
      break;
    case "post_comments":
      m.append(who(acc));
      pp.texts.forEach((t, i) => {
        if (i === 0) {
          m.append(textBlock(t, "mk-text", ""));
          if (pp.media.length) m.append(carousel(pp.media));
        } else {
          const c = el("div", "mk-comment");
          c.append(textBlock(t, "mk-text", `${labels.comment} ${i}`));
          m.append(c);
        }
      });
      break;
    case "article":
      if (pp.media.length) m.append(carousel(pp.media));
      if (pp.title) m.append(el("h2", "mk-h", pp.title.text));
      pp.texts.forEach((t) => m.append(textBlock(t, "mk-text md", "")));
      break;
    default: // caption: media first, then the caption under the handle
      m.append(who(acc));
      if (pp.media.length) m.append(carousel(pp.media));
      if (pp.title) m.append(textBlock(pp.title, "mk-title", labels.title));
      pp.texts.forEach((t) => m.append(textBlock(t, "mk-text", labels[t.label] || "")));
  }
  return m;
}

function fmtValue(k, v) {
  if (typeof v === "boolean") return v ? T.yes : T.no;
  if (Array.isArray(v)) return v.map((x) => (typeof x === "object" ? JSON.stringify(x) : String(x))).join(", ");
  if (v && typeof v === "object") return JSON.stringify(v);
  return String(v);
}

function optionsTable(opts) {
  const keys = Object.keys(opts || {}).sort();
  if (!keys.length) return null;
  const dl = el("dl", "kv");
  for (const k of keys) {
    dl.append(el("dt", "", k));
    const dd = el("dd");
    const v = opts[k];
    const url = typeof v === "string" && /^https?:\/\//.test(v) ? safeURL(v) : null;
    if (url) { const a = el("a", "", v); a.href = url; a.target = "_blank"; a.rel = "noopener noreferrer"; dd.append(a); }
    else dd.textContent = fmtValue(k, v);
    dl.append(dd);
  }
  return dl;
}

function fmtSize(n) {
  if (!n) return "";
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(0)} KB`;
  return `${(n / (1 << 20)).toFixed(1)} MB`;
}

function section(title, ...children) {
  const s = el("section", "pv-sec");
  s.append(el("h4", "", title), ...children.filter(Boolean));
  return s;
}

function renderAccount(pp, tz) {
  const p = pp.post;
  const box = el("div", "pv-acc");
  box.style.setProperty("--net", netColor({ provider: pp.account && pp.account.provider }));
  const head = el("div", "pv-head");
  head.append(netIcon({ provider: pp.account && pp.account.provider }));
  const name = el("div", "pv-name");
  name.append(el("b", "", NET_NAMES[pp.account && pp.account.provider] || (pp.account && pp.account.provider) || "?"));
  if (pp.account) name.append(el("span", "", `${pp.account.name}${pp.account.handle ? " · @" + pp.account.handle : ""}`));
  head.append(name, el("span", "spacer"), statusPill(p.status));
  box.append(head, whenLine(p, tz));

  if (pp.problems.length) {
    const ul = el("ul", "pv-problems");
    pp.problems.forEach((x) => ul.append(el("li", "", x)));
    const h = el("div", "pv-ph");
    h.append(svg(WARN_ICON), el("span", "", T.problems));
    const b = el("div", "pv-warnbox");
    b.append(h, ul);
    box.append(b);
  }
  if (p.error) {
    const e = el("div", "pv-err");
    e.append(el("b", "", T.error + " "), document.createTextNode(p.error));
    if (p.attempts) e.append(el("div", "small", `${p.attempts} ${T.attempts}` + (p.next_attempt_at ? ` · ${T.nextTry} ${fmtIn(tz, new Date(p.next_attempt_at), LONG_OPTS)}` : "")));
    box.append(e);
  }

  box.append(mock(pp));

  if (pp.media.length) {
    const list = el("ol", "pv-mlist");
    pp.media.forEach((m) => {
      const li = el("li");
      li.append(el("span", "fn", m.missing ? m.id : m.filename || m.id), el("span", "mt", m.missing ? T.missing : `${m.mime} · ${fmtSize(m.size)}`));
      list.append(li);
    });
    box.append(section(T.media, list));
  }
  box.append(section(T.options, optionsTable(p.options)));

  const meta = el("dl", "kv small");
  const add = (k, v) => { if (v) { meta.append(el("dt", "", k), el("dd", "", v)); } };
  add(T.post, p.id);
  add(T.group, p.group_id);
  add(T.created, fmtIn(tz, new Date(p.created_at), LONG_OPTS));
  add(T.updated, fmtIn(tz, new Date(p.updated_at), LONG_OPTS));
  box.append(section(T.details, meta));
  return box;
}

async function openPreview(p) {
  const body = $("preview-body");
  const shell = el("div", "pv");
  const top = el("div", "pv-top");
  const close = el("button", "icon"); close.type = "button"; close.setAttribute("aria-label", T.close); close.append(svg(CLOSE_ICON));
  close.addEventListener("click", () => $("preview").close());
  top.append(el("span", "pv-ro", T.readOnly), el("span", "spacer"));
  const link = p.remote_url && safeURL(p.remote_url);
  if (link) {
    const a = el("a", "icon"); a.href = link; a.target = "_blank"; a.rel = "noopener noreferrer";
    a.setAttribute("aria-label", T.open); a.append(svg(EXT_ICON));
    top.append(a);
  }
  top.append(close);
  shell.append(top, el("div", "pv-loading"));
  body.replaceChildren(shell);
  if (!$("preview").open) $("preview").showModal();

  let data;
  try { data = await api("posts/" + encodeURIComponent(p.id)); }
  catch (e) {
    if (e.status === 401) { $("preview").close(); return showLogin(); }
    shell.lastChild.replaceWith(el("p", "pv-err", `${T.load}: ${e.message}`));
    return;
  }
  const tz = data.timezone || "UTC";
  const posts = data.posts || [];
  const holder = el("div");
  const show = (i) => {
    holder.replaceChildren(renderAccount(posts[i], tz));
    tabs.querySelectorAll("button").forEach((b, j) => b.setAttribute("aria-selected", i === j));
  };
  const tabs = el("div", "pv-tabs");
  tabs.setAttribute("role", "tablist");
  if (posts.length > 1) {
    posts.forEach((pp, i) => {
      const b = el("button", "chip" + (pp.problems.length ? " bad" : ""));
      b.type = "button"; b.setAttribute("role", "tab");
      const prov = pp.account && pp.account.provider;
      b.style.setProperty("--net", netColor({ provider: prov }));
      b.append(netIcon({ provider: prov }), el("span", "", (pp.account && pp.account.handle) ? "@" + pp.account.handle : NET_NAMES[prov] || prov || "?"));
      if (pp.problems.length) b.append(svg(WARN_ICON));
      b.addEventListener("click", () => show(i));
      tabs.append(b);
    });
  }
  shell.lastChild.replaceWith(...(posts.length > 1 ? [tabs, holder] : [holder]));
  if (posts.length) show(0);
}

// ---------- navigation ----------

function step(n) {
  state.day = addDays(state.day, showDay() && !mobile.matches ? n : 7 * n);
  load();
}
function goToday() { state.day = startOfDay(new Date()); load(); }

function showLogin() { $("app").hidden = true; $("login").hidden = false; }
function showApp() { $("login").hidden = true; $("app").hidden = false; load(); loadDrafts(); }

document.addEventListener("DOMContentLoaded", async () => {
  const signIn = $("login-btn");
  signIn.append(svg(KEY_ICON), document.createTextNode(T.signIn));
  $("reg-toggle").textContent = T.addKey;
  $("reg-token").placeholder = T.token;
  $("reg-btn").textContent = T.create;
  $("v-week").textContent = T.week;
  $("v-day").textContent = T.day;
  $("drafts-label").textContent = T.drafts;

  signIn.addEventListener("click", login);
  $("reg-toggle").addEventListener("click", () => { $("register").hidden = !$("register").hidden; $("reg-token").focus(); });
  $("register").addEventListener("submit", register);
  $("logout").addEventListener("click", logout);
  $("prev").addEventListener("click", () => step(-1));
  $("next").addEventListener("click", () => step(1));
  $("range").addEventListener("click", () => { if (state.view !== "drafts") goToday(); });
  $("v-week").addEventListener("click", () => { state.view = "week"; load(); });
  $("v-day").addEventListener("click", () => { state.view = "day"; load(); });
  $("v-drafts").addEventListener("click", () => {
    state.view = state.view === "drafts" ? "week" : "drafts";
    if (state.view === "drafts") loadDrafts();
    load();
  });
  mobile.addEventListener("change", render);

  // Close the sheet by tapping the backdrop.
  $("preview").addEventListener("click", (e) => { if (e.target === e.currentTarget) e.currentTarget.close(); });

  // Swipe between days on touch screens.
  let sx = null, sy = null;
  $("cal").addEventListener("touchstart", (e) => { sx = e.touches[0].clientX; sy = e.touches[0].clientY; }, { passive: true });
  $("cal").addEventListener("touchend", (e) => {
    if (sx === null || !showDay() || state.view === "drafts") return;
    const dx = e.changedTouches[0].clientX - sx, dy = e.changedTouches[0].clientY - sy;
    sx = null;
    if (Math.abs(dx) > 60 && Math.abs(dx) > 2 * Math.abs(dy)) { state.day = addDays(state.day, dx < 0 ? 1 : -1); load(); }
  });

  document.addEventListener("keydown", (e) => {
    if ($("app").hidden || e.target.tagName === "INPUT" || $("preview").open) return;
    if (e.key === "ArrowLeft") step(-1);
    if (e.key === "ArrowRight") step(1);
    if (e.key === "t") goToday();
    if (e.key === "w") { state.view = "week"; load(); }
    if (e.key === "d") { state.view = "day"; load(); }
  });

  try {
    const me = await api("me");
    if (me.logged_in) return showApp();
    if (me.registration_open !== false) {
      $("reg-toggle").hidden = false;
      if (!me.has_passkeys) $("register").hidden = false;
    }
  } catch { /* fall through to login */ }
  showLogin();
});
