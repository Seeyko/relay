"use strict";
// Relay dashboard: passkey login and a read-only week/day calendar.
// Every value from the API is rendered with textContent (never innerHTML).

const $ = (id) => document.getElementById(id);
const NETS = ["x", "facebook", "instagram", "youtube", "tiktok", "website"];
const NET_LABEL = { x: "X", facebook: "Facebook", instagram: "Instagram", youtube: "YouTube", tiktok: "TikTok", website: "Website" };

const state = {
  view: "week",
  day: startOfDay(new Date()),
  posts: [],
  hidden: new Set(),
};

// ---------- base64url <-> ArrayBuffer for WebAuthn ----------

function b64uToBuf(s) {
  s = s.replace(/-/g, "+").replace(/_/g, "/");
  while (s.length % 4) s += "=";
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}
function bufToB64u(buf) {
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
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
  if (!res.ok) {
    const err = new Error(data.error || res.statusText);
    err.status = res.status;
    throw err;
  }
  return data;
}

function msg(el, text, kind) {
  el.textContent = text;
  el.className = "msg" + (kind ? " " + kind : "");
}

// ---------- login / registration ----------

async function login() {
  const out = $("login-msg");
  if (!window.PublicKeyCredential) return msg(out, "No passkey support.", "err");
  msg(out, "");
  try {
    const opts = await api("login/begin", { method: "POST", body: {} });
    const pk = opts.publicKey;
    pk.challenge = b64uToBuf(pk.challenge);
    (pk.allowCredentials || []).forEach((c) => (c.id = b64uToBuf(c.id)));
    const cred = await navigator.credentials.get({ publicKey: pk });
    const r = cred.response;
    await api("login/finish", {
      method: "POST",
      body: {
        id: cred.id, rawId: bufToB64u(cred.rawId), type: cred.type,
        response: {
          authenticatorData: bufToB64u(r.authenticatorData),
          clientDataJSON: bufToB64u(r.clientDataJSON),
          signature: bufToB64u(r.signature),
          userHandle: r.userHandle ? bufToB64u(r.userHandle) : "",
        },
      },
    });
    msg(out, "");
    showApp();
  } catch (e) {
    msg(out, e.name === "NotAllowedError" ? "" : e.message, "err");
  }
}

async function register() {
  const out = $("reg-msg");
  const tokenEl = $("reg-token");
  const token = tokenEl.value.trim();
  if (!token) return;
  msg(out, "");
  try {
    const opts = await api("register/begin", { method: "POST", body: {}, token });
    const pk = opts.publicKey;
    pk.challenge = b64uToBuf(pk.challenge);
    pk.user.id = b64uToBuf(pk.user.id);
    (pk.excludeCredentials || []).forEach((c) => (c.id = b64uToBuf(c.id)));
    const cred = await navigator.credentials.create({ publicKey: pk });
    const r = cred.response;
    await api("register/finish", {
      method: "POST", token,
      body: {
        id: cred.id, rawId: bufToB64u(cred.rawId), type: cred.type,
        response: {
          attestationObject: bufToB64u(r.attestationObject),
          clientDataJSON: bufToB64u(r.clientDataJSON),
          transports: r.getTransports ? r.getTransports() : [],
        },
      },
    });
    msg(out, "Added.");
  } catch (e) {
    msg(out, e.name === "InvalidStateError" ? "Already added." : e.name === "NotAllowedError" ? "" : e.message, "err");
  } finally {
    tokenEl.value = ""; // never keep the token around
  }
}

async function logout() {
  await api("logout", { method: "POST", body: {} }).catch(() => {});
  location.reload();
}

// ---------- dates ----------

function startOfDay(d) { const x = new Date(d); x.setHours(0, 0, 0, 0); return x; }
function addDays(d, n) { const x = new Date(d); x.setDate(x.getDate() + n); return x; }
function startOfWeek(d) { const x = startOfDay(d); const wd = (x.getDay() + 6) % 7; return addDays(x, -wd); }
function sameDay(a, b) { return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate(); }
const fmtTime = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
const fmtMonth = new Intl.DateTimeFormat(undefined, { month: "short" });
const fmtDay = new Intl.DateTimeFormat(undefined, { weekday: "short" });
const fmtDayMonth = new Intl.DateTimeFormat(undefined, { weekday: "short", day: "numeric", month: "short" });
const fmtShort = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short" });

function range() {
  if (state.view === "day") return [state.day, addDays(state.day, 1)];
  const s = startOfWeek(state.day);
  return [s, addDays(s, 7)];
}

// ---------- data ----------

async function load() {
  const [from, to] = range();
  try {
    const data = await api(`calendar?from=${encodeURIComponent(from.toISOString())}&to=${encodeURIComponent(to.toISOString())}`);
    state.posts = (data.posts || []).map((p) => ({ ...p, date: new Date(p.at) }))
      .sort((a, b) => a.date - b.date);
  } catch (e) {
    if (e.status === 401) return showLogin();
    state.posts = [];
  }
  render();
}

// ---------- rendering ----------

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function netVar(p) { return NETS.includes(p.provider) ? `var(--${p.provider})` : "var(--soft)"; }

function thumb(p) {
  const m = p.media.find((m) => m.mime.startsWith("image/")) || p.media[0];
  if (!m) return null;
  if (m.mime.startsWith("video/")) {
    const v = el("video", "thumb");
    v.src = m.url + "#t=0.1"; v.muted = true; v.preload = "metadata"; v.playsInline = true;
    return v;
  }
  const i = el("img", "thumb");
  i.src = m.url; i.alt = ""; i.loading = "lazy"; i.decoding = "async";
  return i;
}

function row(p, i) {
  const r = el("button", "row");
  r.type = "button";
  r.style.setProperty("--net", netVar(p));
  r.style.setProperty("--i", String(i));
  if (p.status === "published" || p.source === "imported") r.classList.add("done");
  if (p.status === "failed" || p.status === "draft") r.classList.add(p.status);
  r.setAttribute("aria-label", `${NET_LABEL[p.provider] || p.provider}, ${fmtTime.format(p.date)}, ${p.status}`);
  const text = el("span", "text", p.title || p.parts[0] || "");
  if (p.status === "failed") text.prepend(el("span", "flag", "Failed"));
  r.append(el("span", "time", fmtTime.format(p.date)), text);
  const t = thumb(p);
  r.append(t || el("span"));
  r.addEventListener("click", () => openPreview(p));
  return r;
}

function visible() { return state.posts.filter((p) => !state.hidden.has(p.provider)); }

function renderFilters() {
  const box = $("filters");
  box.replaceChildren();
  const present = [...new Set(state.posts.map((p) => p.provider))].filter(Boolean).sort();
  if (present.length < 2 && !state.hidden.size) return;
  for (const n of present) {
    const b = el("button", state.hidden.has(n) ? "off" : "");
    b.type = "button";
    const d = el("span", "dot"); d.style.setProperty("--net", NETS.includes(n) ? `var(--${n})` : "var(--soft)");
    b.append(d, document.createTextNode(NET_LABEL[n] || n));
    b.addEventListener("click", () => { state.hidden.has(n) ? state.hidden.delete(n) : state.hidden.add(n); render(); });
    box.append(b);
  }
}

function render() {
  $("v-week").setAttribute("aria-selected", state.view === "week");
  $("v-day").setAttribute("aria-selected", state.view === "day");
  const [from, to] = range();
  const last = addDays(to, -1);
  $("range").textContent = state.view === "day" ? fmtDayMonth.format(from)
    : from.getMonth() === last.getMonth() ? `${from.getDate()}–${last.getDate()} ${fmtMonth.format(last)}` : `${fmtShort.format(from)} – ${fmtShort.format(last)}`;
  renderFilters();
  $("cal").replaceChildren(state.view === "week" ? renderWeek(from) : renderDay());
  const today = $("cal").querySelector(".day.today");
  if (today && !matchMedia("(min-width: 900px)").matches) today.scrollIntoView({ block: "start" });
}

function dayBlock(d, ps) {
  const today = startOfDay(new Date());
  const box = el("div", "day" + (sameDay(d, today) ? " today" : d < today ? " past" : ""));
  const head = el("button", "day-head");
  head.type = "button";
  head.append(el("span", "n", String(d.getDate())), el("span", "w", fmtDay.format(d)));
  head.addEventListener("click", () => { state.day = d; state.view = "day"; load(); });
  box.append(head);
  ps.forEach((p, i) => box.append(row(p, i)));
  return box;
}

function renderWeek(from) {
  const grid = el("div", "week");
  const posts = visible();
  const wide = matchMedia("(min-width: 900px)").matches;
  for (let i = 0; i < 7; i++) {
    const d = addDays(from, i);
    const ps = posts.filter((p) => sameDay(p.date, d));
    if (ps.length || wide) grid.append(dayBlock(d, ps));
  }
  if (!grid.children.length) grid.append(el("div", "empty", "Nothing planned."));
  return grid;
}

function renderDay() {
  const box = el("div", "dayview");
  const posts = visible();
  if (!posts.length) { box.append(el("div", "empty", "Nothing planned.")); return box; }
  posts.forEach((p, i) => box.append(row(p, i)));
  return box;
}

function safeURL(u) {
  try { const x = new URL(u); return x.protocol === "https:" || x.protocol === "http:" ? x.href : null; } catch { return null; }
}

function openPreview(p) {
  const body = $("preview-body");
  body.replaceChildren();
  const head = el("div", "pv-head");
  const d = el("span", "dot"); d.style.setProperty("--net", netVar(p));
  head.append(d, document.createTextNode(`${NET_LABEL[p.provider] || p.provider || "?"} · ${fmtDayMonth.format(p.date)} · ${fmtTime.format(p.date)}`));
  const link = p.remote_url && safeURL(p.remote_url);
  if (link) {
    const a = el("a", "", "Open \u2197");
    a.href = link; a.target = "_blank"; a.rel = "noopener noreferrer";
    head.append(a);
  }
  body.append(head);
  if (p.error) body.append(el("p", "pv-err", p.error));
  if (p.title) body.append(el("h3", "", p.title));
  if (p.media.length) {
    const g = el("div", "pv-media");
    for (const m of p.media) {
      if (m.mime.startsWith("video/")) {
        const v = el("video"); v.src = m.url; v.controls = true; v.preload = "metadata"; v.playsInline = true; g.append(v);
      } else {
        const i = el("img"); i.src = m.url; i.alt = ""; g.append(i);
      }
    }
    body.append(g);
  }
  p.parts.forEach((t) => body.append(el("p", "pv-part", t)));
  $("preview").showModal();
  $("preview").focus();
}

// ---------- boot ----------

function showLogin() { $("app").hidden = true; $("login").hidden = false; }
function showApp() { $("login").hidden = true; $("app").hidden = false; load(); }

function step(n) {
  state.day = addDays(state.day, state.view === "day" ? n : 7 * n);
  load();
}

document.addEventListener("DOMContentLoaded", async () => {
  $("login-btn").addEventListener("click", login);
  $("reg-btn").addEventListener("click", register);
  $("logout").addEventListener("click", logout);
  $("prev").addEventListener("click", () => step(-1));
  $("next").addEventListener("click", () => step(1));
  $("range").addEventListener("click", () => { state.day = startOfDay(new Date()); load(); });
  $("v-week").addEventListener("click", () => { state.view = "week"; load(); });
  $("v-day").addEventListener("click", () => { state.view = "day"; load(); });
  document.addEventListener("keydown", (e) => {
    if ($("app").hidden || e.target.tagName === "INPUT" || $("preview").open) return;
    if (e.key === "ArrowLeft") step(-1);
    if (e.key === "ArrowRight") step(1);
    if (e.key === "t") { state.day = startOfDay(new Date()); load(); }
  });
  try {
    const me = await api("me");
    if (me.logged_in) return showApp();
    if (me.registration_open === false) $("register").hidden = true;
    else if (!me.has_passkeys) $("register").open = true;
  } catch { /* fall through to login */ }
  showLogin();
});
