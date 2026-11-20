"use strict";
// Relay dashboard: passkey login and a read-only calendar.
// Every value from the API is rendered with textContent (never innerHTML).

const $ = (id) => document.getElementById(id);
const FR = (navigator.language || "").toLowerCase().startsWith("fr");
const T = FR
  ? { signIn: "Se connecter", addKey: "Ajouter une clé", token: "Jeton API", create: "Créer", week: "Semaine", day: "Jour",
      empty: "Rien de prévu", cancelled: "Annulé.", failed: "Échec", unsupported: "Ce navigateur ne gère pas les passkeys.",
      added: "Clé ajoutée.", dup: "Cette clé existe déjà.", open: "Ouvrir" }
  : { signIn: "Sign in", addKey: "Add a key", token: "API token", create: "Create", week: "Week", day: "Day",
      empty: "Nothing planned", cancelled: "Cancelled.", failed: "Failed", unsupported: "This browser doesn't support passkeys.",
      added: "Key added.", dup: "This key already exists.", open: "Open" };

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

const mobile = matchMedia("(max-width: 760px)");
const state = { view: "week", day: startOfDay(new Date()), posts: [], loadedFrom: null };

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

async function load() {
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
  if (status === "failed") meta.append(el("i", "flag"));
  body.append(meta, el("div", "txt", p.title || p.parts[0] || ""));
  c.append(body);
  const t = thumb(p);
  if (t) c.append(t);
  c.addEventListener("click", () => openPreview(p));
  return c;
}

function renderHeader() {
  const from = startOfWeek(state.day);
  const to = addDays(from, 6);
  const range = $("range");
  if (!mobile.matches && state.view === "day") range.textContent = fDay.format(state.day);
  else range.textContent = fRange.formatRange ? fRange.formatRange(from, to) : `${fRange.format(from)} – ${fRange.format(to)}`;
  const today = new Date();
  const away = showDay() ? !sameDay(state.day, today) : +startOfWeek(today) !== +from;
  range.classList.toggle("away", away);
  range.title = away ? "↺" : "";
  $("v-week").setAttribute("aria-selected", state.view === "week");
  $("v-day").setAttribute("aria-selected", state.view === "day");
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
  renderStrip();
  $("cal").replaceChildren(showDay() ? renderDay() : renderWeek());
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

function openPreview(p) {
  const pv = el("div", "pv");
  pv.style.setProperty("--net", netColor(p));
  const top = el("div", "pv-top");
  top.append(netIcon(p), el("span", "", fLong.format(p.date)), el("span", "spacer"));
  const link = p.remote_url && safeURL(p.remote_url);
  if (link) {
    const a = el("a", "icon"); a.href = link; a.target = "_blank"; a.rel = "noopener noreferrer";
    a.setAttribute("aria-label", T.open); a.append(svg(EXT_ICON));
    top.append(a);
  }
  pv.append(top);
  if (p.media.length) {
    const g = el("div", "pv-media");
    for (const m of p.media) {
      if (m.mime.startsWith("video/")) { const v = el("video"); v.src = m.url; v.controls = true; v.preload = "metadata"; v.playsInline = true; g.append(v); }
      else { const i = el("img"); i.src = m.url; i.alt = ""; g.append(i); }
    }
    pv.append(g);
  }
  if (p.title) pv.append(el("h3", "", p.title));
  p.parts.forEach((t) => pv.append(el("div", "pv-part", t)));
  if (p.error) pv.append(el("p", "pv-err", p.error));
  $("preview-body").replaceChildren(pv);
  $("preview").showModal();
}

// ---------- navigation ----------

function step(n) {
  state.day = addDays(state.day, showDay() && !mobile.matches ? n : 7 * n);
  load();
}
function goToday() { state.day = startOfDay(new Date()); load(); }

function showLogin() { $("app").hidden = true; $("login").hidden = false; }
function showApp() { $("login").hidden = true; $("app").hidden = false; load(); }

document.addEventListener("DOMContentLoaded", async () => {
  const signIn = $("login-btn");
  signIn.append(svg(KEY_ICON), document.createTextNode(T.signIn));
  $("reg-toggle").textContent = T.addKey;
  $("reg-token").placeholder = T.token;
  $("reg-btn").textContent = T.create;
  $("v-week").textContent = T.week;
  $("v-day").textContent = T.day;

  signIn.addEventListener("click", login);
  $("reg-toggle").addEventListener("click", () => { $("register").hidden = !$("register").hidden; $("reg-token").focus(); });
  $("register").addEventListener("submit", register);
  $("logout").addEventListener("click", logout);
  $("prev").addEventListener("click", () => step(-1));
  $("next").addEventListener("click", () => step(1));
  $("range").addEventListener("click", goToday);
  $("v-week").addEventListener("click", () => { state.view = "week"; render(); });
  $("v-day").addEventListener("click", () => { state.view = "day"; render(); });
  mobile.addEventListener("change", render);

  // Close the sheet by tapping the backdrop.
  $("preview").addEventListener("click", (e) => { if (e.target === e.currentTarget) e.currentTarget.close(); });

  // Swipe between days on touch screens.
  let sx = null, sy = null;
  $("cal").addEventListener("touchstart", (e) => { sx = e.touches[0].clientX; sy = e.touches[0].clientY; }, { passive: true });
  $("cal").addEventListener("touchend", (e) => {
    if (sx === null || !showDay()) return;
    const dx = e.changedTouches[0].clientX - sx, dy = e.changedTouches[0].clientY - sy;
    sx = null;
    if (Math.abs(dx) > 60 && Math.abs(dx) > 2 * Math.abs(dy)) { state.day = addDays(state.day, dx < 0 ? 1 : -1); load(); }
  });

  document.addEventListener("keydown", (e) => {
    if ($("app").hidden || e.target.tagName === "INPUT" || $("preview").open) return;
    if (e.key === "ArrowLeft") step(-1);
    if (e.key === "ArrowRight") step(1);
    if (e.key === "t") goToday();
    if (e.key === "w") { state.view = "week"; render(); }
    if (e.key === "d") { state.view = "day"; render(); }
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
