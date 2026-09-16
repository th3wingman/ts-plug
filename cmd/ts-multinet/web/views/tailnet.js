// tailnet.js — per-tailnet detail: header with the login affordance, then
// two sub-tabs: Peers (filter, cap, click-to-probe, selection) and Settings
// (every editable field, plus the danger zone).
//
// Fields with no API behind them (cidr, tun, enabled) render read-only with
// a pointer to the config file — the daemon has no endpoint for them, and
// inventing one here would be a lie.

import {
  h,
  state,
  confFor,
  domainOf,
  hostnameOf,
  stateBadge,
  dnsDot,
  msgEl,
  fieldRow,
  textInput,
  infoRow,
  skipSection,
  PEER_CAP,
  doLogin,
  setDomain,
  setHostname,
  setCIDR,
  setTUN,
  setAllowAll,
  selectAll,
  setLock,
  testHost,
  togglePeer,
  forgetPeer,
  clearSelections,
  restartTailnet,
  setNativeDNS,
  setAuthKey,
  setEnabled,
  peerCols,
  setPeerCol,
  svcCols,
  setSvcCol,
  probePeers,
  removeTailnet,
} from "./shared.js";

const inConfig = (name) =>
  (state.config?.tailnets || []).some((t) => t.name === name);

// peers-table columns (key → header label); visibility is per-browser
// (peerCols in shared.js), toggled from the toolbar's "columns" menu.
const PEER_COLS = [
  ["name", "name"],
  ["fqdn", "fqdn"],
  ["ip", "tailnet ip"],
  ["os", "os"],
  ["state", "state"],
  ["services", "services"],
];

// services-table columns, same mechanism (svcCols in shared.js).
const SVC_COLS = [
  ["name", "name"],
  ["display", "display"],
  ["vip", "vip"],
  ["ports", "ports"],
];

// --- connectivity -------------------------------------------------------------

// testCell adds a per-row "test" button plus its result span. The probe writes
// only into that span — no shared state — so testing a row never re-renders (or
// scroll-resets) the table the user is reading.
function testCell(cell, host, port) {
  const out = h("span", { class: "test-result" });
  const btn = h(
    "button",
    {
      class: "btn btn--tiny",
      type: "button",
      title: `resolve and dial ${host}:${port}`,
      onclick: async (e) => {
        const b = e.currentTarget;
        b.disabled = true;
        out.className = "test-result";
        out.textContent = "testing…";
        out.title = "";
        try {
          const r = await testHost(host, port);
          out.textContent = checkLabel(r, port);
          out.className = "test-result test-result--" + checkClass(r);
          out.title = r.detail || "";
        } catch (err) {
          out.textContent = "error";
          out.className = "test-result test-result--err";
          out.title = err.message;
        }
        b.disabled = false;
      },
    },
    "test",
  );
  cell.append(btn, out);
}

// lockCell adds the per-row pin toggle: a locked essential survives
// clear-all and blocks disabling the tailnet (jumpboxes, logging, metrics).
// Unlocking releases the pin; unselecting stays a separate, deliberate click.
function lockCell(cell, name, resource, locked) {
  cell.append(
    h(
      "button",
      {
        class: "btn btn--tiny",
        type: "button",
        title: locked
          ? "unlock — releases the pin; the selection stays until forgotten"
          : "lock — pin as an essential: survives clear-all, blocks disabling the tailnet",
        onclick: () => setLock(name, resource, !locked),
      },
      locked ? "🔒 unlock" : "lock",
    ),
  );
}

// checkClass maps a /check result to a colour: open is the only success.
const checkClass = (r) =>
  r.result === "open" ? "ok" : r.result === "unreachable" ? "err" : "warn";

// checkLabel names the port actually dialled, so an unreachable verdict is
// never ambiguous about what was tried.
const checkLabel = (r, port) =>
  r.result === "open"
    ? `open :${port} · ${r.latency_ms}ms`
    : (r.result || "error").replace(/_/g, " ");

// rowHost/rowPort pick what a peer's test dials: its MagicDNS fqdn, on the
// first port `probe ports` found (or a previously-returned service), else 80.
const rowHost = (p, s) => p.fqdn || p.name + "." + (s?.suffix || "");
const rowPort = (name, p) =>
  state.probed[name]?.[p.name]?.[0] ?? p.services?.[0] ?? 80;

// svcHost/svcPort do the same for a service, which is addressed by its
// MagicDNS label rather than its VIP (registry.locate resolves names, not IPs).
const svcHost = (svc, s, tc) =>
  svc.name.replace(/^svc:/i, "") + "." + (s?.suffix || domainOf(tc));
const svcPort = (svc) => {
  const m = String((svc.ports || [])[0] || "").match(/\d+/);
  return m ? Number(m[0]) : 80;
};

// --- health strip -------------------------------------------------------------

const dnsLabel = (reg) =>
  reg === true ? "registered" : reg === false ? "hosts block only" : "unknown";

const fact = (label, value) =>
  h(
    "span",
    { class: "fact" },
    h("span", { class: "fact__label" }, label),
    h("span", { class: "fact__value" }, value),
  );

// statsStrip is the at-a-glance health line above the tabs: what the daemon
// reports about this tailnet, so "it works" is visible instead of assumed.
// `selected` pairs the live hosts-block count with the configured one — a gap
// between them is the drift worth seeing.
function statsStrip(s, tc, name) {
  if (!s) {
    return h(
      "div",
      { class: "stats" },
      h("span", { class: "hint" }, "no live status — the node is not running"),
    );
  }
  const advertised =
    state.services.find((x) => x.name === name)?.services?.length || 0;
  const configured = (tc.resources || []).length;
  const live = s.selected ?? 0;
  // DNS and the hosts block only exist for a running node: report them
  // neutrally otherwise rather than crying wolf about \"not registered\".
  const running = s.state === "Running";
  const reg = running ? s.dns_registered : undefined;
  const stale = running && configured > 0 && live < configured;
  return h(
    "div",
    { class: "stats" },
    fact("dns", h("span", {}, dnsDot(reg), dnsLabel(reg))),
    fact("peers", `${s.up ?? 0}/${s.peers ?? 0} up`),
    fact("services", String(advertised)),
    h(
      "span",
      { class: "fact" + (stale ? " is-stale" : "") },
      h("span", { class: "fact__label" }, "selected"),
      h(
        "span",
        {
          class: "fact__value",
          title: stale
            ? `${configured - live} configured resource(s) are not in the hosts block`
            : "",
        },
        `${live}/${configured}`,
      ),
    ),
  );
}

export function renderTailnet(root, name, tab) {
  if (skipSection("detail-body")) return false; // don't stomp a focused input

  const s = state.status.find((x) => x.name === name) || null;
  const tc = confFor(name);
  const activeTab = tab === "settings" || tab === "services" ? tab : "peers";
  root.replaceChildren();

  if (!s && !inConfig(name)) {
    root.append(
      h(
        "div",
        { class: "empty" },
        h("p", {}, `no tailnet named "${name}"`),
        h("a", { class: "btn", href: "#/" }, "back to overview"),
      ),
    );
    return true;
  }

  root.append(
    h(
      "div",
      { class: "crumb" },
      h("a", { class: "crumb__back", href: "#/" }, "← overview"),
      h("span", { class: "crumb__sep" }, "/"),
      h("span", { class: "crumb__here" }, name),
    ),
  );

  // header: identity + the one action that belongs to a node waiting for
  // login (a disabled tailnet is off on purpose — the Settings toggle
  // re-enables it; a parked one needs a retry, not a login)
  const head = h(
    "div",
    { class: "detail__head" },
    h("span", { class: "detail__name" }, name),
    stateBadge(s?.state),
    h("span", { class: "hint" }, `hostname ${s?.hostname || hostnameOf(tc)}`),
    h("span", { class: "hint" }, `our ip ${s?.assigned_ip || "—"}`),
  );
  if (!s || s.state === "NeedsLogin") {
    head.append(
      h(
        "button",
        {
          class: "btn btn--primary",
          type: "button",
          onclick: () => doLogin(name),
        },
        "Login",
      ),
    );
  }
  root.append(head);

  const msg = msgEl(state.msgs[name]);
  if (msg) root.append(msg);

  root.append(statsStrip(s, tc, name));

  root.append(
    h(
      "nav",
      { class: "subtabs" },
      ...[
        ["peers", "", "Peers"],
        ["services", "/services", "Services"],
        ["settings", "/settings", "Settings"],
      ].map(([key, path, label]) =>
        h(
          "a",
          {
            class: "subtabs__btn" + (activeTab === key ? " is-active" : ""),
            href: `#/tailnet/${encodeURIComponent(name)}${path}`,
          },
          label,
        ),
      ),
    ),
  );

  const body = h("div", { id: "detail-body" });
  root.append(body);
  if (activeTab === "settings") renderSettings(body, name, s, tc);
  else if (activeTab === "services") renderServices(body, name, s, tc);
  else renderPeers(body, name, s, tc);
  return true;
}

// --- peers --------------------------------------------------------------------

function renderPeers(root, name, s, tc) {
  const tp = state.peers.find((p) => p.name === name);
  const peers = tp?.peers || [];
  const locked = new Set(tc.locked || []);
  // locked essentials are always selected — the union also covers a
  // hand-edited config that lists a lock without the matching resource
  const selected = new Set([...(tc.resources || []), ...(tc.locked || [])]);

  const count = h("span", { class: "hint" });
  const tbody = h("tbody");
  const theadRow = h("tr", {}); // rebuilt per render — column visibility can change

  const renderRows = () => {
    const cols = peerCols();
    const span = 1 + PEER_COLS.filter(([k]) => cols[k]).length;
    // read the filter here, not once at render time: the search box updates
    // state and calls renderRows(), so a closed-over value would never see it
    const filter = state.peerFilter[name] || "";
    const matches = (p) =>
      !filter ||
      (p.name || "").toLowerCase().includes(filter) ||
      (p.fqdn || "").toLowerCase().includes(filter);
    theadRow.replaceChildren(
      h("th", {}),
      ...PEER_COLS.filter(([k]) => cols[k]).map(([k, label]) =>
        h("th", k === "fqdn" ? { class: "col-fqdn" } : {}, label),
      ),
    );
    tbody.replaceChildren();
    const hit = peers
      .filter(matches)
      .filter((p) => !state.hideInactive[name] || p.online);
    const shown = state.showAll[name] ? hit : hit.slice(0, PEER_CAP);

    for (const p of shown) {
      const box = tc.allow_all
        ? h("input", {
            type: "checkbox",
            checked: true,
            disabled: true,
            title: "allow-all selects every peer",
          })
        : locked.has(p.name)
          ? h("input", {
              type: "checkbox",
              checked: true,
              disabled: true,
              title: "locked — unlock to unselect",
              "aria-label": "select " + p.name,
            })
          : h("input", {
              type: "checkbox",
              "aria-label": "select " + p.name,
              ...(selected.has(p.name) ? { checked: true } : {}),
              onchange: (e) => togglePeer(name, p.name, e.target.checked),
            });
      const services =
        (state.probed[name]?.[p.name] || p.services || [])
          .map((x) => ":" + x)
          .join(" ") || "—";
      const cell = h("td", {}, box);
      lockCell(cell, name, p.name, locked.has(p.name));
      testCell(cell, rowHost(p, s), rowPort(name, p));
      const row = h("tr", { class: p.online ? "" : "is-down" }, cell);
      if (cols.name) row.append(h("td", {}, p.name || "(unnamed)"));
      if (cols.fqdn) row.append(h("td", { class: "col-fqdn" }, p.fqdn || "—"));
      if (cols.ip) row.append(h("td", {}, p.ip || "—"));
      if (cols.os) row.append(h("td", {}, p.os || ""));
      if (cols.state) row.append(h("td", {}, p.online ? "up" : "down"));
      if (cols.services) row.append(h("td", {}, services));
      tbody.append(row);
    }

    if (!shown.length) {
      const note = tp
        ? tp.suffix
          ? filter
            ? "no peers match the filter"
            : "no peers"
          : "no peers — still detecting the tailnet suffix"
        : "status unavailable";
      tbody.append(
        h("tr", {}, h("td", { colspan: "" + span, class: "empty" }, note)),
      );
    } else if (!state.showAll[name] && hit.length > shown.length) {
      tbody.append(
        h(
          "tr",
          {},
          h(
            "td",
            { colspan: "" + span },
            h(
              "button",
              {
                class: "btn btn--small",
                type: "button",
                onclick: () => {
                  state.showAll[name] = true;
                  renderRows();
                },
              },
              `show all ${hit.length}`,
            ),
          ),
        ),
      );
    }
    const sel = peers.filter((p) => selected.has(p.name)).length;
    count.textContent = `${hit.length} shown · ${sel} selected · ${tp?.up ?? 0} up of ${peers.length}`;
  };

  const search = h("input", {
    class: "search",
    placeholder: "filter by name or fqdn",
    spellcheck: "false",
    value: state.peerFilter[name] || "",
    "aria-label": "filter peers",
    oninput: (e) => {
      state.peerFilter[name] = e.target.value.trim().toLowerCase();
      renderRows(); // rows only — the input keeps focus
    },
  });

  root.append(
    h(
      "div",
      { class: "toolbar" },
      search,
      h(
        "details",
        { class: "cols" },
        h("summary", { class: "btn btn--small" }, "columns"),
        h(
          "div",
          { class: "cols__menu" },
          ...PEER_COLS.map(([k, label]) =>
            h(
              "label",
              { class: "switch" },
              h("input", {
                type: "checkbox",
                ...(peerCols()[k] ? { checked: true } : {}),
                onchange: (e) => {
                  setPeerCol(k, e.target.checked);
                  renderRows(); // rows + header only — the menu stays open
                },
              }),
              " " + label,
            ),
          ),
        ),
      ),
      h(
        "button",
        {
          class: "btn btn--small",
          type: "button",
          onclick: () => probePeers(name),
        },
        "probe ports",
      ),
      h(
        "button",
        {
          class: "btn btn--small",
          type: "button",
          onclick: () => {
            state.showAll[name] = !state.showAll[name];
            renderRows();
          },
        },
        state.showAll[name] ? "cap rows" : "show all",
      ),
      h(
        "button",
        {
          class: "btn btn--small",
          type: "button",
          title: "hide offline peers",
          onclick: (e) => {
            state.hideInactive[name] = !state.hideInactive[name];
            e.target.textContent = state.hideInactive[name]
              ? "show inactive"
              : "hide inactive";
            renderRows(); // rows only — the other controls stay put
          },
        },
        state.hideInactive[name] ? "show inactive" : "hide inactive",
      ),
      h(
        "button",
        {
          class: "btn btn--small",
          type: "button",
          disabled: !peers.length,
          title:
            "select every peer on this tailnet, filter ignored (turns allow-all off)",
          onclick: () => selectAll(name, peers.map((p) => p.name)),
        },
        `select all ${peers.length}`,
      ),
      h(
        "button",
        {
          class: "btn btn--small",
          type: "button",
          title: "unselect every unlocked peer and service on this tailnet (locked essentials are kept)",
          onclick: () => clearSelections(name),
        },
        "clear all",
      ),
      count,
    ),
  );

  if (state.peerErr[name]) {
    root.append(h("div", { class: "msg msg--error" }, state.peerErr[name]));
  }
  if (tc.allow_all) {
    root.append(
      h("p", { class: "hint" }, "allow-all is on — every peer is selected"),
    );
  }
  if (!s || s.state === "NeedsLogin" || s.state === "parked") {
    root.append(
      h(
        "p",
        { class: "hint" },
        "the node is not Running yet — peers appear after login",
      ),
    );
  }

  root.append(
    h(
      "div",
      { class: "table-wrap" },
      h("table", {}, h("thead", {}, theadRow), tbody),
    ),
  );

  renderRows();
}

// --- services ----------------------------------------------------------------

// Advertised VIP services visible to this tailnet. Selection uses the same
// select/forget endpoints as peers (the svc: name is the resource); the Ports
// column is advertised metadata — services are never port-probed.
function renderServices(root, name, s, tc) {
  const tp = state.services.find((x) => x.name === name);
  const services = tp?.services || [];
  const locked = new Set(tc.locked || []);
  // same union as renderPeers: locked essentials count as selected
  const selected = new Set([...(tc.resources || []), ...(tc.locked || [])]);
  const tbody = h("tbody");
  const count = h("span", { class: "hint" });
  const theadRow = h("tr", {}); // rebuilt per render — column visibility can change

  const renderRows = () => {
    const cols = svcCols();
    const span = 1 + SVC_COLS.filter(([k]) => cols[k]).length;
    // fresh read, same reason as renderPeers — the search box calls renderRows
    const filter = state.svcFilter[name] || "";
    const matches = (svc) =>
      !filter ||
      (svc.name || "").toLowerCase().includes(filter) ||
      (svc.display_name || "").toLowerCase().includes(filter);
    theadRow.replaceChildren(
      h("th", {}),
      ...SVC_COLS.filter(([k]) => cols[k]).map(([, label]) =>
        h("th", {}, label),
      ),
    );
    tbody.replaceChildren();
    const hit = services.filter(matches);

    for (const svc of hit) {
      const box = tc.allow_all
        ? h("input", {
            type: "checkbox",
            checked: true,
            disabled: true,
            title: "allow-all selects every peer",
          })
        : locked.has(svc.name)
          ? h("input", {
              type: "checkbox",
              checked: true,
              disabled: true,
              title: "locked — unlock to unselect",
              "aria-label": "select " + svc.name,
            })
          : h("input", {
              type: "checkbox",
              "aria-label": "select " + svc.name,
              ...(selected.has(svc.name) ? { checked: true } : {}),
              onchange: (e) => togglePeer(name, svc.name, e.target.checked),
            });
      const cell = h("td", {}, box);
      lockCell(cell, name, svc.name, locked.has(svc.name));
      testCell(cell, svcHost(svc, s, tc), svcPort(svc));
      const row = h("tr", {}, cell);
      if (cols.name) row.append(h("td", {}, svc.name || "(unnamed)"));
      if (cols.display) row.append(h("td", {}, svc.display_name || "—"));
      if (cols.vip) row.append(h("td", {}, (svc.vips || []).join(", ") || "—"));
      if (cols.ports)
        row.append(h("td", {}, (svc.ports || []).join(" ") || "—"));
      tbody.append(row);
    }

    if (!hit.length) {
      const note = tp
        ? services.length
          ? "no services match the filter"
          : "no advertised services visible — service visibility is ACL-gated on the tailnet"
        : "services unavailable";
      tbody.append(
        h("tr", {}, h("td", { colspan: "" + span, class: "empty" }, note)),
      );
    }
    const sel = services.filter((x) => selected.has(x.name)).length;
    count.textContent = `${hit.length} shown · ${sel} selected`;
  };

  const search = h("input", {
    class: "search",
    placeholder: "filter by name or display name",
    spellcheck: "false",
    value: state.svcFilter[name] || "",
    "aria-label": "filter services",
    oninput: (e) => {
      state.svcFilter[name] = e.target.value.trim().toLowerCase();
      renderRows(); // rows only — the input keeps focus
    },
  });

  root.append(
    h(
      "div",
      { class: "toolbar" },
      search,
      h(
        "details",
        { class: "cols" },
        h("summary", { class: "btn btn--small" }, "columns"),
        h(
          "div",
          { class: "cols__menu" },
          ...SVC_COLS.map(([k, label]) =>
            h(
              "label",
              { class: "switch" },
              h("input", {
                type: "checkbox",
                ...(svcCols()[k] ? { checked: true } : {}),
                onchange: (e) => {
                  setSvcCol(k, e.target.checked);
                  renderRows(); // rows + header only — the menu stays open
                },
              }),
              " " + label,
            ),
          ),
        ),
      ),
      h(
        "button",
        {
          class: "btn btn--small",
          type: "button",
          disabled: !services.length,
          title:
            "select every advertised service on this tailnet, filter ignored (turns allow-all off)",
          onclick: () => selectAll(name, services.map((x) => x.name)),
        },
        `select all ${services.length}`,
      ),
      h(
        "button",
        {
          class: "btn btn--small",
          type: "button",
          title: "unselect every unlocked peer and service on this tailnet (locked essentials are kept)",
          onclick: () => clearSelections(name),
        },
        "clear all",
      ),
      count,
    ),
  );
  root.append(
    h(
      "p",
      { class: "hint" },
      "ports are what the service advertises — services are never port-probed",
    ),
  );

  if (state.peerErr[name]) {
    root.append(h("div", { class: "msg msg--error" }, state.peerErr[name]));
  }
  if (tc.allow_all) {
    root.append(
      h(
        "p",
        { class: "hint" },
        "allow-all selects every peer; services stay on their own checkboxes",
      ),
    );
  }
  if (!s || s.state === "NeedsLogin" || s.state === "parked") {
    root.append(
      h(
        "p",
        { class: "hint" },
        "the node is not Running yet — services appear after login",
      ),
    );
  }

  root.append(
    h(
      "div",
      { class: "table-wrap" },
      h("table", {}, h("thead", {}, theadRow), tbody),
    ),
  );

  renderRows();
}

// --- settings -----------------------------------------------------------------

function renderSettings(root, name, s, tc) {
  const locked = new Set(tc.locked || []);
  const resources = [...new Set([...(tc.resources || []), ...(tc.locked || [])])];

  root.append(
    h("h3", { class: "section" }, "identity"),
    fieldRow(
      "domain",
      textInput(domainOf(tc), { class: "input" }),
      (e) => {
        const input = e.target
          .closest(".field__control")
          .querySelector("input");
        setDomain(name, input.value);
      },
      {
        note: "friendly DNS suffix — my-server.<domain> resolves alongside the MagicDNS name",
      },
    ),
    fieldRow(
      "hostname",
      textInput(s?.hostname || hostnameOf(tc), { class: "input" }),
      (e) => {
        const input = e.target
          .closest(".field__control")
          .querySelector("input");
        setHostname(name, input.value);
      },
      {
        note: "node name in the tailnet; applying it restarts this tailnet — no re-login",
      },
    ),
    fieldRow(
      "auth key",
      textInput("", {
        class: "input",
        type: "password",
        placeholder: "tskey-auth-… — paste to store; never shown again",
      }),
      (e) => {
        const input = e.target
          .closest(".field__control")
          .querySelector("input");
        setAuthKey(name, input.value);
        input.value = "";
      },
      {
        note: "tagged-device enrollment — the node joins without a browser; the key stays server-side",
      },
    ),
    h(
      "div",
      { class: "field" },
      h("span", { class: "field__label" }, "suffix"),
      h(
        "div",
        { class: "field__control" },
        h(
          "span",
          { class: "field__value" },
          s?.suffix || "(detected when Running)",
        ),
        h(
          "label",
          { class: "switch" },
          h("input", {
            type: "checkbox",
            ...(tc.native_dns ? { checked: true } : {}),
            onchange: (e) => setNativeDNS(name, e.target.checked),
          }),
          " native MagicDNS name in hosts block",
        ),
      ),
    ),
    infoRow("node ip", s?.assigned_ip),
  );

  root.append(
    h("h3", { class: "section" }, "selection"),
    h(
      "div",
      { class: "field" },
      h("span", { class: "field__label" }, "allow all"),
      h(
        "label",
        { class: "switch" },
        h("input", {
          type: "checkbox",
          ...(tc.allow_all ? { checked: true } : {}),
          onchange: (e) => setAllowAll(name, e.target.checked),
        }),
        " select every non-Mullvad peer",
      ),
    ),
    h(
      "div",
      { class: "field" },
      h("span", { class: "field__label" }, "resources"),
      resources.length
        ? h(
            "div",
            { class: "chips" },
          ...resources.map((r) =>
            locked.has(r)
              ? h(
                  "span",
                  {
                    class: "chip chip--locked",
                    title: "locked essential — unlock on the Peers or Services tab",
                  },
                  "🔒 ",
                  r,
                )
              : h(
                  "span",
                  { class: "chip" },
                  r,
                  h(
                    "button",
                    {
                      class: "chip__x",
                      type: "button",
                      title: "stop exposing " + r,
                      onclick: () => forgetPeer(name, r),
                    },
                    "×",
                  ),
                ),
          ),
          )
        : h(
            "span",
            { class: "field__value" },
            "none — select peers on the Peers tab",
          ),
    ),
  );

  root.append(
    h("h3", { class: "section" }, "routing"),
    fieldRow(
      "cidr",
      textInput(s?.cidr || tc.cidr, { class: "input" }),
      (e) => {
        const input = e.target
          .closest(".field__control")
          .querySelector("input");
        setCIDR(name, input.value);
      },
      {
        note: "synthetic range inside 198.18.0.0/15, no overlap with other tailnets",
      },
    ),
    fieldRow(
      "tun",
      textInput(tc.tun, { class: "input" }),
      (e) => {
        const input = e.target
          .closest(".field__control")
          .querySelector("input");
        setTUN(name, input.value);
      },
      { note: "TUN device name, 1-15 chars" },
    ),
    h(
      "p",
      { class: "hint" },
      "cidr and tun are structural: applying them stops and restarts this tailnet — node state is kept, so no new login.",
    ),
  );

  root.append(
    h("h3", { class: "section" }, "maintenance"),
    h(
      "div",
      { class: "field" },
      h("span", { class: "field__label" }, "enabled"),
      h(
        "label",
        { class: "switch" },
        h("input", {
          type: "checkbox",
          ...(tc.enabled === false ? {} : { checked: true }),
          onchange: (e) => setEnabled(name, e.target.checked),
        }),
        " off: node stopped, state and login kept",
      ),
    ),
    h(
      "div",
      { class: "field" },
      h("span", { class: "field__label" }, "restart"),
      h(
        "div",
        { class: "field__control" },
        h(
          "button",
          {
            class: "btn",
            type: "button",
            onclick: () => restartTailnet(name),
          },
          "restart tailnet",
        ),
        h(
          "span",
          { class: "hint" },
          "stop and start this node in place — state and login are kept; use it if a network outage left it dark",
        ),
      ),
    ),
  );

  root.append(
    h("h3", { class: "section section--danger" }, "danger zone"),
    h(
      "div",
      { class: "field" },
      h(
        "div",
        { class: "field__control" },
        h(
          "button",
          {
            class: "btn btn--danger",
            type: "button",
            onclick: () => removeTailnet(name),
          },
          "remove tailnet",
        ),
        h(
          "span",
          { class: "hint" },
          "node state (login identity, selection pins) is kept — re-adding logs back in without a browser",
        ),
      ),
    ),
  );
}
