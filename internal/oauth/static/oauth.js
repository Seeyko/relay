"use strict";
// Relay consent page: sign in with the dashboard passkey, choose the access, approve with a fresh passkey check.
// The request parameters stay on the server: this page only knows the request id.

const $ = (id) => document.getElementById(id);
const FR = (navigator.language || "").toLowerCase().startsWith("fr");
const T = FR
  ? { signIn: "Se connecter", full: "Accès complet", drafts: "Brouillons", approve: "Autoriser", deny: "Refuser",
      nokey: "Ajoutez d'abord une passkey dans le tableau de bord.", cancelled: "Annulé.", failed: "Échec",
      expired: "Demande expirée. Relancez la connexion depuis l'app.", unsupported: "Ce navigateur ne gère pas les passkeys." }
  : { signIn: "Sign in", full: "Full access", drafts: "Drafts only", approve: "Approve", deny: "Deny",
      nokey: "Add a passkey in the dashboard first.", cancelled: "Cancelled.", failed: "Failed",
      expired: "This request expired. Start again from the app.", unsupported: "This browser doesn't support passkeys." };
const LABELS = { "relay:admin": T.full, "relay:drafts": T.drafts };

const req = $("consent").dataset.req;
let csrf = "", scope = "", assertion = null;

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

async function call(path, body) {
  const res = await fetch(path, {
    method: "POST", headers: { "Content-Type": "application/json" }, credentials: "same-origin", cache: "no-store",
    body: JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) { const e = new Error(data.error || res.statusText); e.status = res.status; throw e; }
  return data;
}

function say(text, err) { const m = $("msg"); m.textContent = text; m.className = "msg" + (err ? " err" : ""); }

function fail(e) {
  if (e.status === 404) { done(); return say(T.expired, true); }
  say(e.name === "NotAllowedError" ? T.cancelled : `${T.failed}: ${e.message}`, e.name !== "NotAllowedError");
}

function done() { $("ask").hidden = true; $("signin").hidden = true; $("deny").hidden = true; }

function publicKey(opts) {
  const pk = opts.publicKey;
  pk.challenge = b64uToBuf(pk.challenge);
  (pk.allowCredentials || []).forEach((c) => (c.id = b64uToBuf(c.id)));
  return pk;
}

function encode(cred) {
  const r = cred.response;
  return {
    id: cred.id, rawId: bufToB64u(cred.rawId), type: cred.type,
    response: { authenticatorData: bufToB64u(r.authenticatorData), clientDataJSON: bufToB64u(r.clientDataJSON),
      signature: bufToB64u(r.signature), userHandle: r.userHandle ? bufToB64u(r.userHandle) : "" },
  };
}

// The passkey challenge is fetched before the tap, so Safari runs the passkey prompt inside the tap.
async function prepare() {
  assertion = null;
  try { assertion = publicKey(await call("/oauth/api/approve/begin", { req, csrf })); } catch (e) { fail(e); }
}

async function signIn() {
  if (!window.PublicKeyCredential) return say(T.unsupported, true);
  say("");
  try {
    const opts = await fetch("/ui/api/login/begin", {
      method: "POST", headers: { "Content-Type": "application/json" }, credentials: "same-origin", body: "{}",
    }).then((r) => r.json());
    const cred = await navigator.credentials.get({ publicKey: publicKey(opts) });
    const res = await fetch("/ui/api/login/finish", {
      method: "POST", headers: { "Content-Type": "application/json" }, credentials: "same-origin", body: JSON.stringify(encode(cred)),
    });
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
    await load();
  } catch (e) { fail(e); }
}

async function approve() {
  if (!window.PublicKeyCredential) return say(T.unsupported, true);
  if (!assertion) await prepare();
  if (!assertion) return;
  say("");
  const btn = $("approve");
  btn.disabled = true;
  try {
    const cred = await navigator.credentials.get({ publicKey: assertion });
    const res = await call("/oauth/api/approve/finish", { req, csrf, scope, credential: encode(cred) });
    done();
    location.assign(res.redirect);
  } catch (e) {
    fail(e);
    btn.disabled = false;
    prepare();
  }
}

async function deny() {
  try { const res = await call("/oauth/api/deny", { req }); done(); location.assign(res.redirect); } catch (e) { fail(e); }
}

function renderScopes(list) {
  const box = $("scopes");
  box.replaceChildren();
  scope = list[0];
  for (const sc of list) {
    const b = document.createElement("button");
    b.type = "button";
    b.setAttribute("role", "radio");
    b.textContent = LABELS[sc] || sc;
    b.setAttribute("aria-checked", String(sc === scope));
    b.addEventListener("click", () => {
      scope = sc;
      for (const o of box.children) o.setAttribute("aria-checked", String(o === b));
    });
    box.append(b);
  }
}

async function load() {
  let st;
  try { st = await call("/oauth/api/state", { req }); } catch (e) { return fail(e); }
  $("deny").hidden = false;
  if (st.logged_in) {
    csrf = st.csrf;
    renderScopes(st.scopes);
    $("signin").hidden = true;
    $("ask").hidden = false;
    prepare();
  } else if (st.has_passkeys) {
    $("signin").hidden = false;
  } else {
    $("nokey").hidden = false;
    const a = document.createElement("a");
    a.href = "/ui/";
    a.textContent = T.nokey;
    $("nokey").replaceChildren(a);
  }
}

document.addEventListener("DOMContentLoaded", () => {
  $("signin").textContent = T.signIn;
  $("approve").textContent = T.approve;
  $("deny").textContent = T.deny;
  $("signin").addEventListener("click", signIn);
  $("approve").addEventListener("click", approve);
  $("deny").addEventListener("click", deny);
  load();
});
