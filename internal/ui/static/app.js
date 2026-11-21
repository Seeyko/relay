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
  "linkedin-page": "M4.5 3a2 2 0 1 1 0 4 2 2 0 0 1 0-4zM3 9h3v12H3zM9 9h3v1.7c.5-.9 1.7-1.9 3.6-1.9 3.4 0 4.4 2.1 4.4 5.3V21h-3v-6.2c0-1.6-.3-3-2.1-3-1.9 0-2.4 1.3-2.4 3V21H9z",
  threads: "M12 2a10 10 0 1 0 6.3 17.8l-1.3-1.6A8 8 0 1 1 20 12v1.2c0 1.2-.8 2-1.8 2s-1.7-.8-1.7-2V8h-2v.9A4.5 4.5 0 1 0 15 15.4a3.6 3.6 0 0 0 3.2 1.8c2.2 0 3.8-1.7 3.8-4V12A10 10 0 0 0 12 2zm0 12.5a2.5 2.5 0 1 1 0-5 2.5 2.5 0 0 1 0 5z",
  website: "M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zm6.9 9h-3a15 15 0 0 0-1.3-5.6A8 8 0 0 1 18.9 11zM12 4c.9 1.2 1.8 3.6 1.9 7h-3.8c.1-3.4 1-5.8 1.9-7zM9.4 5.4A15 15 0 0 0 8.1 11h-3a8 8 0 0 1 4.3-5.6zM5.1 13h3a15 15 0 0 0 1.3 5.6A8 8 0 0 1 5.1 13zM12 20c-.9-1.2-1.8-3.6-1.9-7h3.8c-.1 3.4-1 5.8-1.9 7zm2.6-1.4a15 15 0 0 0 1.3-5.6h3a8 8 0 0 1-4.3 5.6z",
};
const KEY_ICON = "M15 7a3 3 0 1 1-6 0 3 3 0 0 1 6 0zM12 10v11m0-4h3m-3-3h2";
const EXT_ICON = "M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5";
const CLOSE_ICON = "M6 6l12 12M18 6L6 18";
const EYE_ICON = "M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12zm10-3a3 3 0 1 0 0 6 3 3 0 0 0 0-6z";
const HEART_ICON = "M12 20s-7-4.4-9.2-8.6C1.4 8.6 3.2 5 6.6 5c2 0 3.4 1.1 4.2 2.4h2.4C14 6.1 15.4 5 17.4 5c3.4 0 5.2 3.6 3.8 6.4C19 15.6 12 20 12 20z";
const COMMENT_ICON = "M21 12a8 8 0 0 1-11.6 7.1L4 20l1.1-4.6A8 8 0 1 1 21 12z";
const SHARE_ICON = "M17 2l4 4-4 4M3 11V9a3 3 0 0 1 3-3h15M7 22l-4-4 4-4M21 13v2a3 3 0 0 1-3 3H3";
const SAVE_ICON = "M6 3h12v18l-6-4-6 4z";
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

const fCompact = new Intl.NumberFormat(L, { notation: "compact", maximumFractionDigits: 1 });
const fShort = new Intl.DateTimeFormat(L, { weekday: "short", day: "numeric", month: "short", hour: "numeric", minute: "2-digit" });

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
  const st = p.stats || {};
  const top = st.views > 0 ? ["views", EYE_ICON] : st.likes > 0 ? ["likes", HEART_ICON] : null;
  if (top) { const k = el("span", "kpi"); k.append(svg(top[1]), document.createTextNode(fCompact.format(st[top[0]]))); meta.append(k); }
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
    head.append(el("span", g.planned_at ? "dc-when" : "dc-when undated", g.planned_at ? fShort.format(new Date(g.planned_at)) : T.noDate));
    const probs = g.posts.reduce((n, p) => n + p.problems, 0);
    if (probs) { const w = el("span", "warn"); w.append(svg(WARN_ICON), document.createTextNode(String(probs))); head.append(w); }
    const nets = el("div", "dc-nets");
    for (const p of g.posts) {
      const chip = el("span", "chip" + (p.problems ? " bad" : ""));
      chip.style.setProperty("--net", netColor(p));
      chip.setAttribute("aria-label", NET_NAMES[p.provider] || p.provider);
      chip.append(netIcon(p));
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
// One post group: a tab per account, the post laid out roughly like the network, its stats once published.

function fmtIn(tz, d, opts) {
  try { return new Intl.DateTimeFormat(L, { ...opts, timeZone: tz }).format(d); } catch { return new Intl.DateTimeFormat(L, opts).format(d); }
}
const SHORT_OPTS = { weekday: "short", day: "numeric", month: "short", hour: "numeric", minute: "2-digit" };

function whenText(post, tz) {
  const at = post.status === "draft" ? post.planned_at : post.status === "published" ? post.published_at : post.scheduled_at;
  if (!at) return post.status === "draft" ? T.noDate : "";
  return fmtIn(tz, new Date(at), SHORT_OPTS);
}

function statusPill(status) {
  const label = { draft: T.draft, scheduled: T.scheduled, publishing: T.publishing, failed: T.failedS }[status];
  return label ? el("span", "pill " + status, label) : null;
}

// A character counter only matters before publishing, and only near the limit.
function counter(t, live) {
  if (live || !t.limit || t.count < t.limit * 0.8) return null;
  return el("span", "count" + (t.count > t.limit ? " over" : ""), `${t.count}/${t.limit}`);
}

function foldedText(t, cls) {
  const box = el("div", cls);
  const chars = Array.from(t.text);
  let cut = t.fold || 0;
  const nl = chars.indexOf("\n");
  if (cut && nl >= 0 && nl < cut) cut = nl;
  if (cut >= chars.length) cut = 0;
  if (cut && cut === t.fold) {
    const sp = chars.slice(0, cut).lastIndexOf(" ");
    if (sp > cut - 25) cut = sp;
  }
  if (!cut) { box.textContent = t.text; return box; }
  const more = el("button", "more", `… ${T.more}`);
  more.type = "button";
  more.addEventListener("click", () => { box.replaceChildren(document.createTextNode(t.text)); });
  box.append(el("span", "", chars.slice(0, cut).join("").trimEnd()), more);
  return box;
}

function mediaEl(m) {
  if (m.missing) return el("div", "m-missing", T.missing);
  if ((m.mime || "").startsWith("video/")) {
    const v = el("video"); v.src = m.path; v.controls = true; v.preload = "metadata"; v.playsInline = true;
    return v;
  }
  const i = el("img"); i.src = m.path; i.alt = ""; i.decoding = "async";
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

function textEl(t, cls, live) {
  const box = el("div", "mk-block");
  box.append(t.fold ? foldedText(t, cls) : el("div", cls, t.text));
  const c = counter(t, live);
  if (c) box.append(c);
  return box;
}

function mock(pp, live) {
  const m = el("div", "mock " + pp.layout);
  const title = () => pp.title && m.append(textEl(pp.title, "mk-title", live));
  switch (pp.layout) {
    case "thread":
      pp.texts.forEach((t, i) => {
        const item = el("div", "mk-item");
        item.append(textEl(t, "mk-text", live));
        if (i === 0 && pp.media.length) item.append(carousel(pp.media));
        m.append(item);
      });
      break;
    case "post_comments":
      pp.texts.forEach((t, i) => {
        if (i === 0) { m.append(textEl(t, "mk-text", live)); if (pp.media.length) m.append(carousel(pp.media)); return; }
        const c = el("div", "mk-comment");
        c.append(textEl(t, "mk-text", live));
        m.append(c);
      });
      break;
    case "article":
      if (pp.media.length) m.append(carousel(pp.media));
      if (pp.title) m.append(el("h2", "mk-h", pp.title.text));
      pp.texts.forEach((t) => m.append(textEl(t, "mk-text md", live)));
      break;
    default: // caption and video: visual first, then title and text
      if (pp.media.length) m.append(carousel(pp.media));
      title();
      pp.texts.forEach((t) => m.append(textEl(t, "mk-text", live)));
  }
  return m;
}

function statsRow(st) {
  if (!st) return null;
  const row = el("div", "stats");
  const add = (k, icon) => {
    if (st[k] === undefined) return;
    const s = el("span", "stat");
    s.append(svg(icon), el("b", "", fCompact.format(st[k])));
    row.append(s);
  };
  add("views", EYE_ICON); add("likes", HEART_ICON); add("comments", COMMENT_ICON); add("shares", SHARE_ICON); add("saves", SAVE_ICON);
  if (st.engagement_rate) row.append(el("span", "stat rate", `${(st.engagement_rate * 100).toFixed(1)} %`));
  return row.childElementCount ? row : null;
}

function renderAccount(pp, tz) {
  const p = pp.post;
  const live = p.status === "published";
  const prov = pp.account && pp.account.provider;
  const box = el("div", "pv-acc");
  box.style.setProperty("--net", netColor({ provider: prov }));
  const drawn = netMock(pp, tz, live);

  // The network mockup carries the name, the time and the counters; around it only what it cannot show.
  const head = el("div", "pv-head" + (drawn ? " slim" : ""));
  if (!drawn) head.append(netIcon({ provider: prov }));
  const name = el("div", "pv-name");
  if (!drawn) name.append(el("b", "", pp.account && pp.account.handle ? "@" + pp.account.handle : NET_NAMES[prov] || prov || "?"));
  const when = whenText(p, tz);
  if (when) name.append(el("span", "", when));
  head.append(name);
  const st = pp.stats || {};
  if (drawn && st.views > 0) {
    const v = el("span", "stat");
    v.append(svg(EYE_ICON), el("b", "", fCompact.format(st.views)));
    head.append(v);
  }
  if (drawn && st.engagement_rate) head.append(el("span", "stat rate", `${(st.engagement_rate * 100).toFixed(1)} %`));
  const pill = statusPill(p.status);
  if (pill) head.append(pill);
  box.append(head);

  if (pp.problems.length) {
    const ul = el("ul", "pv-problems");
    pp.problems.forEach((x) => ul.append(el("li", "", x)));
    box.append(ul);
  }
  if (p.error) box.append(el("p", "pv-err", p.error));
  if (drawn) { box.append(drawn); return box; }
  const sr = statsRow(pp.stats);
  if (sr) box.append(sr);
  box.append(mock(pp, live));
  return box;
}

// The page behind the sheet must not scroll (iOS scrolls the body through a modal dialog).
let lockedY = 0;
function lockPage() {
  lockedY = window.scrollY;
  document.body.style.position = "fixed";
  document.body.style.top = `-${lockedY}px`;
  document.body.style.left = document.body.style.right = "0";
}
function unlockPage() {
  document.body.style.position = document.body.style.top = document.body.style.left = document.body.style.right = "";
  window.scrollTo(0, lockedY);
}

async function openPreview(p) {
  const dlg = $("preview");
  const body = $("preview-body");
  const top = el("div", "pv-top");
  const tabs = el("div", "pv-tabs");
  tabs.setAttribute("role", "tablist");
  const actions = el("div", "pv-actions");
  const link = p.remote_url && safeURL(p.remote_url);
  if (link) {
    const a = el("a", "icon"); a.href = link; a.target = "_blank"; a.rel = "noopener noreferrer";
    a.setAttribute("aria-label", T.open); a.append(svg(EXT_ICON));
    actions.append(a);
  }
  const close = el("button", "icon"); close.type = "button"; close.setAttribute("aria-label", T.close); close.append(svg(CLOSE_ICON));
  close.addEventListener("click", () => dlg.close());
  actions.append(close);
  top.append(tabs, actions);
  const holder = el("div", "pv-body");
  holder.append(el("div", "pv-loading"));
  body.replaceChildren(top, holder);
  if (!dlg.open) { lockPage(); dlg.showModal(); }

  let data;
  try { data = await api("posts/" + encodeURIComponent(p.id)); }
  catch (e) {
    if (e.status === 401) { dlg.close(); return showLogin(); }
    holder.replaceChildren(el("p", "pv-err", `${T.load}: ${e.message}`));
    return;
  }
  const tz = data.timezone || "UTC";
  const posts = data.posts || [];
  const show = (i) => {
    holder.replaceChildren(renderAccount(posts[i], tz));
    tabs.querySelectorAll("button").forEach((b, j) => b.setAttribute("aria-selected", i === j));
    body.scrollTop = 0;
  };
  if (posts.length > 1) {
    posts.forEach((pp, i) => {
      const prov = pp.account && pp.account.provider;
      const b = el("button", "tab" + (pp.problems.length ? " bad" : ""));
      b.type = "button"; b.setAttribute("role", "tab");
      b.setAttribute("aria-label", NET_NAMES[prov] || prov || "?");
      b.style.setProperty("--net", netColor({ provider: prov }));
      b.append(netIcon({ provider: prov }));
      b.addEventListener("click", () => show(i));
      tabs.append(b);
    });
  }
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
  $("preview").addEventListener("close", unlockPage);

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
