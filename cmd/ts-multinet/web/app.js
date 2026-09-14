// app.js — ts-multinet control panel. Vanilla JS, one page, three views;
// polls the daemon every 5s and renders from /status + /peers + /config.
//
// All API values (peer names, domains, login URLs) cross a trust boundary —
// they can carry attacker-chosen bytes — so DOM is built with h(), never
// innerHTML.

import { get, post } from "./api.js";

const POLL_MS = 5000;
const PROBE_PORTS = "22,80,443,8080"; // same defaults as `ts-multinet peers`

const state = {
  status: [],
  peers: [],
  config: null,
  cardMsgs: {},  // tailnet -> {kind, text, link?} (login/domain/allow-all feedback)
  peerErrs: {},  // tailnet -> error text from select/forget
};

// --- tiny DOM helpers ---------------------------------------------------------

const $ = (sel, el = document) => el.querySelector(sel);

// h builds an element: children may be strings (text nodes) or nodes; attrs
// are plain attributes ("class" and "on*" are special-cased).
const h = (tag, attrs = {}, ...children) => {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return el;
};

// skipSection reports whether re-rendering would stomp a focused input
// inside it — hold off until the user is done typing.
function skipSection(id) {
  const el = document.getElementById(id);
  const active = document.activeElement;
  return active && el.contains(active) && active.tagName === "INPUT";
}

// --- data ---------------------------------------------------------------------

async function refresh() {
  try {
    const [status, peers, config] = await Promise.all([
      get("/status"),
      get("/peers?ports=" + PROBE_PORTS),
      get("/config"),
    ]);
    state.status = status || [];
    state.peers = peers || [];
    state.config = config;
    $("#offline-banner").hidden = true;
  } catch {
    $("#offline-banner").hidden = false; // keep last data on screen
  }
  render();
}

// confFor returns the config tailnet entry for a name (or a bare default).
function confFor(name) {
  return (state.config?.tailnets || []).find((t) => t.name === name) || { name };
}

const domainOf = (tc) => tc.domain || tc.name;

// --- actions ------------------------------------------------------------------

async function doLogin(name) {
  state.cardMsgs[name] = { kind: "info", text: "starting login…" };
  render();
  try {
    const res = await post("/login", { tailnet: name });
    if (res.login_url) {
      state.cardMsgs[name] = { kind: "info", text: "open the login URL — log in once, forever", link: res.login_url };
    } else if (res.error) {
      state.cardMsgs[name] = { kind: "error", text: res.error };
    } else {
      state.cardMsgs[name] = { kind: "ok", text: "state: " + res.state };
    }
  } catch (e) {
    state.cardMsgs[name] = { kind: "error", text: e.message };
  }
  refresh();
}

async function setAllowAll(name, on) {
  try {
    await post(`/tailnet/${encodeURIComponent(name)}/allow-all`, { on });
    state.cardMsgs[name] = { kind: "ok", text: on ? "selecting every peer" : "allow-all off" };
  } catch (e) {
    state.cardMsgs[name] = { kind: "error", text: e.message };
  }
  refresh();
}

async function setDomain(name, input) {
  try {
    await post(`/tailnet/${encodeURIComponent(name)}/domain`, { domain: input.value.trim() });
    state.cardMsgs[name] = { kind: "ok", text: "domain updated (empty = tailnet name)" };
  } catch (e) {
    state.cardMsgs[name] = { kind: "error", text: e.message };
  }
  refresh();
}

async function togglePeer(name, peer, select) {
  try {
    const path = select ? "select" : "forget";
    await post(`/tailnet/${encodeURIComponent(name)}/${path}`, { peer });
    delete state.peerErrs[name];
  } catch (e) {
    state.peerErrs[name] = e.message;
  }
  refresh();
}

async function doReload() {
  $("#reload-msg").textContent = "reloading…";
  try {
    await post("/reload");
    $("#reload-msg").textContent = "reloaded";
  } catch (e) {
    $("#reload-msg").textContent = e.message;
  }
  setTimeout(() => { $("#reload-msg").textContent = ""; }, 4000);
  refresh();
}

// --- rendering ----------------------------------------------------------------

const STATE_CLASS = {
  Running: "badge--ok",
  NeedsLogin: "badge--warn",
};

function cardMsg(name) {
  const m = state.cardMsgs[name];
  if (!m) return null;
  const el = h("div", { class: "msg msg--" + m.kind });
  if (m.link) {
    el.append(
      h("a", { href: m.link, target: "_blank", rel: "noopener" }, "login URL"),
      " — "
    );
  }
  el.append(m.text);
  return el;
}

function renderDashboard() {
  if (skipSection("view-dashboard")) return;
  const el = $("#view-dashboard");
  el.replaceChildren();
  if (!state.status.length) {
    el.append(h("p", { class: "empty" }, "no tailnets — add one to /etc/ts-multinet/config.json and restart the daemon"));
    return;
  }
  for (const s of state.status) {
    const tc = confFor(s.name);
    const actions = h("div", { class: "card__actions" });
    if (s.state !== "Running") {
      actions.append(h("button", { class: "btn btn--primary", type: "button", "data-login": s.name, onclick: () => doLogin(s.name) }, "Login"));
    } else if (s.login_url) {
      actions.append(h("a", { href: s.login_url, target: "_blank", rel: "noopener" }, "pending login URL"));
    }
    actions.append(
      h("label", { class: "switch" },
        h("input", { type: "checkbox", "data-allow-all": s.name, ...(tc.allow_all ? { checked: true } : {}) }),
        " allow all")
    );
    const msg = cardMsg(s.name);
    el.append(h("div", { class: "card" },
      h("div", { class: "card__head" },
        h("span", { class: "card__name" }, s.name),
        h("span", { class: "badge " + (STATE_CLASS[s.state] || "badge--err") }, s.state)),
      h("dl", { class: "card__rows" },
        h("dt", {}, "suffix"), h("dd", {}, s.suffix || "(detecting)"),
        h("dt", {}, "domain"),
        h("dd", { class: "card__domain" },
          h("input", { "data-domain": s.name, value: domainOf(tc), spellcheck: "false", "aria-label": "custom domain" }),
          h("button", { class: "btn btn--small", type: "button", "data-set-domain": s.name, onclick: () => setDomainFromInput(s.name) }, "save")),
        h("dt", {}, "cidr"), h("dd", {}, s.cidr),
        h("dt", {}, "node ip"), h("dd", {}, s.assigned_ip || "—"),
        h("dt", {}, "peers"), h("dd", {}, `${s.up} up / ${s.peers} total, ${s.selected} selected`)),
      actions,
      ...(msg ? [msg] : [])));
  }
}

function setDomainFromInput(name) {
  const input = document.querySelector(`input[data-domain="${CSS.escape(name)}"]`);
  if (input) setDomain(name, input);
}

function renderPeers() {
  if (skipSection("view-peers")) return;
  const el = $("#view-peers");
  el.replaceChildren();
  if (!state.peers.length) {
    el.append(h("p", { class: "empty" }, "no tailnets"));
    return;
  }
  for (const tp of state.peers) {
    const tc = confFor(tp.name);
    const selected = new Set(tc.resources || []);
    const tbody = h("tbody");
    for (const p of tp.peers || []) {
      const box = tc.allow_all
        ? h("input", { type: "checkbox", checked: true, disabled: true, title: "allow-all selects every peer" })
        : h("input", {
            type: "checkbox", "data-peer": p.name, "aria-label": "select " + p.name,
            ...(selected.has(p.name) ? { checked: true } : {}),
            onchange: (e) => togglePeer(tp.name, p.name, e.target.checked),
          });
      const services = (p.services || []).map((s) => ":" + s).join(" ") || "—";
      tbody.append(h("tr", { class: p.online ? "" : "is-down" },
        h("td", {}, box),
        h("td", {}, p.name),
        h("td", {}, p.ip || "—"),
        h("td", {}, p.os || ""),
        h("td", {}, p.online ? "up" : "down"),
        h("td", {}, services)));
    }
    if (!tbody.children.length) {
      const note = tp.suffix ? "no peers" : "no peers — still detecting the tailnet suffix";
      tbody.append(h("tr", {}, h("td", { colspan: "6", class: "empty" }, note)));
    }
    const section = h("div", { class: "peers" },
      h("h2", {}, tp.name, h("span", { class: "peers__meta" }, `${tp.up} up · ${tp.suffix || ""}`)));
    if (state.peerErrs[tp.name]) {
      section.append(h("div", { class: "msg msg--error" }, state.peerErrs[tp.name]));
    }
    if (tc.allow_all) {
      section.append(h("p", { class: "hint" }, "allow-all is on — every peer is selected"));
    }
    section.append(h("table", {},
      h("thead", {}, h("tr", {},
        h("th", {}, ""), h("th", {}, "name"), h("th", {}, "tailnet ip"),
        h("th", {}, "os"), h("th", {}, "state"), h("th", {}, "services"))),
      tbody));
    el.append(section);
  }
}

function renderConfig() {
  if (skipSection("view-config")) return;
  const el = $("#view-config");
  el.replaceChildren(
    h("p", { class: "hint" },
      "read-only view. selection changes apply instantly from this page; structural changes (add/remove a tailnet, cidr, tun) mean editing ",
      h("code", {}, "/etc/ts-multinet/config.json"),
      " and ",
      h("code", {}, "sudo systemctl restart ts-multinet"),
      "."),
    h("pre", {}, state.config ? JSON.stringify(state.config, null, 2) : "—"));
}

function render() {
  renderDashboard();
  renderPeers();
  renderConfig();
}

// --- wiring -------------------------------------------------------------------

$("#tabs").addEventListener("click", (e) => {
  const btn = e.target.closest("[data-view]");
  if (!btn) return;
  for (const b of $("#tabs").children) b.classList.toggle("is-active", b === btn);
  for (const v of document.querySelectorAll(".view")) {
    v.hidden = v.id !== "view-" + btn.dataset.view;
  }
});

$("#reload-btn").addEventListener("click", doReload);

document.addEventListener("change", (e) => {
  const allow = e.target.closest("[data-allow-all]");
  if (allow) return setAllowAll(allow.dataset.allowAll, allow.checked);
});

document.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && e.target.matches("input[data-domain]")) {
    setDomain(e.target.dataset.domain, e.target);
  }
});

refresh();
setInterval(() => { if (!document.hidden) refresh(); }, POLL_MS);
