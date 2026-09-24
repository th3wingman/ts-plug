// shared.js — helpers, state, and API actions shared by every view.
//
// All API values (peer names, domains, login URLs) cross a trust boundary —
// they can carry attacker-chosen bytes — so DOM is built with h(), never
// innerHTML.

import { get, post, del } from "../api.js";

export const POLL_MS = 5000;
export const PROBE_PORTS = "22,80,443,8080"; // same defaults as `ts-multinet peers`
export const PEER_CAP = 200; // rows rendered before the "show all" toggle

export const $ = (sel, el = document) => el.querySelector(sel);

// h builds an element: children may be strings (text nodes) or nodes; attrs
// are plain attributes ("class" and "on*" are special-cased).
export const h = (tag, attrs = {}, ...children) => {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false)
      el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return el;
};

// skipSection reports whether re-rendering would stomp a focused input
// inside it — hold off until the user is done typing.
export function skipSection(id) {
  const el = document.getElementById(id);
  const active = document.activeElement;
  return active && el && el.contains(active) && active.tagName === "INPUT";
}

// slug mirrors the daemon's slugify: a DNS-safe label from a free-form name.
export const slug = (s) =>
  (s || "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63)
    .replace(/-+$/g, "") || "tailnet";

export const domainOf = (tc) => tc.domain || slug(tc.name);
export const hostnameOf = (tc) => tc.hostname || "ts-multinet-" + slug(tc.name);

// --- state -------------------------------------------------------------------

export const state = {
  status: [], // GET /status
  peers: [], // GET /peers (no ports: probing is click-only)
  services: [], // GET /services (advertised VIP services; never probed)
  applied: null, // GET /applied (config drift → "reload needed")
  config: null, // GET /config
  msgs: {}, // tailnet -> {kind, text, link?, transient?}
  pageMsg: null, // {kind, text} config-page / add-tailnet feedback
  peerErr: {}, // tailnet -> last select/forget error
  probed: {}, // tailnet -> {peerName: [ports]} — click-to-probe results
  posturePreviews: Object.create(null), // click-only snapshots; never localStorage or periodic collection
  peerFilter: {}, // tailnet -> filter text
  svcFilter: {}, // tailnet -> services filter text
  hideInactive: {}, // tailnet -> bool (hide offline peers)
  showAll: {}, // tailnet -> bool (reveal beyond PEER_CAP)
};

export const confFor = (name) =>
  (state.config?.tailnets || []).find((t) => t.name === name) || { name };

// rerender is installed by app.js's router; keeping it as a callback avoids a
// circular import (views -> shared, app -> views).
let rerender = () => {};
export function setRerender(fn) {
  rerender = fn;
}

export const statusFor = (name) =>
  state.status.find((s) => s.name === name) || null;

export function msg(tailnet, m) {
  state.msgs[tailnet] = m;
}

export function errMsg(tailnet, text) {
  state.msgs[tailnet] = { kind: "error", text };
}

// lastSig is the render signature of the last painted view. Polling skips the
// re-render when nothing changed, so the 5s tick no longer tears down (and
// scroll-resets) a table the user is reading.
let lastSig = null;

// renderSig covers everything a view reads: a change in any of these is worth
// repainting for.
const renderSig = () =>
  JSON.stringify([
    state.status,
    state.peers,
    state.services,
    state.applied,
    state.config,
    state.msgs,
    state.probed,
    state.posturePreviews,
    state.peerFilter,
    state.svcFilter,
    state.hideInactive,
    state.showAll,
    state.pageMsg,
    state.peerErr,
  ]);

export async function refresh() {
  try {
    const [status, peers, services, applied, config] = await Promise.all([
      get("/status"),
      get("/peers"), // never ports= here — probing is on demand
      get("/services"),
      get("/applied"),
      get("/config"),
    ]);
    state.status = status || [];
    state.peers = peers || [];
    state.services = services || [];
    state.applied = applied || null;
    state.config = config;
    // transient messages (login flow) go stale once the tailnet is Running;
    // warnings and errors stay until the next action replaces them
    for (const s of state.status) {
      if (s.state === "Running" && state.msgs[s.name]?.transient) {
        delete state.msgs[s.name];
      }
    }
    $("#offline-banner").hidden = true;
  } catch {
    $("#offline-banner").hidden = false; // keep last data on screen
  }
  paintApplied(); // topbar: applied vs. config-file drift
  const sig = renderSig();
  if (sig === lastSig) return; // nothing changed — leave the DOM alone
  // rerender reports whether it painted: a focused input suppresses it, and
  // then the data must stay "unpainted" so the next poll retries.
  if (rerender()) lastSig = sig;
}

// paintApplied renders the topbar indicator: green when the running config
// matches the file, amber when the file changed after the last apply (a
// manual edit the daemon has not re-read — Reload applies it).
function paintApplied() {
  const el = $("#applied");
  if (!el) return;
  const a = state.applied;
  if (!a) {
    el.textContent = "";
    el.className = "topbar__applied";
    el.title = "";
    return;
  }
  if (a.dirty) {
    el.textContent = "config changed — Reload";
    el.className = "topbar__applied is-dirty";
    el.title =
      "the config file was edited after the last apply — click Reload to apply it";
  } else {
    el.textContent = "config applied";
    el.className = "topbar__applied is-ok";
    el.title = a.applied_at
      ? `config file applied at ${a.applied_at}`
      : "config file applied";
  }
}

// --- actions -----------------------------------------------------------------

export async function doLogin(name) {
  state.msgs[name] = { kind: "info", text: "starting login…", transient: true };
  rerender();
  try {
    const res = await post("/login", { tailnet: name });
    if (res.login_url) {
      state.msgs[name] = {
        kind: "info",
        text: "open the login URL — log in once, forever",
        link: res.login_url,
        transient: true,
      };
    } else if (res.error) {
      errMsg(name, res.error);
    } else {
      msg(name, { kind: "ok", text: "state: " + res.state });
    }
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

export async function setAllowAll(name, on) {
  try {
    await post(`/tailnet/${encodeURIComponent(name)}/allow-all`, { on });
    msg(name, {
      kind: "ok",
      text: on ? "selecting every peer" : "allow-all off",
    });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// Auto-select rules are managed like the other selection knobs: one write per
// change; the server's ok text names the rules and what they currently match.
export async function addAutoRule(name, rule) {
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/auto`, {
      resources: [rule],
    });
    msg(name, { kind: "ok", text: res.ok || "rule added" });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

export async function removeAutoRule(name, rule) {
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/auto`, {
      on: false,
      resources: [rule],
    });
    msg(name, { kind: "ok", text: res.ok || "rule removed" });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

export async function setDomain(name, value) {
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/domain`, {
      domain: value.trim(),
    });
    // the server's ok text carries warnings (short domains that look like
    // public TLDs) — prefer it over a canned message
    msg(name, {
      kind: res.ok?.includes("warning") ? "info" : "ok",
      text: res.ok || "domain updated (empty = tailnet name)",
    });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// setCIDR/setTUN are structural: the daemon stops and restarts the tailnet to
// apply them (node state and login survive), so the server's ok text is the
// user-visible contract here.
export async function setCIDR(name, value) {
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/cidr`, {
      cidr: value.trim(),
    });
    msg(name, { kind: "info", text: res.ok || "cidr updated" });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

export async function setTUN(name, value) {
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/tun`, {
      tun: value.trim(),
    });
    msg(name, { kind: "info", text: res.ok || "tun updated" });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

export async function setHostname(name, value) {
  try {
    await post(`/tailnet/${encodeURIComponent(name)}/hostname`, {
      hostname: value.trim(),
    });
    msg(name, {
      kind: "ok",
      text: "hostname set — the tailnet restarts to report it (no re-login)",
    });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// selectAll adds a whole set of resources at once — the tables' "select all".
// One config write instead of a select per row. The daemon clears allow_all,
// so the set becomes explicit and rows stay individually uncheckable.
export async function selectAll(name, resources) {
  if (!resources.length) return;
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/select-all`, {
      resources,
    });
    msg(name, { kind: "ok", text: res.ok || `selected ${resources.length}` });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// setLock pins (or releases) one selection as an essential — locked
// selections survive clear-all and block disabling the tailnet (jumpboxes,
// logging, metrics).
export async function setLock(name, resource, on) {
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/lock`, {
      peer: resource,
      on,
    });
    msg(name, {
      kind: "ok",
      text: res.ok || (on ? `${resource} locked` : `${resource} unlocked`),
    });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// testHost is the on-demand connectivity probe behind each row's "test"
// button: GET /check resolves the name and dials it, the same call
// `ts-multinet check` makes. The caller renders the result inline — nothing
// here touches state, so a probe never re-renders (or scroll-resets) a table.
export async function testHost(host, port) {
  return get(`/check?host=${encodeURIComponent(host)}&port=${port}`);
}

export async function togglePeer(name, peer, select) {
  try {
    await post(
      `/tailnet/${encodeURIComponent(name)}/${select ? "select" : "forget"}`,
      {
        peer,
      },
    );
    delete state.peerErr[name];
  } catch (e) {
    state.peerErr[name] = e.message;
  }
  refresh();
}

export async function forgetPeer(name, peer) {
  await togglePeer(name, peer, false);
}

// clearSelections unselects everything on a tailnet (resources emptied and
// allow-all off) in one write, so "select nothing" is a single action rather
// than a forget per resource.
export async function clearSelections(name) {
  if (
    !window.confirm(
      `clear all selections on "${name}"? unlocked peers and services stop resolving from this host — locked essentials are kept, auto-select rules are removed`,
    )
  )
    return;
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/clear`);
    msg(name, { kind: "ok", text: res.ok || "all selections cleared" });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// restartTailnet stops and starts one tailnet in place (state and login kept) —
// the manual recovery for a node a network outage left dark.
export async function restartTailnet(name) {
  if (
    !window.confirm(
      `restart tailnet "${name}"? it stops and starts this node; state and login are kept`,
    )
  )
    return;
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/restart`);
    msg(name, { kind: "info", text: res.ok || "tailnet restarted" });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

export async function resetIdentity(name) {
  if (!window.confirm(
    `Reset identity for "${name}"? This disconnects the tailnet, deletes its local node identity and clears its saved enrollment key. Selections and peer pins are kept. It stays disabled until you enable it and log in again. The old device is not removed from the Tailscale admin console.`,
  )) return;
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/reset-identity`, {
      confirm: name,
    });
    msg(name, { kind: "info", text: res.ok });
  } catch (e) {
    errMsg(name, e.message);
  }
  await refresh();
}

// setNativeDNS toggles whether the tailnet's native MagicDNS name is also
// listed in the /etc/hosts block (what shell hostname completion reads).
// DNS resolution is unaffected — both spellings resolve either way.
export async function setNativeDNS(name, on) {
  try {
    await post(`/tailnet/${encodeURIComponent(name)}/native-dns`, { on });
    msg(name, {
      kind: "ok",
      text: on
        ? "native MagicDNS name in the hosts block — completion offers both spellings"
        : "friendly name only — completion offers <host>.<domain>",
    });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// setDomainHosts forces the .<domain> suffix on every hosts-block entry for
// this tailnet. Default off: entries are the bare hostname (a name two
// tailnets share is qualified automatically); DNS resolution is unaffected.
export async function setDomainHosts(name, on) {
  try {
    await post(`/tailnet/${encodeURIComponent(name)}/domain-hosts`, { on });
    msg(name, {
      kind: "ok",
      text: on
        ? "host entries now always carry .<domain>"
        : "host entries bare again (a name two tailnets share still gets qualified)",
    });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// setAuthKey stores an auth key for the tailnet — tagged-device enrollment
// with no browser flow (the tag comes from the key). The daemon never sends
// the key back out; GET /config serves it redacted.
export async function setAuthKey(name, key) {
  key = (key || "").trim();
  if (!key) return;
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/authkey`, {
      auth_key: key,
    });
    msg(name, { kind: "ok", text: res.ok || "auth key stored" });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// setEnabled turns a tailnet off/on without unprovisioning: off stops the
// node (state and login kept), on starts it again.
export async function setEnabled(name, on) {
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/enabled`, {
      on,
    });
    msg(name, {
      kind: "ok",
      text: res.ok || (on ? "tailnet enabled" : "tailnet disabled"),
    });
  } catch (e) {
    errMsg(name, e.message);
  }
  await refresh();
}

export async function setReportPosture(name, on) {
  if (on && !window.confirm(
    `Report device posture to "${name}"? This shares this host's hardware serial numbers and, when requested, MAC addresses with that tailnet for Fleet/CrowdStrike matching. Only this node restarts; its login is kept.`,
  )) return false;
  let saved = false;
  try {
    const res = await post(`/tailnet/${encodeURIComponent(name)}/report-posture`, { on });
    delete state.posturePreviews[name];
    msg(name, { kind: "info", text: res.ok });
    saved = true;
  } catch (e) {
    errMsg(name, e.message);
  }
  await refresh();
  return saved;
}

export async function previewPosture(name) {
  const entry = { loading: true };
  state.posturePreviews[name] = entry;
  rerender();
  try {
    entry.data = await get(`/tailnet/${encodeURIComponent(name)}/posture-preview`);
  } catch (e) {
    entry.error = e.message;
  } finally {
    entry.loading = false;
    if (state.posturePreviews[name] === entry) rerender();
  }
}

// Same explicit action on Overview and Settings; enabled defaults to true.
export function enabledButton(name) {
  const enabled = confFor(name).enabled !== false;
  const locked = enabled && (confFor(name).locked || []).length > 0;
  return h("button", {
    class: "btn" + (enabled ? "" : " btn--primary"),
    type: "button",
    disabled: locked,
    "aria-label": `${enabled ? "Disable" : "Enable"} ${name}`,
    title: locked
      ? "Unlock selections before disabling this tailnet"
      : enabled ? "Stop this tailnet; keep its identity and login" : "Start this tailnet",
    onclick: async (e) => {
      const button = e.currentTarget;
      button.disabled = true;
      try {
        await setEnabled(name, !enabled);
      } finally {
        button.disabled = false;
      }
    },
  }, enabled ? "Disable" : "Enable");
}

// Table column visibility is a browser view preference — localStorage, not
// daemon state: each browser picks its own. One store per table.
const readCols = (key, defaults) => {
  try {
    const saved = JSON.parse(localStorage.getItem(key) || "null");
    if (saved && typeof saved === "object") {
      return { ...defaults, ...saved };
    }
  } catch {
    /* unreadable: fall back to defaults */
  }
  return { ...defaults };
};
const writeCols = (key, defaults, col, on) => {
  const cols = readCols(key, defaults);
  cols[col] = on;
  try {
    localStorage.setItem(key, JSON.stringify(cols));
  } catch {
    /* private mode: the preference just won't persist */
  }
};

// peers: fqdn off by default — it is <name>.<suffix>, and the name column
// already shows the short form
const PEERS_COLS_KEY = "tsm.peersCols";
const PEERS_COLS_DEFAULT = {
  name: true,
  fqdn: false,
  ip: true,
  os: true,
  state: true,
  services: true,
  tags: true,
};
export function peerCols() {
  return readCols(PEERS_COLS_KEY, PEERS_COLS_DEFAULT);
}
export function setPeerCol(col, on) {
  writeCols(PEERS_COLS_KEY, PEERS_COLS_DEFAULT, col, on);
}

// services: all columns on by default
const SVC_COLS_KEY = "tsm.svcCols";
const SVC_COLS_DEFAULT = { name: true, display: true, vip: true, ports: true };
export function svcCols() {
  return readCols(SVC_COLS_KEY, SVC_COLS_DEFAULT);
}
export function setSvcCol(col, on) {
  writeCols(SVC_COLS_KEY, SVC_COLS_DEFAULT, col, on);
}

// probePeers fetches one tailnet's peers WITH the port list — the only path
// that probes; results are cached per peer name until the next probe.
export async function probePeers(name) {
  state.msgs[name] = { kind: "info", text: "probing ports…" };
  rerender();
  try {
    const all = await get("/peers?ports=" + PROBE_PORTS);
    const tp = (all || []).find((t) => t.name === name);
    const byPeer = {};
    for (const p of tp?.peers || []) byPeer[p.name] = p.services || [];
    state.probed[name] = byPeer;
    const n = (tp?.peers || []).length;
    msg(name, { kind: "ok", text: `probed ${n} peers` });
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

export async function removeTailnet(name) {
  if (
    !window.confirm(
      `Permanently delete tailnet "${name}"? This stops it and purges its local keys, saved login, selections and settings. Re-adding requires fresh authentication. Use Disable instead to keep them. The old device must be removed separately in the Tailscale admin console.`,
    )
  )
    return;
  try {
    const res = await del(`/tailnet/${encodeURIComponent(name)}`);
    for (const cache of [state.msgs, state.peerErr, state.probed, state.peerFilter,
      state.svcFilter, state.hideInactive, state.showAll, state.posturePreviews]) {
      delete cache[name];
    }
    state.pageMsg = { kind: "ok", text: res.ok };
    location.hash = "#/"; // the detail view's tailnet is gone
  } catch (e) {
    errMsg(name, e.message);
  }
  refresh();
}

// addTailnet posts the config page's form. Empty cidr/tun/domain/hostname
// mean "auto" (the daemon picks cidr/tun, slugs domain/hostname).
export async function addTailnet(values) {
  const body = { name: values.name };
  if (values.cidr || values.tun) {
    if (!values.cidr || !values.tun) {
      state.pageMsg = {
        kind: "error",
        text: "cidr and tun come as a pair — or leave both empty for auto",
      };
      rerender();
      return;
    }
    body.cidr = values.cidr;
    body.tun = values.tun;
  }
  if (values.domain) body.domain = values.domain;
  if (values.hostname) body.hostname = values.hostname;
  try {
    const res = await post("/tailnet", body);
    state.pageMsg = {
      kind: "ok",
      text: `${res.ok} — cidr ${res.cidr}, tun ${res.tun}`,
    };
  } catch (e) {
    state.pageMsg = { kind: "error", text: e.message };
  }
  refresh();
}

// saveGlobals posts the editable global subset; empty fields are omitted
// (leave a field empty to keep its current/default value).
export async function saveGlobals(values) {
  const body = {};
  for (const k of [
    "mtu",
    "dns_listen",
    "upstream_dns",
    "ui_listen",
    "hosts_file",
  ]) {
    const v = (values[k] || "").trim();
    if (!v) continue;
    body[k] = k === "mtu" ? Number(v) : v;
  }
  if (!Object.keys(body).length) {
    state.pageMsg = {
      kind: "error",
      text: "nothing to save — fill at least one field",
    };
    rerender();
    return;
  }
  try {
    const res = await post("/config", body);
    const restart = res.needs_restart
      ? ` — needs restart: ${res.needs_restart}`
      : "";
    state.pageMsg = {
      kind: "info",
      text: (res.ok || "globals saved") + restart,
    };
  } catch (e) {
    state.pageMsg = { kind: "error", text: e.message };
  }
  refresh();
}

export async function reloadDaemon() {
  const el = $("#reload-msg");
  el.textContent = "reloading…";
  try {
    const res = await post("/reload");
    el.textContent = (res && res.ok) || "reloaded"; // may carry a restart notice
  } catch (e) {
    el.textContent = e.message;
  }
  setTimeout(() => {
    el.textContent = "";
  }, 8000);
  refresh();
}

// --- components ---------------------------------------------------------------

const STATE_CLASS = {
  Running: "badge--ok",
  NeedsLogin: "badge--warn",
};

export const stateBadge = (s) =>
  h(
    "span",
    { class: "badge " + (STATE_CLASS[s] || "badge--err") },
    s || "unknown",
  );

// dnsDot: dns_registered is absent until the daemon reports it — unknown
// renders neutral, never a false alarm.
export function dnsDot(reg) {
  const cls =
    reg === true ? "dot--ok" : reg === false ? "dot--err" : "dot--idle";
  const title =
    reg === true
      ? "resolved: routing domains registered"
      : reg === false
        ? "resolved: not registered (hosts block only)"
        : "resolved: not reported";
  return h("span", { class: "dot " + cls, title });
}

export function msgEl(m) {
  if (!m) return null;
  const el = h("div", { class: "msg msg--" + m.kind });
  if (m.link) {
    el.append(
      h("a", { href: m.link, target: "_blank", rel: "noopener" }, "login URL"),
      " — ",
    );
  }
  el.append(m.text);
  return el;
}

export const tailnetMsg = (name) => msgEl(state.msgs[name]);

// fieldRow is a labeled form row: label, input, save button.
export function fieldRow(label, input, onSave, opts = {}) {
  return h(
    "div",
    { class: "field" },
    h("label", { class: "field__label" }, label),
    h(
      "div",
      { class: "field__control" },
      input,
      onSave
        ? h(
            "button",
            { class: "btn btn--small", type: "button", onclick: onSave },
            "save",
          )
        : null,
    ),
    opts.note ? h("p", { class: "hint" }, opts.note) : null,
  );
}

export function textInput(value, attrs = {}) {
  return h("input", {
    value: value ?? "",
    spellcheck: "false",
    ...attrs,
  });
}

export function infoRow(label, value) {
  return h(
    "div",
    { class: "field" },
    h("span", { class: "field__label" }, label),
    h("span", { class: "field__value" }, value || "—"),
  );
}
