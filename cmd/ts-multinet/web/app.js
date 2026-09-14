// app.js — ts-multinet control panel. Vanilla JS, one page, three views;
// polls the daemon every 5s and renders from /status + /peers + /config.
//
// All API values (peer names, domains, login URLs) cross a trust boundary —
// they can carry attacker-chosen bytes — so DOM is built with h(), never
// innerHTML.

import { get, post, del } from "./api.js";

const POLL_MS = 5000;
const PROBE_PORTS = "22,80,443,8080"; // same defaults as `ts-multinet peers`

const state = {
  status: [],
  peers: [],
  config: null,
  cardMsgs: {}, // tailnet -> {kind, text, link?} (login/domain/allow-all feedback)
  peerErrs: {}, // tailnet -> error text from select/forget
  dashMsg: null, // {kind, text} add/remove feedback (and their needs-restart notices)
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
    // login messages are stale once the tailnet is actually Running
    for (const s of state.status) {
      if (s.state === "Running" && state.cardMsgs[s.name]?.kind === "info") {
        delete state.cardMsgs[s.name];
      }
    }
    $("#offline-banner").hidden = true;
  } catch {
    $("#offline-banner").hidden = false; // keep last data on screen
  }
  render();
}

// confFor returns the config tailnet entry for a name (or a bare default).
function confFor(name) {
  return (
    (state.config?.tailnets || []).find((t) => t.name === name) || { name }
  );
}

// slug mirrors the daemon's slugify: a DNS-safe label from a free-form name.
const slug = (s) =>
  (s || "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63)
    .replace(/-+$/g, "") || "tailnet";

const domainOf = (tc) => tc.domain || slug(tc.name);
const hostnameOf = (tc) => tc.hostname || "ts-multinet-" + slug(tc.name);

// --- actions ------------------------------------------------------------------

async function doLogin(name) {
  state.cardMsgs[name] = { kind: "info", text: "starting login…" };
  render();
  try {
    const res = await post("/login", { tailnet: name });
    if (res.login_url) {
      state.cardMsgs[name] = {
        kind: "info",
        text: "open the login URL — log in once, forever",
        link: res.login_url,
      };
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
    state.cardMsgs[name] = {
      kind: "ok",
      text: on ? "selecting every peer" : "allow-all off",
    };
  } catch (e) {
    state.cardMsgs[name] = { kind: "error", text: e.message };
  }
  refresh();
}

async function setDomain(name, input) {
  try {
    await post(`/tailnet/${encodeURIComponent(name)}/domain`, {
      domain: input.value.trim(),
    });
    state.cardMsgs[name] = {
      kind: "ok",
      text: "domain updated (empty = tailnet name)",
    };
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
    const res = await post("/reload");
    $("#reload-msg").textContent = (res && res.ok) || "reloaded"; // may carry a needs-restart notice
  } catch (e) {
    $("#reload-msg").textContent = e.message;
  }
  setTimeout(() => {
    $("#reload-msg").textContent = "";
  }, 8000);
  refresh();
}

// addTailnet posts the dashboard form. Empty cidr/tun/domain mean "auto".
async function addTailnet() {
  const val = (id) => document.getElementById(id).value.trim();
  const name = val("add-name");
  if (!name) {
    state.dashMsg = { kind: "error", text: "name is required" };
    render();
    return;
  }
  const body = { name };
  const cidr = val("add-cidr");
  const tun = val("add-tun");
  if (cidr || tun) {
    if (!cidr || !tun) {
      state.dashMsg = {
        kind: "error",
        text: "cidr and tun come as a pair — or leave both empty for auto",
      };
      render();
      return;
    }
    body.cidr = cidr;
    body.tun = tun;
  }
  const domain = val("add-domain");
  if (domain) body.domain = domain;
  const hostname = val("add-hostname");
  if (hostname) body.hostname = hostname;
  try {
    const res = await post("/tailnet", body);
    state.dashMsg = {
      kind: "ok",
      text: `${res.ok} — cidr ${res.cidr}, tun ${res.tun}`,
    };
    for (const id of ["add-name", "add-cidr", "add-tun", "add-domain"]) {
      document.getElementById(id).value = "";
    }
  } catch (e) {
    state.dashMsg = { kind: "error", text: e.message };
  }
  refresh();
}

async function removeTailnet(name) {
  if (
    !window.confirm(
      `remove tailnet "${name}"? its node state is kept — re-adding logs back in without a browser`,
    )
  )
    return;
  try {
    const res = await del(`/tailnet/${encodeURIComponent(name)}`);
    state.dashMsg = { kind: "ok", text: res.ok };
  } catch (e) {
    state.dashMsg = { kind: "error", text: e.message };
  }
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
      " — ",
    );
  }
  el.append(m.text);
  return el;
}

// renderAddForm is the dashboard's always-present add panel; it doubles as
// the empty state's "add your first tailnet" call to action.
function renderAddForm(empty) {
  const form = h(
    "form",
    {
      class: "add-form",
      onsubmit: (e) => {
        e.preventDefault();
        addTailnet();
      },
    },
    h("h2", {}, empty ? "add your first tailnet" : "add tailnet"),
    h("input", {
      id: "add-name",
      placeholder: "name",
      required: true,
      spellcheck: "false",
      "aria-label": "tailnet name",
    }),
    h("input", {
      id: "add-cidr",
      placeholder: "cidr (auto)",
      spellcheck: "false",
      "aria-label": "cidr, optional",
    }),
    h("input", {
      id: "add-tun",
      placeholder: "tun (auto)",
      spellcheck: "false",
      "aria-label": "tun, optional",
    }),
    h("input", {
      id: "add-domain",
      placeholder: "domain (auto)",
      spellcheck: "false",
      "aria-label": "custom domain, optional",
    }),
    h("input", {
      id: "add-hostname",
      placeholder: "hostname (auto)",
      spellcheck: "false",
      "aria-label": "node name in the tailnet, optional",
    }),
    h("button", { class: "btn btn--primary", type: "submit" }, "add"),
  );
  if (empty) {
    form.append(
      h(
        "p",
        { class: "hint" },
        "the daemon starts empty — add tailnets here or with `ts-multinet add <name>`,",
        h("br"),
        "then log each one in once (browser URL) and pick the peers you want reachable.",
      ),
    );
  }
  return form;
}

function renderDashboard() {
  if (skipSection("view-dashboard")) return;
  const el = $("#view-dashboard");
  el.replaceChildren();
  el.append(renderAddForm(!state.status.length));
  if (state.dashMsg) {
    el.append(
      h("div", { class: "msg msg--" + state.dashMsg.kind }, state.dashMsg.text),
    );
  }
  if (!state.status.length) {
    el.append(h("p", { class: "empty" }, "no tailnets yet — nothing to show"));
    return;
  }
  for (const s of state.status) {
    const tc = confFor(s.name);
    const actions = h("div", { class: "card__actions" });
    if (s.state !== "Running") {
      actions.append(
        h(
          "button",
          {
            class: "btn btn--primary",
            type: "button",
            "data-login": s.name,
            onclick: () => doLogin(s.name),
          },
          "Login",
        ),
      );
    }
    actions.append(
      h(
        "label",
        { class: "switch" },
        h("input", {
          type: "checkbox",
          "data-allow-all": s.name,
          ...(tc.allow_all ? { checked: true } : {}),
        }),
        " allow all",
      ),
      h(
        "button",
        {
          class: "btn btn--small btn--danger",
          type: "button",
          "data-remove": s.name,
          onclick: () => removeTailnet(s.name),
        },
        "remove",
      ),
    );
    const msg = cardMsg(s.name);
    el.append(
      h(
        "div",
        { class: "card" },
        h(
          "div",
          { class: "card__head" },
          h("span", { class: "card__name" }, s.name),
          h(
            "span",
            { class: "badge " + (STATE_CLASS[s.state] || "badge--err") },
            s.state,
          ),
        ),
        h(
          "dl",
          { class: "card__rows" },
          h("dt", {}, "suffix"),
          h("dd", {}, s.suffix || "(detecting)"),
          h("dt", {}, "domain"),
          h(
            "dd",
            { class: "card__domain" },
            h("input", {
              "data-domain": s.name,
              value: domainOf(tc),
              spellcheck: "false",
              "aria-label": "custom domain",
            }),
            h(
              "button",
              {
                class: "btn btn--small",
                type: "button",
                "data-set-domain": s.name,
                onclick: () => setDomainFromInput(s.name),
              },
              "save",
            ),
          ),
          h("dt", {}, "hostname"),
          h(
            "dd",
            { class: "card__domain" },
            h("input", {
              "data-hostname": s.name,
              value: hostnameOf(tc),
              spellcheck: "false",
              "aria-label": "node hostname in the tailnet",
            }),
            h(
              "button",
              {
                class: "btn btn--small",
                type: "button",
                "data-set-hostname": s.name,
                onclick: () => setHostnameFromInput(s.name),
              },
              "save",
            ),
          ),
          h("dt", {}, "cidr"),
          h("dd", {}, s.cidr),
          h("dt", {}, "node ip"),
          h("dd", {}, s.assigned_ip || "—"),
          h("dt", {}, "peers"),
          h("dd", {}, `${s.up} up / ${s.peers} total, ${s.selected} selected`),
        ),
        actions,
        ...(msg ? [msg] : []),
      ),
    );
  }
}

function setDomainFromInput(name) {
  const input = document.querySelector(
    `input[data-domain="${CSS.escape(name)}"]`,
  );
  if (input) setDomain(name, input);
}

async function setHostname(name, input) {
  try {
    await post(`/tailnet/${encodeURIComponent(name)}/hostname`, {
      hostname: input.value.trim(),
    });
    state.cardMsgs[name] = {
      kind: "ok",
      text: "hostname set — the tailnet restarts to report it (no re-login)",
    };
  } catch (e) {
    state.cardMsgs[name] = { kind: "error", text: e.message };
  }
  refresh();
}

function setHostnameFromInput(name) {
  const input = document.querySelector(
    `input[data-hostname="${CSS.escape(name)}"]`,
  );
  if (input) setHostname(name, input);
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
        ? h("input", {
            type: "checkbox",
            checked: true,
            disabled: true,
            title: "allow-all selects every peer",
          })
        : h("input", {
            type: "checkbox",
            "data-peer": p.name,
            "aria-label": "select " + p.name,
            ...(selected.has(p.name) ? { checked: true } : {}),
            onchange: (e) => togglePeer(tp.name, p.name, e.target.checked),
          });
      const services = (p.services || []).map((s) => ":" + s).join(" ") || "—";
      tbody.append(
        h(
          "tr",
          { class: p.online ? "" : "is-down" },
          h("td", {}, box),
          h("td", {}, p.name),
          h("td", {}, p.ip || "—"),
          h("td", {}, p.os || ""),
          h("td", {}, p.online ? "up" : "down"),
          h("td", {}, services),
        ),
      );
    }
    if (!tbody.children.length) {
      const note = tp.suffix
        ? "no peers"
        : "no peers — still detecting the tailnet suffix";
      tbody.append(
        h("tr", {}, h("td", { colspan: "6", class: "empty" }, note)),
      );
    }
    const section = h(
      "div",
      { class: "peers" },
      h(
        "h2",
        {},
        tp.name,
        h("span", { class: "peers__meta" }, `${tp.up} up · ${tp.suffix || ""}`),
      ),
    );
    if (state.peerErrs[tp.name]) {
      section.append(
        h("div", { class: "msg msg--error" }, state.peerErrs[tp.name]),
      );
    }
    if (tc.allow_all) {
      section.append(
        h("p", { class: "hint" }, "allow-all is on — every peer is selected"),
      );
    }
    section.append(
      h(
        "table",
        {},
        h(
          "thead",
          {},
          h(
            "tr",
            {},
            h("th", {}, ""),
            h("th", {}, "name"),
            h("th", {}, "tailnet ip"),
            h("th", {}, "os"),
            h("th", {}, "state"),
            h("th", {}, "services"),
          ),
        ),
        tbody,
      ),
    );
    el.append(section);
  }
}

function renderConfig() {
  if (skipSection("view-config")) return;
  const el = $("#view-config");
  el.replaceChildren(
    h(
      "p",
      { class: "hint" },
      "read-only view. Every change — selection, domain, add/remove tailnet — applies live from this page or the CLI; manual file edits land via Reload. Only globals (mtu, dns_listen, upstream_dns, state_dir, hosts_file, ui_listen) need ",
      h("code", {}, "sudo systemctl restart ts-multinet"),
      ".",
    ),
    h("pre", {}, state.config ? JSON.stringify(state.config, null, 2) : "—"),
  );
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
  for (const b of $("#tabs").children)
    b.classList.toggle("is-active", b === btn);
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

// clicking Remove on the focused card is fine; the add form handles its own
// submit — nothing else to wire here.

refresh();
setInterval(() => {
  if (!document.hidden) refresh();
}, POLL_MS);
