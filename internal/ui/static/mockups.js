"use strict";
// Network mockups: the preview drawn like the real post in each network's app (2026 layouts, light and dark).
// Visual reference only, no code copied: Postiz provider previews (AGPL-3.0) and Mixpost PostPreview
// components (MIT). Built with DOM APIs and textContent only, like the rest of the dashboard.
// Uses el, svg, counter, fmtIn, fCompact, FR and L from app.js.

const NT = FR
  ? { more: "plus", seeMore: "Voir plus", like: "J'aime", comment: "Commenter", share: "Partager", repost: "Republier",
      send: "Envoyer", comments: "commentaires", shares: "partages", reposts: "republications", followers: "abonnés",
      subscribers: "abonnés", subscribe: "S'abonner", views: "vues", save: "Enregistrer", remix: "Remixer",
      download: "Télécharger", dislike: "Je n'aime pas", follow: "Suivre", reels: "Reels", audio: "Son original",
      sound: "son original", following: "Abonnements", forYou: "Pour toi", sendMsg: "Envoyer un message",
      viewAll: (n) => `Afficher les ${n} commentaires`, author: "Auteur", reply: "Répondre", shorts: "Shorts" }
  : { more: "more", seeMore: "See more", like: "Like", comment: "Comment", share: "Share", repost: "Repost",
      send: "Send", comments: "comments", shares: "shares", reposts: "reposts", followers: "followers",
      subscribers: "subscribers", subscribe: "Subscribe", views: "views", save: "Save", remix: "Remix",
      download: "Download", dislike: "Dislike", follow: "Follow", reels: "Reels", audio: "Original audio",
      sound: "original sound", following: "Following", forYou: "For You", sendMsg: "Send message",
      viewAll: (n) => `View all ${n} comments`, author: "Author", reply: "Reply", shorts: "Shorts" };

// 24×24 glyphs. Stroked unless drawn with filled: true.
const G = {
  more: "M5 12h.01M12 12h.01M19 12h.01",
  moreV: "M12 5h.01M12 12h.01M12 19h.01",
  close: "M6 6l12 12M18 6L6 18",
  heart: "M12 20.3C12 20.3 3 14.9 3 8.9 3 6.1 5.2 4 7.9 4c1.7 0 3.2.9 4.1 2.2C12.9 4.9 14.4 4 16.1 4 18.8 4 21 6.1 21 8.9c0 6-9 11.4-9 11.4z",
  bubble: "M20.6 16.9A9.2 9.2 0 1 0 16.9 20.6L21.4 21.4z",
  xReply: "M3.5 11.2c0-3.9 3.2-7 7.1-7h2.7a7.2 7.2 0 0 1 .2 14.4l-6.2 3.3v-3.7a7.1 7.1 0 0 1-3.8-7z",
  xRepost: "M4.5 8.5L7.5 5.5 10.5 8.5M7.5 6v9.5a3 3 0 0 0 3 3H14M19.5 15.5l-3 3-3-3M16.5 18V8.5a3 3 0 0 0-3-3H10",
  xViews: "M4.5 20v-6M9.5 20V5M14.5 20v-9M19.5 20v-12",
  bookmark: "M6.5 3.5h11v17l-5.5-4-5.5 4z",
  upload: "M12 3.5V15M7.5 8L12 3.5 16.5 8M4.5 14v4.5a2 2 0 0 0 2 2h11a2 2 0 0 0 2-2V14",
  plane: "M21.5 2.5L2.5 9.3l7.7 3.2 3.2 7.7zM10.2 12.5l11.3-10",
  repost: "M17 2.5l3 3-3 3M4 11.5V9.5a4 4 0 0 1 4-4h12M7 21.5l-3-3 3-3M20 12.5v2a4 4 0 0 1-4 4H4",
  thumb: "M7 10.5v10H3.5v-10zM7 10.5l4.2-7.5c1.6 0 2.8 1.3 2.8 2.9v3.6h5.4a2 2 0 0 1 2 2.3l-1.3 6.9a2.2 2.2 0 0 1-2.2 1.8H7",
  thumbDown: "M17 13.5v-10h3.5v10zM17 13.5l-4.2 7.5c-1.6 0-2.8-1.3-2.8-2.9v-3.6H4.6a2 2 0 0 1-2-2.3l1.3-6.9A2.2 2.2 0 0 1 6.1 3.5H17",
  fbComment: "M12 3.5c5 0 9 3.6 9 8s-4 8-9 8c-1.3 0-2.5-.2-3.6-.7L3.5 20.5l1.4-4.3A7.5 7.5 0 0 1 3 11.5c0-4.4 4-8 9-8z",
  fbShare: "M13.5 4.5l7.5 7-7.5 7v-4c-4.9 0-8.3 1.4-10.5 4.6.8-4.9 3.7-9.4 10.5-10.3z",
  liComment: "M4 4.5h16v11.5H9.5L4 20.5z",
  globe: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM3 12h18M12 3c2.4 2.5 3.6 5.5 3.6 9s-1.2 6.5-3.6 9c-2.4-2.5-3.6-5.5-3.6-9S9.6 5.5 12 3z",
  music: "M9 18V5.5l11-2V16M9 18a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0zM20 16a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0z",
  search: "M10.5 3.5a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM20.5 20.5l-5-5",
  camera: "M3.5 7.5h3.5l2-3h6l2 3h3.5v12h-17zM12 9.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7z",
  chevron: "M6 9l6 6 6-6",
  play: "M8 5v14l11-7z",
  plus: "M12 6v12M6 12h12",
  remix: "M4 7h9a4 4 0 0 1 0 8H8M7 12l-3 3 3 3M20 17h-3",
  saveList: "M3.5 6h12M3.5 11.5h12M3.5 17h7M18 13.5v7M14.5 17h7",
  download: "M12 4v11M7.5 10.5L12 15l4.5-4.5M5 20h14",
  ttComment: "M12 3C6.8 3 2.5 6.6 2.5 11.1c0 2.6 1.4 4.9 3.6 6.4L5.4 21.5l4.4-2.3c.7.1 1.4.2 2.2.2 5.2 0 9.5-3.6 9.5-8.1S17.2 3 12 3zM7.5 12.4a1.3 1.3 0 1 1 0-2.6 1.3 1.3 0 0 1 0 2.6zm4.5 0a1.3 1.3 0 1 1 0-2.6 1.3 1.3 0 0 1 0 2.6zm4.5 0a1.3 1.3 0 1 1 0-2.6 1.3 1.3 0 0 1 0 2.6z",
  ttShare: "M13.5 3.5L22 11.4l-8.5 7.9v-4.6c-5.3 0-8.8 1.5-11.5 5.3.9-5.7 4.4-10.6 11.5-11.6z",
  ytShare: "M14 5l7 6.5-7 6.5v-4c-5 0-8.2 1.4-10.5 4.5C4.4 13.8 7.4 9.6 14 9z",
};

// ---------- small helpers ----------

function ic(d, cls, filled) {
  const s = svg(d, { filled, evenodd: filled });
  if (cls) s.setAttribute("class", cls);
  return s;
}
const isVid = (m) => (m.mime || "").startsWith("video/");
const num = (v) => (v > 0 ? fCompact.format(v) : "");
const at = (h) => "@" + String(h || "").replace(/^@+/, "");
const bare = (h) => String(h || "").replace(/^@+/, "");

function hueOf(s) {
  let h = 0;
  for (const ch of s) h = (h * 31 + ch.codePointAt(0)) % 360;
  return h;
}
// The account's profile picture (c.av, a /media/ path), or the first letter of its name on a colored
// circle when Relay has none or the image does not load. "sq" draws it square-rounded (LinkedIn Page logos).
function avatar(c, size, cls) {
  const a = el("span", "nw-av" + (cls ? " " + cls : ""));
  a.style.setProperty("--s", size + "px");
  // One color per brand whatever the network spells it: "Ride My Park", "RideMyPark", "@ridemypark".
  a.style.setProperty("--h", String(hueOf((c.name || c.handle || "?").toLowerCase().replace(/[^\p{L}\p{N}]/gu, ""))));
  const letter = () => { a.classList.remove("pic"); a.textContent = Array.from((c.name || c.handle || "?").trim())[0].toUpperCase(); };
  a.setAttribute("aria-hidden", "true");
  if (!c.av) { letter(); return a; }
  const i = el("img");
  i.alt = ""; i.decoding = "async"; i.draggable = false;
  i.addEventListener("error", letter, { once: true });
  i.src = c.av;
  a.classList.add("pic");
  a.append(i);
  return a;
}

// Hashtags, mentions and links in the network's link color. URLs are shortened like the apps do.
const TOKENS = /(https?:\/\/[^\s<>"]+|(?<![\p{L}\p{N}_&])#[\p{L}\p{N}_]+|(?<![\p{L}\p{N}_.])@[\p{L}\p{N}_.]*[\p{L}\p{N}_])/gu;
function shortURL(u) {
  const s = u.replace(/^https?:\/\/(www\.)?/, "");
  return s.length > 26 ? s.slice(0, 25) + "…" : s;
}
function rich(text) {
  const f = document.createDocumentFragment();
  let last = 0;
  for (const m of text.matchAll(TOKENS)) {
    let tok = m[0];
    if (tok.startsWith("http")) tok = tok.replace(/[.,;:!?)\]'"»]+$/, "");
    if (m.index > last) f.append(text.slice(last, m.index));
    f.append(el("span", "lk", tok.startsWith("http") ? shortURL(tok) : tok));
    last = m.index + tok.length;
  }
  f.append(text.slice(last));
  return f;
}

// Where the "more" cut falls: the fold, the line break that ends the visible lines, or the last space near it.
function cutAt(text, fold, lines = 1) {
  const chars = Array.from(text);
  let cut = fold || 0;
  let nl = -1;
  for (let k = 0; k < lines; k++) { nl = chars.indexOf("\n", nl + 1); if (nl < 0) break; }
  if (cut && nl >= 0 && nl < cut) cut = nl;
  if (cut >= chars.length) return 0;
  if (cut && cut === fold) {
    const sp = chars.slice(0, cut).lastIndexOf(" ");
    if (sp > cut - 25) cut = sp;
  }
  return cut;
}

// Text with the network's "more" cut. lead() returns a node shown before the text (Instagram's handle).
function folded(text, { cls, fold, more, lead, ell = "…", lines = 1 }) {
  const box = el("div", cls);
  const fill = (s) => {
    box.replaceChildren();
    if (lead) box.append(lead(), " ");
    box.append(rich(s));
  };
  const cut = cutAt(text || "", fold, lines);
  if (!cut) { fill(text || ""); return box; }
  fill(Array.from(text).slice(0, cut).join("").trimEnd());
  const b = el("button", "nw-more", more);
  b.type = "button";
  b.addEventListener("click", (e) => { e.stopPropagation(); fill(text); });
  box.append(ell, b);
  return box;
}

// One media file. Videos show their first frame and play on tap.
function media(m) {
  if (m.missing) return el("div", "nw-missing", T.missing);
  if (isVid(m)) {
    const v = el("video");
    v.src = m.path + "#t=0.1"; v.muted = true; v.loop = true; v.playsInline = true; v.preload = "metadata";
    v.addEventListener("click", (e) => { e.stopPropagation(); v.paused ? v.play() : v.pause(); });
    return v;
  }
  const i = el("img");
  i.src = m.path; i.alt = ""; i.decoding = "async"; i.draggable = false;
  return i;
}
function natural(node, cb) {
  if (node.tagName === "IMG") {
    if (node.complete && node.naturalWidth) cb(node.naturalWidth / node.naturalHeight);
    else node.addEventListener("load", () => node.naturalWidth && cb(node.naturalWidth / node.naturalHeight), { once: true });
  } else if (node.tagName === "VIDEO") {
    node.addEventListener("loadedmetadata", () => node.videoWidth && cb(node.videoWidth / node.videoHeight), { once: true });
  }
}
// Size a box to the media's own shape, within the network's allowed range.
function fit(box, node, min, max, initial) {
  box.style.aspectRatio = initial;
  natural(node, (r) => { box.style.aspectRatio = String(Math.min(max, Math.max(min, r))); });
}
function playBadge(cls) {
  const b = el("span", "nw-play " + (cls || ""));
  b.append(ic(G.play, "", true));
  return b;
}

// Grid of up to max cells; the last one says how many more there are.
function grid(c, cls, max) {
  const list = c.media.slice(0, max);
  const g = el("div", `${cls} n${list.length}`);
  list.forEach((m, i) => {
    const cell = el("div", "nw-cell");
    cell.append(media(m));
    if (isVid(m) || (c.video && list.length === 1)) cell.append(playBadge());
    if (i === max - 1 && c.media.length > max) cell.append(el("span", "nw-extra", "+" + (c.media.length - max)));
    g.append(cell);
  });
  return g;
}

function action(d, label, cls, filled) {
  const a = el("span", "nw-act" + (cls ? " " + cls : ""));
  a.append(ic(d, "", filled));
  if (label) a.append(el("span", "", label));
  return a;
}

// ---------- time ----------

const rtf = new Intl.RelativeTimeFormat(L, { numeric: "auto" });
function rel(d, mode, tz) {
  const s = Math.max(0, (Date.now() - d) / 1000);
  const dayMonth = () => fmtIn(tz, d, { day: "numeric", month: mode === "long" ? "long" : "short" });
  if (mode === "long") {
    if (s < 3600) return rtf.format(-Math.max(1, Math.floor(s / 60)), "minute");
    if (s < 86400) return rtf.format(-Math.floor(s / 3600), "hour");
    if (s < 7 * 86400) return rtf.format(-Math.floor(s / 86400), "day");
    if (s < 30 * 86400) return rtf.format(-Math.floor(s / 604800), "week");
    return dayMonth();
  }
  if (s < 3600) return Math.max(1, Math.floor(s / 60)) + (FR ? " min" : "m");
  if (s < 86400) return Math.floor(s / 3600) + (FR ? " h" : "h");
  if (mode === "x") return dayMonth();
  if (s < 7 * 86400) return Math.floor(s / 86400) + (FR ? " j" : "d");
  if (s < 35 * 86400) return Math.floor(s / 604800) + (FR ? " sem." : "w");
  return dayMonth();
}
// Published: the network's relative time. Otherwise: the planned or scheduled day (the hour is above the mockup).
function stamp(c, mode) {
  const p = c.pp.post;
  if (p.status === "published" && p.published_at) return rel(new Date(p.published_at), mode, c.tz);
  const when = p.planned_at || p.scheduled_at;
  return when ? fmtIn(c.tz, new Date(when), { day: "numeric", month: "short" }) : "";
}

// ---------- X ----------

function mockX(c) {
  const root = el("div", "nw nw-x");
  const parts = c.pp.texts.length ? c.pp.texts : [{ text: "" }];
  parts.forEach((t, i) => {
    const row = el("article", "x-post" + (i < parts.length - 1 ? " cont" : ""));
    const side = el("div", "x-side");
    side.append(avatar(c, 40));
    const main = el("div", "x-main");
    const head = el("div", "x-head");
    head.append(el("b", "x-name", c.name), el("span", "x-sub", `${at(c.handle)} · ${stamp(c, "x")}`), ic(G.more, "x-more"));
    main.append(head);
    if (t.text) main.append(folded(t.text, { cls: "x-text", fold: t.limit > 280 ? 280 : 0, more: FR ? "Afficher plus" : "Show more", ell: "… " }));
    if (i === 0 && c.media.length) {
      const g = grid(c, "x-media", 4);
      if (c.media.length === 1) fit(g, g.querySelector("img,video") || g, 0.75, 1.91, "16 / 9");
      main.append(g);
    }
    const st = i === 0 ? c.st : {};
    const bar = el("div", "x-actions");
    bar.append(action(G.xReply, num(st.comments)), action(G.xRepost, num(st.shares)), action(G.heart, num(st.likes)),
      action(G.xViews, num(st.views)));
    const right = el("span", "x-right");
    right.append(ic(G.bookmark), ic(G.upload));
    bar.append(right);
    main.append(bar);
    row.append(side, main);
    root.append(row);
  });
  return root;
}

// ---------- Threads ----------

function mockThreads(c) {
  const root = el("div", "nw nw-th");
  const parts = c.pp.texts.length ? c.pp.texts : [{ text: "" }];
  parts.forEach((t, i) => {
    const row = el("article", "th-post" + (i < parts.length - 1 ? " cont" : ""));
    const side = el("div", "th-side");
    side.append(avatar(c, 36));
    const main = el("div", "th-main");
    const head = el("div", "th-head");
    head.append(el("b", "", bare(c.handle)), el("span", "th-time", stamp(c, "compact")), ic(G.more, "th-more"));
    main.append(head);
    if (t.text) main.append(folded(t.text, { cls: "th-text" }));
    if (i === 0 && c.media.length === 1) {
      const box = el("div", "th-one");
      const n = media(c.media[0]);
      box.append(n);
      if (c.video) box.append(playBadge());
      fit(box, n, 0.75, 1.91, "4 / 5");
      main.append(box);
    } else if (i === 0 && c.media.length) {
      const strip = el("div", "th-strip");
      c.media.forEach((m) => {
        const f = el("div", "th-cell");
        const n = media(m);
        f.append(n);
        fit(f, n, 0.56, 1.91, "3 / 4");
        strip.append(f);
      });
      main.append(strip);
    }
    const st = i === 0 ? c.st : {};
    const bar = el("div", "th-actions");
    bar.append(action(G.heart, num(st.likes)), action(G.bubble, num(st.comments)), action(G.repost, num(st.shares)), action(G.plane, ""));
    main.append(bar);
    row.append(side, main);
    root.append(row);
  });
  return root;
}

// ---------- Instagram ----------

function slides(c, cls, onIndex) {
  const track = el("div", cls);
  c.media.forEach((m) => {
    const s = el("div", "nw-slide");
    s.append(media(m));
    track.append(s);
  });
  if (c.media.length > 1) {
    let idx = 0;
    track.addEventListener("scroll", () => {
      const i = Math.round(track.scrollLeft / Math.max(1, track.clientWidth));
      if (i !== idx) { idx = i; onIndex(i); }
    }, { passive: true });
  }
  return track;
}
function dots(n, cls) {
  const d = el("div", cls);
  for (let i = 0; i < n; i++) d.append(el("i", i === 0 ? "on" : ""));
  return { node: d, set: (k) => d.querySelectorAll("i").forEach((x, j) => x.classList.toggle("on", j === k)) };
}

function mockInstagram(c) {
  if (c.story) return igStory(c);
  if (c.reel) return vertical(c, "ig");
  const root = el("div", "nw nw-ig");
  const head = el("div", "ig-head");
  head.append(avatar(c, 32), el("b", "", bare(c.handle)), ic(G.more, "ig-more"));
  root.append(head);
  const n = c.media.length;
  let dd = null;
  if (n) {
    const wrap = el("div", "ig-media");
    const badge = el("span", "ig-count", `1/${n}`);
    dd = n > 1 ? dots(n, "ig-dots") : null;
    const track = slides(c, "ig-track", (i) => { badge.textContent = `${i + 1}/${n}`; dd.set(i); });
    wrap.append(track);
    if (n > 1) wrap.append(badge);
    if (c.video && n === 1) wrap.append(playBadge());
    // Instagram crops every slide to the first one's shape, between 4:5 and 1.91:1.
    fit(wrap, track.querySelector("img,video") || track, 0.8, 1.91, "4 / 5");
    root.append(wrap);
  }
  const bar = el("div", "ig-actions");
  bar.append(action(G.heart, num(c.st.likes)), action(G.bubble, num(c.st.comments)), action(G.repost, ""),
    action(G.plane, num(c.st.shares)));
  if (dd) bar.append(dd.node);
  bar.append(action(G.bookmark, "", "ig-save"));
  root.append(bar);
  const t = c.pp.texts[0];
  if (t && t.text) {
    root.append(folded(t.text, { cls: "ig-cap", fold: t.fold, more: NT.more, ell: "… ",
      lead: () => el("b", "", bare(c.handle)) }));
  }
  if (c.st.comments > 0) root.append(el("div", "ig-all", NT.viewAll(fCompact.format(c.st.comments))));
  root.append(el("div", "ig-time", stamp(c, "long")));
  return root;
}

function igStory(c) {
  const root = el("div", "nw vt vt-story");
  const stage = el("div", "vt-stage");
  if (c.media[0]) stage.append(media(c.media[0]));
  root.append(stage, el("div", "vt-shade top"));
  const bars = el("div", "st-bars");
  (c.media.length ? c.media : [0]).forEach((_, i) => bars.append(el("i", i === 0 ? "on" : "")));
  const head = el("div", "st-head");
  head.append(avatar(c, 32), el("b", "", bare(c.handle)), el("span", "st-time", stamp(c, "compact")), el("span", "nw-flex"),
    ic(G.more), ic(G.close));
  const foot = el("div", "st-foot");
  foot.append(el("span", "st-input", NT.sendMsg), ic(G.heart), ic(G.plane));
  root.append(bars, head, foot);
  return root;
}

// ---------- full-screen vertical video: Reels, TikTok, Shorts, Facebook Reels ----------

function vertical(c, net) {
  const root = el("div", `nw vt vt-${net}`);
  const n = c.media.length;
  const stage = el("div", "vt-stage");
  let dd = null;
  if (n > 1) {
    dd = dots(n, "vt-dots");
    stage.append(slides(c, "vt-track", (i) => dd.set(i)));
  } else if (n) stage.append(media(c.media[0]));
  root.append(stage, el("div", "vt-shade top"), el("div", "vt-shade"));

  const top = el("div", "vt-top");
  const rail = el("div", "vt-rail");
  const info = el("div", "vt-info");
  const t = c.pp.texts[0] || { text: "" };
  const st = c.st;
  const item = (d, label, filled) => { const a = action(d, label, "vt-act", filled); rail.append(a); return a; };

  if (net === "tt") {
    const tabs = el("div", "tt-tabs");
    tabs.append(el("span", "", NT.following), el("b", "", NT.forYou));
    top.append(el("span", "nw-flex"), tabs, el("span", "nw-flex tt-search"));
    top.lastChild.append(ic(G.search));
    const av = el("span", "tt-av");
    const plus = el("span", "tt-plus");
    plus.append(ic(G.plus));
    av.append(avatar(c, 46), plus);
    rail.append(av);
    item(G.heart, num(st.likes) || "0", true);
    item(G.ttComment, num(st.comments) || "0", true);
    item(G.bookmark, num(st.saves) || "0", true);
    item(G.ttShare, num(st.shares) || NT.share, true);
    const disc = el("span", "tt-disc");
    disc.append(avatar(c, 24));
    rail.append(disc);
    info.append(el("b", "vt-name", bare(c.handle)));
    if (c.pp.title && c.pp.title.text) info.append(el("b", "tt-title", c.pp.title.text));
    if (t.text) info.append(folded(t.text, { cls: "vt-cap", fold: 70, more: NT.more, ell: "… " }));
    const mus = el("div", "vt-audio");
    mus.append(ic(G.music), el("span", "", `${NT.sound} - ${bare(c.handle)}`));
    info.append(mus);
  } else if (net === "yt") {
    top.append(el("span", "nw-flex"), ic(G.search), ic(G.moreV));
    item(G.thumb, num(st.likes) || NT.like, true);
    item(G.thumbDown, NT.dislike, true);
    item(G.ttComment, num(st.comments) || "0", true);
    item(G.ytShare, NT.share, true);
    item(G.remix, NT.remix);
    const ch = el("span", "yt-thumb");
    ch.append(avatar(c, 40, "sq"));
    rail.append(ch);
    const who = el("div", "vt-who");
    who.append(avatar(c, 28), el("b", "", at(c.handle)), el("span", "yt-sub", NT.subscribe));
    info.append(who);
    const title = (c.pp.title && c.pp.title.text) || t.text;
    if (title) info.append(el("div", "yt-stitle", title));
    root.append(el("div", "yt-progress"));
  } else { // Instagram Reels and Facebook Reels
    const brand = el("b", "vt-brand", NT.reels);
    brand.append(ic(G.chevron));
    top.append(brand, el("span", "nw-flex"), ic(G.camera));
    if (net === "fb") {
      item(G.thumb, num(st.likes) || NT.like);
      item(G.fbComment, num(st.comments) || "");
      item(G.fbShare, num(st.shares) || NT.share);
    } else {
      item(G.heart, num(st.likes));
      item(G.bubble, num(st.comments));
      item(G.repost, "");
      item(G.plane, num(st.shares));
    }
    item(G.more, "");
    const thumb = el("span", "ig-audio");
    thumb.append(avatar(c, 28, "sq"));
    rail.append(thumb);
    const who = el("div", "vt-who");
    who.append(avatar(c, 32), el("b", "", net === "fb" ? c.name : bare(c.handle)), el("span", "vt-follow", NT.follow));
    info.append(who);
    if (t.text) info.append(folded(t.text, { cls: "vt-cap", fold: 34, more: NT.more, ell: "… " }));
    const mus = el("div", "vt-audio");
    mus.append(ic(G.music), el("span", "", `${bare(c.handle)} · ${NT.audio}`));
    info.append(mus);
  }
  if (dd) info.prepend(dd.node);
  root.append(top, rail, info);
  return root;
}

// ---------- Facebook Page ----------

function mockFacebook(c) {
  if (c.reel) return vertical(c, "fb");
  const root = el("div", "nw nw-fb");
  const head = el("div", "fb-head");
  const who = el("div", "fb-who");
  const sub = el("span", "fb-sub", stamp(c, "compact") + " · ");
  sub.append(ic(G.globe));
  who.append(el("b", "", c.name), sub);
  head.append(avatar(c, 40), who, ic(G.more, "fb-ic"), ic(G.close, "fb-ic"));
  root.append(head);
  const t = c.pp.texts[0];
  if (t && t.text) {
    const big = !c.media.length && Array.from(t.text).length <= 85 && !t.text.includes("\n");
    root.append(folded(t.text, { cls: "fb-text" + (big ? " big" : ""), fold: t.fold, more: NT.seeMore, ell: "… ", lines: 4 }));
  }
  if (c.media.length) {
    const g = grid(c, "fb-media", 5);
    if (c.media.length === 1) fit(g, g.querySelector("img,video") || g, 0.8, 1.91, "1 / 1");
    root.append(g);
  }
  const st = c.st;
  if (st.likes || st.comments || st.shares) {
    const counts = el("div", "fb-counts");
    const l = el("span", "fb-likes");
    if (st.likes) { const b = el("span", "fb-badge"); b.append(ic(G.thumb, "", true)); l.append(b, el("span", "", num(st.likes))); }
    const r = el("span", "", [st.comments && `${num(st.comments)} ${NT.comments}`, st.shares && `${num(st.shares)} ${NT.shares}`].filter(Boolean).join("  "));
    counts.append(l, r);
    root.append(counts);
  }
  const bar = el("div", "fb-actions");
  bar.append(action(G.thumb, NT.like), action(G.fbComment, NT.comment), action(G.fbShare, NT.share));
  root.append(bar);
  return root;
}

// ---------- LinkedIn page ----------

function mockLinkedIn(c) {
  const root = el("div", "nw nw-li");
  const head = el("div", "li-head");
  const who = el("div", "li-who");
  who.append(el("b", "", c.name));
  if (c.pp.followers) who.append(el("span", "", `${new Intl.NumberFormat(L).format(c.pp.followers)} ${NT.followers}`));
  const when = el("span", "", stamp(c, "compact") + " • ");
  when.append(ic(G.globe));
  who.append(when);
  head.append(avatar(c, 48, "sq"), who, ic(G.more, "li-ic"));
  root.append(head);
  const [first, ...comments] = c.pp.texts;
  if (first && first.text) root.append(folded(first.text, { cls: "li-text", fold: first.fold, more: NT.more, ell: " …", lines: 3 }));
  if (c.media.length) {
    const g = grid(c, "li-media", 4);
    if (c.media.length === 1) fit(g, g.querySelector("img,video") || g, 0.8, 1.91, "1.91 / 1");
    root.append(g);
  }
  const st = c.st;
  if (st.likes || st.comments || st.shares) {
    const counts = el("div", "li-counts");
    const l = el("span", "li-likes");
    if (st.likes) {
      const b = el("span", "li-badge");
      b.append(ic(G.thumb, "", true));
      l.append(b, el("span", "", num(st.likes)));
    }
    counts.append(l, el("span", "", [st.comments && `${num(st.comments)} ${NT.comments}`, st.shares && `${num(st.shares)} ${NT.reposts}`].filter(Boolean).join(" • ")));
    root.append(counts);
  }
  const bar = el("div", "li-actions");
  bar.append(action(G.thumb, NT.like), action(G.liComment, NT.comment), action(G.xRepost, NT.repost), action(G.plane, NT.send));
  root.append(bar);
  comments.forEach((t) => {
    const row = el("div", "li-comment");
    const bubble = el("div", "li-bubble");
    const name = el("div", "li-cname");
    name.append(el("b", "", c.name), el("span", "li-author", NT.author));
    bubble.append(name, folded(t.text, { cls: "li-ctext", fold: 0, more: NT.more }));
    const col = el("div", "li-ccol");
    col.append(bubble, el("div", "li-cact", `${NT.like} · ${NT.reply}`));
    row.append(avatar(c, 32, "sq"), col);
    root.append(row);
  });
  return root;
}

// ---------- TikTok ----------

function mockTikTok(c) {
  return vertical(c, "tt");
}

// ---------- YouTube ----------

function mockYouTube(c) {
  if (c.short) return vertical(c, "yt");
  const root = el("div", "nw nw-yt");
  const player = el("div", "yt-player");
  if (c.media[0]) {
    const n = media(c.media[0]);
    player.append(n);
    // A vertical video of 3 minutes or less is a Short: switch to the Shorts player once its shape is known.
    if (isVid(c.media[0])) natural(n, (r) => { if (r < 0.8 && root.isConnected) root.replaceWith(vertical({ ...c, short: true }, "yt")); });
    if (!isVid(c.media[0])) player.append(playBadge("yt-big"));
  }
  player.append(el("div", "yt-bar"));
  root.append(player);
  const info = el("div", "yt-info");
  const title = (c.pp.title && c.pp.title.text) || "";
  info.append(el("div", "yt-title", title));
  const views = c.live && c.st.views !== undefined ? `${fCompact.format(c.st.views)} ${NT.views}` : "";
  const meta = el("div", "yt-meta", [views, stamp(c, "long")].filter(Boolean).join(" · ") + " ");
  meta.append(el("b", "", `…${NT.more}`));
  info.append(meta);
  const ch = el("div", "yt-channel");
  const who = el("div", "yt-who");
  who.append(el("b", "", c.name));
  if (c.pp.followers) who.append(el("span", "", fCompact.format(c.pp.followers)));
  ch.append(avatar(c, 36), who, el("span", "yt-subscribe", NT.subscribe));
  info.append(ch);
  const chips = el("div", "yt-chips");
  const like = el("span", "yt-chip yt-like");
  like.append(ic(G.thumb), el("span", "", num(c.st.likes)), el("i"), ic(G.thumbDown));
  const chip = (d, label) => { const x = el("span", "yt-chip"); x.append(ic(d), el("span", "", label)); chips.append(x); };
  chips.append(like);
  chip(G.ytShare, NT.share); chip(G.remix, NT.remix); chip(G.saveList, NT.save); chip(G.download, NT.download);
  info.append(chips);
  const t = c.pp.texts[0];
  if (t && t.text) {
    const desc = el("div", "yt-desc");
    desc.append(el("b", "", [views, stamp(c, "long")].filter(Boolean).join("  ")));
    desc.append(folded(t.text, { cls: "yt-dtext", fold: t.fold || 160, more: NT.more, ell: "…", lines: 3 }));
    info.append(desc);
  }
  root.append(info);
  return root;
}

// ---------- entry point ----------

const MOCKS = { x: mockX, threads: mockThreads, instagram: mockInstagram, facebook: mockFacebook,
  "linkedin-page": mockLinkedIn, tiktok: mockTikTok, youtube: mockYouTube };

// The post drawn like the network shows it, or null when Relay has no mockup for the network.
function netMock(pp, tz, live) {
  const acc = pp.account;
  const fn = acc && MOCKS[acc.provider];
  if (!fn) return null;
  const opts = pp.post.options || {};
  const kind = String(opts.kind || "").toLowerCase();
  const mt = String(opts.media_type || "").toUpperCase();
  const vids = pp.media.filter(isVid).length;
  const c = {
    pp, tz, live, kind,
    name: acc.name || bare(acc.handle) || NET_NAMES[acc.provider],
    handle: acc.handle || acc.name || "",
    av: pp.account_avatar || acc.avatar || "",
    st: (live && pp.stats) || {},
    media: pp.media,
    video: vids > 0 || ["video", "reel", "short"].includes(kind),
    story: mt === "STORIES" || kind === "story",
    reel: mt === "REELS" || kind === "reel" || opts.reel === true || (pp.media.length === 1 && vids === 1 && mt !== "STORIES"),
    short: opts.shorts === true || kind === "short",
  };
  if (acc.provider === "facebook") c.reel = opts.reel === true || kind === "reel";
  const wrap = el("div", "nwrap");
  wrap.append(fn(c));
  const counts = [pp.title, ...pp.texts].filter(Boolean).map((t) => counter(t, live)).filter(Boolean);
  if (counts.length) {
    const row = el("div", "nw-counts");
    row.append(...counts);
    wrap.append(row);
  }
  return wrap;
}
