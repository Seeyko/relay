"use strict";
// Relay dashboard: passkey or email login, a read-only calendar, the drafts list, a full post preview, the
// connected apps, the team and the social accounts (connect / reconnect with each network).
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
      readOnly: "Lecture seule · approbation par approve_post", yes: "oui", no: "non", load: "Chargement impossible",
      apps: "Apps", noApps: "Aucune app connectée", full: "Accès complet", draftsOnly: "Brouillons", lastUsed: "utilisée",
      revoke: "Révoquer", email: "Email", sendLink: "Recevoir un lien", sent: "Lien envoyé, vérifiez vos emails.",
      join: "Rejoindre", name: "Votre nom", continue: "Continuer", badLink: "Lien invalide ou expiré.",
      team: "Équipe", invite: "Inviter", invited: "Invité", resend: "Renvoyer", cancel: "Annuler", remove: "Retirer",
      addPasskey: "Ajouter une passkey", signOut: "Se déconnecter", by: "par", noEmail: "Email non configuré",
      roles: { owner: "Propriétaire", admin: "Admin", editor: "Éditeur", viewer: "Lecteur" }, save: "OK",
      accounts: "Comptes", reconnect: "Reconnecter", synced: "Synchro", syncFailed: "Échec de synchro", neverSynced: "Jamais synchronisé", connectNet: "Connecter un réseau", noAccounts: "Aucun compte",
      redirects: "URI de redirection", noNetworks: "Aucune connexion réseau configurée",
      allOrgs: "Toutes les organisations", orgs: "Organisations", newOrg: "Nouvelle organisation", rename: "Renommer",
      pickOrg: "Choisissez d'abord une organisation en haut." }
  : { signIn: "Sign in", addKey: "Add a key", token: "API token", create: "Create", week: "Week", day: "Day",
      empty: "Nothing planned", cancelled: "Cancelled.", failed: "Failed", unsupported: "This browser doesn't support passkeys.",
      added: "Key added.", dup: "This key already exists.", open: "Open",
      drafts: "Drafts", draft: "Draft", noDrafts: "No drafts", noDate: "No date", plannedFor: "Planned for",
      scheduledFor: "Scheduled for", publishedAt: "Published", failedAt: "Failed, was due", publishing: "Publishing",
      scheduled: "Scheduled", published: "Published", failedS: "Failed", problems: "Fix before publishing", error: "Error",
      attempts: "attempts", nextTry: "next try", media: "Media", options: "Options", details: "Details", more: "more",
      comment: "Comment", title: "Title", description: "Description", caption: "Caption", local: "your time",
      created: "Created", updated: "Updated", post: "Post", group: "Group", missing: "media not found", close: "Close",
      readOnly: "Read-only · approve with approve_post", yes: "yes", no: "no", load: "Could not load",
      apps: "Apps", noApps: "No connected apps", full: "Full access", draftsOnly: "Drafts only", lastUsed: "used",
      revoke: "Revoke", email: "Email", sendLink: "Email me a link", sent: "Link sent, check your inbox.",
      join: "Join", name: "Your name", continue: "Continue", badLink: "This link is invalid or expired.",
      team: "Team", invite: "Invite", invited: "Invited", resend: "Resend", cancel: "Cancel", remove: "Remove",
      addPasskey: "Add a passkey", signOut: "Sign out", by: "by", noEmail: "Email not configured",
      roles: { owner: "Owner", admin: "Admin", editor: "Editor", viewer: "Viewer" }, save: "OK",
      accounts: "Accounts", reconnect: "Reconnect", synced: "Synced", syncFailed: "Sync failed", neverSynced: "Never synced", connectNet: "Connect a network", noAccounts: "No accounts",
      redirects: "Redirect URIs", noNetworks: "No network sign-in configured",
      allOrgs: "All organizations", orgs: "Organizations", newOrg: "New organization", rename: "Rename",
      pickOrg: "Pick an organization at the top first." };
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
const TEAM_ICON = "M16 19v-1a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v1M9 10a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM22 19v-1a4 4 0 0 0-3-3.9M16 4.1a3 3 0 0 1 0 5.8";
const ME_ICON = "M20 21v-1a5 5 0 0 0-5-5H9a5 5 0 0 0-5 5v1M12 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8z";
const LINK_ICON = "M10 14a5 5 0 0 0 7.1 0l3-3a5 5 0 0 0-7.1-7.1l-1.5 1.5M14 10a5 5 0 0 0-7.1 0l-3 3a5 5 0 0 0 7.1 7.1l1.5-1.5";
const APPS_ICON = "M9 3v4M15 3v4M7 7h10v4a5 5 0 0 1-10 0zM12 16v5";
const WARN_ICON = "M12 9v4m0 4h.01M10.3 3.9L2.4 18a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z";

const mobile = matchMedia("(max-width: 760px)");
const state = { me: null, view: "week", day: startOfDay(new Date()), posts: [], loadedFrom: null, drafts: [], org: loadOrg() };

// ---------- organizations ----------

// The header switcher narrows the calendar, the drafts and the accounts to one organization ("" = all).
function loadOrg() { try { return localStorage.getItem("relay.org") || ""; } catch { return ""; } }
function saveOrg(v) { try { localStorage.setItem("relay.org", v); } catch { /* private mode */ } }
function orgs() { return (state.me && state.me.orgs) || []; }
function orgName(id) { const o = orgs().find((o) => o.id === id); return o ? o.name : ""; }
// withOrg adds ?org= to the views the switcher filters.
function withOrg(path) {
  if (!state.org || !/^(calendar\?|drafts$|accounts$|posts\/)/.test(path)) return path;
  return path + (path.includes("?") ? "&" : "?") + "org=" + encodeURIComponent(state.org);
}

function renderOrgSwitch() {
  const sel = $("org-sel");
  const list = orgs();
  if (state.org && !list.some((o) => o.id === state.org)) { state.org = ""; saveOrg(""); }
  sel.hidden = list.length < 2;
  sel.replaceChildren();
  if (sel.hidden) return;
  const all = el("option", "", T.allOrgs); all.value = ""; sel.append(all);
  for (const o of list) { const op = el("option", "", o.name); op.value = o.id; op.selected = o.id === state.org; sel.append(op); }
}

// orgChips: toggles for the organizations a member sees; onChange gets the new list of ids.
function orgChips(selected, disabled, onChange) {
  const box = el("div", "org-chips");
  const cur = new Set(selected || []);
  for (const o of orgs()) {
    const b = el("button", "org-chip", o.name); b.type = "button";
    b.setAttribute("aria-pressed", String(cur.has(o.id)));
    b.disabled = disabled;
    b.addEventListener("click", async () => {
      const next = new Set(cur);
      if (next.has(o.id)) next.delete(o.id); else next.add(o.id);
      if (!next.size) return;
      try { await onChange([...next]); } catch (e) { alert(e.message); return; }
      if (cur.has(o.id)) cur.delete(o.id); else cur.add(o.id);
      b.setAttribute("aria-pressed", String(cur.has(o.id)));
    });
    box.append(b);
  }
  return box;
}

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
  const res = await fetch("/ui/api/" + withOrg(path), {
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
    afterLogin();
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
    await addPasskey(token);
    $("register").hidden = true;
    say(T.added);
  } catch (e) {
    say(e.name === "InvalidStateError" ? T.dup : e.name === "NotAllowedError" ? T.cancelled : `${T.failed}: ${e.message}`, true);
  }
}

// addPasskey registers a passkey for the signed-in member, or with the API token for the first owner.
async function addPasskey(token) {
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
}

async function emailLogin(ev) {
  ev.preventDefault();
  const input = $("email-input");
  const email = input.value.trim();
  if (!email) return;
  const btn = $("email-btn");
  btn.disabled = true;
  try { await api("email/login", { method: "POST", body: { email } }); say(T.sent); input.value = ""; }
  catch (e) { say(`${T.failed}: ${e.message}`, true); }
  btn.disabled = false;
}

// ---------- links sent by email (/ui/accept, /ui/signin) ----------

async function landing(kind) {
  const token = new URLSearchParams(location.search).get("token") || "";
  history.replaceState(null, "", "/ui/" + kind); // the token leaves the address bar
  $("landing").hidden = false;
  const btn = $("landing-btn"), msg = $("landing-msg"), name = $("landing-name");
  const bad = () => { msg.textContent = T.badLink; msg.className = "msg err"; $("landing-form").hidden = true; };
  if (kind === "accept") {
    btn.textContent = T.join;
    name.placeholder = T.name;
    name.hidden = false;
    try { const inv = await api("invite", { method: "POST", body: { token } }); $("landing-email").textContent = inv.email; }
    catch { return bad(); }
    name.focus();
  } else {
    btn.textContent = T.continue;
  }
  $("landing-form").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    btn.disabled = true;
    try {
      if (kind === "accept") await api("invite/accept", { method: "POST", body: { token, name: name.value.trim() } });
      else await api("email/signin", { method: "POST", body: { token } });
      location.replace("/ui/");
    } catch (e) {
      btn.disabled = false;
      if (e.status === 400 && kind === "accept" && !name.value.trim()) return name.focus();
      bad();
    }
  });
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
        const pm = i === 0 ? pp.media : t.media || [];
        if (pm.length) item.append(carousel(pm));
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
  if (!drawn && pp.account_avatar) {
    const av = el("img", "pv-av");
    av.src = pp.account_avatar; av.alt = ""; av.decoding = "async";
    av.addEventListener("error", () => av.remove(), { once: true });
    head.append(av);
  }
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
  if (!dlg.open) {
    lockPage(); dlg.showModal();
    // showModal focuses the first button (close), which draws a ring on touch screens: focus the sheet itself.
    body.tabIndex = -1; body.focus({ preventScroll: true });
  }

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

// ---------- connected apps (OAuth) ----------

async function openApps() {
  const dlg = $("preview");
  const body = $("preview-body");
  const top = el("div", "pv-top");
  const close = el("button", "icon"); close.type = "button"; close.setAttribute("aria-label", T.close); close.append(svg(CLOSE_ICON));
  close.addEventListener("click", () => dlg.close());
  const actions = el("div", "pv-actions");
  actions.append(close);
  top.append(el("div", "apps-title", T.apps), actions);
  const holder = el("div", "pv-body");
  holder.append(el("div", "pv-loading"));
  body.replaceChildren(top, holder);
  if (!dlg.open) { lockPage(); dlg.showModal(); body.tabIndex = -1; body.focus({ preventScroll: true }); }

  let data;
  try { data = await api("apps"); }
  catch (e) {
    if (e.status === 401) { dlg.close(); return showLogin(); }
    holder.replaceChildren(el("p", "pv-err", `${T.load}: ${e.message}`));
    return;
  }
  const apps = data.apps || [];
  if (!apps.length) return holder.replaceChildren(el("p", "empty", T.noApps));
  const list = el("div", "apps");
  const day = (t) => new Date(t).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
  for (const g of apps) {
    const row = el("div", "app-row");
    const info = el("div", "app-info");
    info.append(el("b", "", g.client_name || g.client_id));
    const meta = [g.scope === "relay:drafts" ? T.draftsOnly : T.full, day(g.created_at)];
    if (g.approved_by) meta.push(`${T.by} ${g.approved_by}`);
    if (g.last_used_at) meta.push(`${T.lastUsed} ${day(g.last_used_at)}`);
    info.append(el("span", "app-meta", meta.join(" · ")));
    const btn = el("button", "app-revoke", T.revoke); btn.type = "button";
    btn.addEventListener("click", async () => {
      btn.disabled = true;
      try { await api(`apps/${encodeURIComponent(g.id)}/revoke`, { method: "POST", body: {} }); row.remove(); }
      catch (e) { btn.disabled = false; if (e.status === 401) { dlg.close(); showLogin(); } }
      if (!list.children.length) holder.replaceChildren(el("p", "empty", T.noApps));
    });
    btn.hidden = g.can_revoke === false;
    row.append(info, btn);
    list.append(row);
  }
  holder.replaceChildren(list);
}

// ---------- sheets: account and team ----------

function openSheet(title) {
  const dlg = $("preview");
  const body = $("preview-body");
  const top = el("div", "pv-top");
  const close = el("button", "icon"); close.type = "button"; close.setAttribute("aria-label", T.close); close.append(svg(CLOSE_ICON));
  close.addEventListener("click", () => dlg.close());
  const actions = el("div", "pv-actions");
  actions.append(close);
  top.append(el("div", "apps-title", title), actions);
  const holder = el("div", "pv-body");
  body.replaceChildren(top, holder);
  if (!dlg.open) { lockPage(); dlg.showModal(); body.tabIndex = -1; body.focus({ preventScroll: true }); }
  return holder;
}

function smallBtn(text, cls, fn) {
  const b = el("button", "app-revoke " + (cls || ""), text); b.type = "button";
  b.addEventListener("click", async () => {
    b.disabled = true;
    try { await fn(); } catch (e) { b.disabled = false; if (e.status === 401) { $("preview").close(); showLogin(); } else alert(e.message); }
  });
  return b;
}

async function openMe() {
  let me = state.me;
  try { const r = await api("me"); me = state.me = r; } catch { /* keep */ }
  const u = (me && me.user) || {};
  const holder = openSheet(u.name || u.email || "Relay");
  const list = el("div", "apps");
  const row = el("div", "app-row");
  const info = el("div", "app-info");
  info.append(el("b", "", u.email || "—"), el("span", "app-meta", [T.roles[u.role] || u.role, `${u.passkeys || 0} passkey${u.passkeys === 1 ? "" : "s"}`].join(" · ")));
  row.append(info);
  if (me && me.registration_open !== false && window.PublicKeyCredential) {
    row.append(smallBtn(T.addPasskey, "plain", async () => {
      try { await addPasskey(); } catch (e) { if (e.name === "NotAllowedError") return openMe(); throw e; }
      openMe();
    }));
  }
  list.append(row);
  const out = el("button", "app-revoke", T.signOut); out.type = "button";
  out.addEventListener("click", logout);
  const foot = el("div", "sheet-foot");
  foot.append(out);
  holder.replaceChildren(list, foot);
}

function roleSelect(value, disabled, onChange) {
  const s = el("select", "role");
  for (const r of ["owner", "admin", "editor", "viewer"]) {
    const o = el("option", "", T.roles[r]); o.value = r; o.selected = r === value;
    s.append(o);
  }
  s.disabled = disabled;
  s.addEventListener("change", () => onChange(s.value, s));
  return s;
}

async function openTeam() {
  const holder = openSheet(T.team);
  holder.append(el("div", "pv-loading"));
  let data;
  try { data = await api("team"); }
  catch (e) {
    if (e.status === 401) { $("preview").close(); return showLogin(); }
    return holder.replaceChildren(el("p", "pv-err", `${T.load}: ${e.message}`));
  }
  const meID = data.me;
  const myRole = (data.members.find((m) => m.id === meID) || {}).role;
  const parts = [];

  if (data.email) {
    const form = el("form", "invite");
    const input = el("input"); input.type = "email"; input.required = true; input.placeholder = T.email; input.autocomplete = "off";
    const role = roleSelect("editor", false, () => {});
    if (myRole !== "owner") role.querySelector('option[value="owner"]').remove();
    const go = el("button", "", T.invite); go.type = "submit";
    form.append(input, role, go);
    let inviteOrgs = state.org ? [state.org] : orgs().slice(0, 1).map((o) => o.id);
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      go.disabled = true;
      try { await api("team/invite", { method: "POST", body: { email: input.value.trim(), role: role.value, lang: FR ? "fr" : "en", orgs: inviteOrgs } }); openTeam(); }
      catch (e) { go.disabled = false; alert(e.message); }
    });
    parts.push(form);
    if (orgs().length > 1) {
      const chips = orgChips(inviteOrgs, false, (ids) => { inviteOrgs = ids; });
      chips.classList.add("invite-orgs");
      parts.push(chips);
    }
  } else {
    parts.push(el("p", "app-meta pad", T.noEmail));
  }

  const list = el("div", "apps");
  const day = (t) => new Date(t).toLocaleDateString(undefined, { day: "numeric", month: "short" });
  for (const m of data.members) {
    const row = el("div", "app-row" + (m.disabled ? " off" : ""));
    const info = el("div", "app-info");
    info.append(el("b", "", m.name || m.email || "—"));
    const meta = [];
    if (m.name && m.email) meta.push(m.email);
    if (m.last_login_at) meta.push(day(m.last_login_at));
    if (meta.length) info.append(el("span", "app-meta", meta.join(" · ")));
    if (m.role !== "owner" && orgs().length > 1) {
      info.append(orgChips(m.orgs, m.id === meID, (ids) =>
        api(`team/members/${encodeURIComponent(m.id)}`, { method: "POST", body: { orgs: ids } })));
    }
    if (!m.email) {
      const f = el("form", "inline");
      const i = el("input"); i.type = "email"; i.required = true; i.placeholder = T.email;
      const b = el("button", "", T.save); b.type = "submit";
      f.append(i, b);
      f.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        try { await api(`team/members/${encodeURIComponent(m.id)}`, { method: "POST", body: { email: i.value.trim() } }); openTeam(); }
        catch (e) { alert(e.message); }
      });
      info.append(f);
    }
    const locked = m.role === "owner" && myRole !== "owner";
    const sel = roleSelect(m.role, locked, async (v, s) => {
      try { await api(`team/members/${encodeURIComponent(m.id)}`, { method: "POST", body: { role: v } }); }
      catch (e) { s.value = m.role; alert(e.message); return; }
      m.role = v;
      if (m.id === meID) location.reload();
    });
    if (myRole !== "owner") { const o = sel.querySelector('option[value="owner"]'); if (o && m.role !== "owner") o.remove(); }
    row.append(info, sel);
    if (m.id !== meID && !locked) {
      row.append(smallBtn(T.remove, "", async () => {
        await api(`team/members/${encodeURIComponent(m.id)}/remove`, { method: "POST", body: {} });
        row.remove();
      }));
    }
    list.append(row);
  }
  for (const inv of data.invites) {
    const row = el("div", "app-row pending");
    const info = el("div", "app-info");
    const invOrgs = (inv.orgs || []).map(orgName).filter(Boolean);
    info.append(el("b", "", inv.email), el("span", "app-meta", [T.invited, T.roles[inv.role] || inv.role, ...invOrgs].join(" · ")));
    row.append(info);
    if (data.email) row.append(smallBtn(T.resend, "plain", async () => { await api(`team/invites/${encodeURIComponent(inv.id)}/resend`, { method: "POST", body: {} }); openTeam(); }));
    row.append(smallBtn(T.cancel, "", async () => { await api(`team/invites/${encodeURIComponent(inv.id)}/revoke`, { method: "POST", body: {} }); row.remove(); }));
    list.append(row);
  }
  parts.push(list);
  if (state.me && state.me.manage_orgs) parts.push(...orgsAdmin());
  holder.replaceChildren(...parts);
}

// orgsAdmin: owners create and rename organizations.
function orgsAdmin() {
  const out = [el("div", "sheet-label", T.orgs)];
  const list = el("div", "apps");
  for (const o of orgs()) {
    const row = el("div", "app-row");
    const info = el("div", "app-info");
    info.append(el("b", "", o.name), el("span", "app-meta", `${o.slug} · ${o.accounts} ${T.accounts.toLowerCase()}`));
    row.append(info, smallBtn(T.rename, "plain", async () => {
      const name = prompt(T.rename, o.name);
      if (!name || name.trim() === o.name) return;
      try { await api(`orgs/${encodeURIComponent(o.id)}`, { method: "POST", body: { name: name.trim() } }); await refreshMe(); openTeam(); }
      catch (e) { alert(e.message); }
    }));
    list.append(row);
  }
  const form = el("form", "invite");
  const input = el("input"); input.required = true; input.placeholder = T.newOrg; input.maxLength = 80;
  const go = el("button", "", T.create); go.type = "submit";
  form.append(input, go);
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    go.disabled = true;
    try { await api("orgs", { method: "POST", body: { name: input.value.trim() } }); await refreshMe(); openTeam(); }
    catch (e) { go.disabled = false; alert(e.message); }
  });
  out.push(list, form);
  return out;
}

async function refreshMe() {
  try { state.me = await api("me"); } catch { /* keep */ }
  renderOrgSwitch();
}

// ---------- accounts: connect and reconnect with each network ----------

async function startConnection(body) {
  const r = await api("accounts/connect", { method: "POST", body });
  location.assign(r.url);
}

// syncLine: when Relay last reached the network for this account; red when the last call failed or
// nothing worked for a day.
const fRel = new Intl.RelativeTimeFormat(L, { numeric: "auto", style: "short" });
function ago(iso) {
  const s = (new Date(iso) - Date.now()) / 1000;
  const units = [["day", 86400], ["hour", 3600], ["minute", 60]];
  for (const [u, n] of units) if (Math.abs(s) >= n) return fRel.format(Math.round(s / n), u);
  return fRel.format(0, "minute");
}
function syncLine(a) {
  const line = el("span", "sync");
  if (a.sync_error) {
    line.classList.add("bad");
    line.textContent = `${T.syncFailed} ${ago(a.sync_error_at)}`;
    line.title = a.sync_error;
    return line;
  }
  if (!a.synced_at) { line.classList.add("bad"); line.textContent = T.neverSynced; return line; }
  if (Date.now() - new Date(a.synced_at) > 24 * 3600 * 1000) line.classList.add("bad");
  line.textContent = `${T.synced} ${ago(a.synced_at)}`;
  line.title = new Date(a.synced_at).toLocaleString(L);
  return line;
}

async function openAccounts() {
  const holder = openSheet(T.accounts);
  holder.append(el("div", "pv-loading"));
  let data;
  try { data = await api("accounts"); }
  catch (e) {
    if (e.status === 401) { $("preview").close(); return showLogin(); }
    return holder.replaceChildren(el("p", "pv-err", `${T.load}: ${e.message}`));
  }
  const parts = [];
  if (!data.accounts.length) parts.push(el("p", "empty", T.noAccounts));
  else {
    const list = el("div", "apps");
    for (const a of data.accounts) {
      const row = el("div", "app-row");
      const pic = el("span", "acc-pic");
      pic.style.color = netColor(a);
      if (a.avatar) { const img = el("img"); img.src = a.avatar; img.alt = ""; pic.append(img); }
      const badge = el("span", "acc-net"); badge.style.color = netColor(a); badge.append(netIcon(a));
      pic.append(badge);
      const info = el("div", "app-info");
      info.append(el("b", "", a.name || a.handle || a.id),
        el("span", "app-meta", [a.handle ? "@" + a.handle.replace(/^@/, "") : "", NET_NAMES[a.provider] || a.provider,
          !state.org && orgs().length > 1 ? a.org : ""].filter(Boolean).join(" · ")));
      info.append(syncLine(a));
      row.append(pic, info);
      if (orgs().length > 1) {
        const mv = el("select", "role");
        for (const o of orgs()) { const op = el("option", "", o.name); op.value = o.id; op.selected = o.id === a.org_id; mv.append(op); }
        mv.addEventListener("change", async () => {
          try { await api(`accounts/${encodeURIComponent(a.id)}/org`, { method: "POST", body: { org: mv.value } }); openAccounts(); }
          catch (e) { mv.value = a.org_id; alert(e.message); }
        });
        row.append(mv);
      }
      if (a.reconnect) row.append(smallBtn(T.reconnect, "plain", () => startConnection({ account_id: a.id })));
      list.append(row);
    }
    parts.push(list);
  }
  if (!data.networks.length) {
    parts.push(el("p", "app-meta pad", T.noNetworks));
  } else {
    parts.push(el("div", "sheet-label", T.connectNet));
    const nets = el("div", "net-btns");
    for (const n of data.networks) {
      const b = el("button", "net-btn"); b.type = "button";
      const ic = netIcon({ provider: n.connector }); ic.style.color = netColor({ provider: n.connector });
      b.append(ic, document.createTextNode(n.label));
      b.addEventListener("click", async () => {
        b.disabled = true;
        if (!state.org && orgs().length > 1) return alert(T.pickOrg);
        try { await startConnection({ network: n.slug, org: state.org }); }
        catch (e) { b.disabled = false; if (e.status === 401) { $("preview").close(); showLogin(); } else alert(e.message); }
      });
      nets.append(b);
    }
    parts.push(nets);
    const det = el("details", "redirects");
    det.append(el("summary", "", T.redirects));
    for (const n of data.networks) {
      const r = el("div", "redirect");
      r.append(el("span", "", n.label), el("code", "", n.redirect_uri));
      det.append(r);
    }
    parts.push(det);
  }
  holder.replaceChildren(...parts);
}

// ---------- navigation ----------

function step(n) {
  state.day = addDays(state.day, showDay() && !mobile.matches ? n : 7 * n);
  load();
}
function goToday() { state.day = startOfDay(new Date()); load(); }

function showLogin() { $("app").hidden = true; $("login").hidden = false; }

function showApp() { $("login").hidden = true; $("app").hidden = false; load(); loadDrafts(); }

// After a sign-in, the header buttons follow the member's role.
async function afterLogin() {
  showApp();
  try {
    const me = state.me = await api("me");
    $("apps-btn").hidden = !me.apps;
    $("team-btn").hidden = !me.team;
    $("accounts-btn").hidden = !me.accounts;
    renderOrgSwitch();
  } catch { /* keep */ }
}

document.addEventListener("DOMContentLoaded", async () => {
  const signIn = $("login-btn");
  signIn.append(svg(KEY_ICON), document.createTextNode(T.signIn));
  $("reg-toggle").textContent = T.addKey;
  $("reg-token").placeholder = T.token;
  $("reg-btn").textContent = T.create;
  $("email-input").placeholder = T.email;
  $("email-btn").textContent = T.sendLink;
  $("v-week").textContent = T.week;
  $("v-day").textContent = T.day;
  $("drafts-label").textContent = T.drafts;

  signIn.addEventListener("click", login);
  $("reg-toggle").addEventListener("click", () => { $("register").hidden = !$("register").hidden; $("reg-token").focus(); });
  $("register").addEventListener("submit", register);
  $("email-form").addEventListener("submit", emailLogin);
  $("me-btn").append(svg(ME_ICON));
  $("me-btn").addEventListener("click", openMe);
  $("team-btn").append(svg(TEAM_ICON));
  $("team-btn").addEventListener("click", openTeam);
  $("accounts-btn").append(svg(LINK_ICON));
  $("accounts-btn").addEventListener("click", openAccounts);
  $("apps-btn").append(svg(APPS_ICON));
  $("apps-btn").addEventListener("click", openApps);
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
  $("org-sel").addEventListener("change", () => {
    state.org = $("org-sel").value; saveOrg(state.org);
    state.loadedFrom = null;
    load(); loadDrafts();
  });

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

  const path = location.pathname.replace(/\/+$/, "");
  if (path === "/ui/accept" || path === "/ui/signin") return landing(path.slice(4));

  try {
    const me = state.me = await api("me");
    $("apps-btn").hidden = !me.apps;
    $("team-btn").hidden = !me.team;
    $("accounts-btn").hidden = !me.accounts;
    if (me.logged_in) {
      renderOrgSwitch();
      showApp();
      // Back from a network sign-in (/connect/...): show the accounts.
      if (location.hash === "#accounts" && me.accounts) { history.replaceState(null, "", location.pathname); openAccounts(); }
      return;
    }
    $("email-form").hidden = !me.email;
    // No passkey yet: with email on, members sign in by email; the API token form stays folded.
    if (!me.has_passkeys && me.email) $("login-btn").hidden = true;
    if (me.registration_open !== false) {
      $("reg-toggle").hidden = false;
      if (!me.has_passkeys && !me.email) $("register").hidden = false;
    }
  } catch { /* fall through to login */ }
  showLogin();
});
