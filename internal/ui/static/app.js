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
  if (!window.PublicKeyCredential) return msg(out, "This browser does not support passkeys.", "err");
  msg(out, "Waiting for your passkey...");
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
    msg(out, e.name === "NotAllowedError" ? "Cancelled or timed out." : "Sign-in failed: " + e.message, "err");
  }
}

async function register() {
  const out = $("reg-msg");
  const tokenEl = $("reg-token");
  const token = tokenEl.value.trim();
  if (!token) return msg(out, "Enter the API token.", "err");
  msg(out, "Follow your browser's prompt...");
  try {
    const opts = await api("register/begin", { method: "POST", body: { name: $("reg-name").value }, token });
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
    msg(out, "Passkey added. You can sign in now.", "ok");
  } catch (e) {
    msg(out, e.name === "InvalidStateError" ? "This authenticator is already registered." :
      e.name === "NotAllowedError" ? "Cancelled or timed out." : "Failed: " + e.message, "err");
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
const fmtTime = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" });
const fmtDay = new Intl.DateTimeFormat(undefined, { weekday: "short" });
const fmtLong = new Intl.DateTimeFormat(undefined, { weekday: "long", day: "numeric", month: "long", year: "numeric" });
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

function netVar(p) { return NETS.includes(p.provider) ? `var(--${p.provider})` : "var(--muted)"; }

function statusLabel(p) { return p.source === "imported" ? "imported" : p.status; }

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

function card(p, large) {
  const c = el("button", "card");
  c.type = "button";
  c.style.setProperty("--net", netVar(p));
  const top = el("div", "top");
  top.append(el("span", "time", fmtTime.format(p.date)), el("span", "dot"));
  top.lastChild.style.setProperty("--net", netVar(p));
  top.append(el("span", "", NET_LABEL[p.provider] || p.provider || "?"));
  const st = statusLabel(p);
  top.append(el("span", "status s-" + st, st));
  const text = el("div", "text", p.title ? p.title + " · " + (p.parts[0] || "") : (p.parts[0] || ""));
  const t = thumb(p);
  if (large) {
    const body = el("div", "body");
    body.append(top, el("div", "muted small", p.handle ? "@" + p.handle : p.account), text);
    if (p.parts.length > 1) body.append(el("div", "muted small", `Thread · ${p.parts.length} posts`));
    if (t) c.append(t);
    c.append(body);
  } else {
    c.append(top);
    if (t) c.append(t);
    c.append(text);
  }
  c.addEventListener("click", () => openPreview(p));
  return c;
}

function visible() { return state.posts.filter((p) => !state.hidden.has(p.provider)); }

function renderFilters() {
  const box = $("filters");
  box.replaceChildren();
  const present = [...new Set(state.posts.map((p) => p.provider))].filter(Boolean).sort();
  for (const n of present) {
    const b = el("button", "chip" + (state.hidden.has(n) ? " off" : ""));
    b.type = "button";
    const d = el("span", "dot"); d.style.setProperty("--net", NETS.includes(n) ? `var(--${n})` : "var(--muted)");
    const count = state.posts.filter((p) => p.provider === n).length;
    b.append(d, document.createTextNode(`${NET_LABEL[n] || n} · ${count}`));
    b.addEventListener("click", () => { state.hidden.has(n) ? state.hidden.delete(n) : state.hidden.add(n); render(); });
    box.append(b);
  }
}

function render() {
  $("v-week").setAttribute("aria-selected", state.view === "week");
  $("v-day").setAttribute("aria-selected", state.view === "day");
  const [from, to] = range();
  $("range").textContent = state.view === "day" ? fmtLong.format(from)
    : `${fmtShort.format(from)} – ${fmtShort.format(addDays(to, -1))} ${addDays(to, -1).getFullYear()}`;
  renderFilters();
  const cal = $("cal");
  cal.replaceChildren(state.view === "week" ? renderWeek(from) : renderDay());
}

function renderWeek(from) {
  const grid = el("div", "week");
  const today = new Date();
  const posts = visible();
  for (let i = 0; i < 7; i++) {
    const d = addDays(from, i);
    const col = el("div", "day-col" + (sameDay(d, today) ? " today" : ""));
    const head = el("div", "day-head");
    head.append(el("span", "", fmtDay.format(d)), el("span", "n", String(d.getDate())));
    head.title = "Open day";
    head.addEventListener("click", () => { state.day = d; state.view = "day"; load(); });
    const list = el("div", "day-list");
    const ps = posts.filter((p) => sameDay(p.date, d));
    if (!ps.length) list.append(el("div", "empty", "—"));
    ps.forEach((p) => list.append(card(p, false)));
    col.append(head, list);
    grid.append(col);
  }
  return grid;
}

function renderDay() {
  const box = el("div", "dayview");
  const posts = visible();
  if (!posts.length) { box.append(el("div", "empty", "Nothing on this day.")); return box; }
  const byHour = new Map();
  posts.forEach((p) => { const h = p.date.getHours(); if (!byHour.has(h)) byHour.set(h, []); byHour.get(h).push(p); });
  for (const [h, ps] of [...byHour.entries()].sort((a, b) => a[0] - b[0])) {
    const slot = el("div", "slot");
    const list = el("div", "day-list");
    ps.forEach((p) => list.append(card(p, true)));
    slot.append(el("div", "h", String(h).padStart(2, "0") + ":00"), list);
    box.append(slot);
  }
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
  const st = statusLabel(p);
  head.append(d, el("strong", "", NET_LABEL[p.provider] || p.provider || "?"),
    el("span", "muted", p.handle ? "@" + p.handle : p.account), el("span", "status s-" + st, st));
  body.append(head, el("div", "muted small", fmtLong.format(p.date) + " · " + fmtTime.format(p.date)));
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
  p.parts.forEach((t, i) => {
    const part = el("div", "pv-part", t);
    if (p.parts.length > 1) part.prepend(el("div", "muted small", `${i + 1}/${p.parts.length}`));
    body.append(part);
  });
  if (p.error) body.append(el("p", "pv-err", p.error));
  const link = p.remote_url && safeURL(p.remote_url);
  if (link) {
    const a = el("a", "", "Open on " + (NET_LABEL[p.provider] || "the network"));
    a.href = link; a.target = "_blank"; a.rel = "noopener noreferrer";
    body.append(el("p")); body.lastChild.append(a);
  }
  $("preview").showModal();
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
  $("today").addEventListener("click", () => { state.day = startOfDay(new Date()); load(); });
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
    if (!me.has_passkeys) $("register").open = true;
  } catch { /* fall through to login */ }
  showLogin();
});
