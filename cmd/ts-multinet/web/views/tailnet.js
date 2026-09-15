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
  togglePeer,
  forgetPeer,
  clearSelections,
  restartTailnet,
  setNativeDNS,
  setAuthKey,
  probePeers,
  removeTailnet,
} from "./shared.js";

const inConfig = (name) =>
  (state.config?.tailnets || []).some((t) => t.name === name);

export function renderTailnet(root, name, tab) {
  if (skipSection("detail-body")) return; // don't stomp a focused input

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
    return;
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

  // header: identity + the one action that belongs to a not-yet-running node
  const head = h(
    "div",
    { class: "detail__head" },
    h("span", { class: "detail__name" }, name),
    stateBadge(s?.state),
    h("span", { class: "hint" }, `hostname ${s?.hostname || hostnameOf(tc)}`),
    h("span", { class: "hint" }, `our ip ${s?.assigned_ip || "—"}`),
  );
  if (!s || s.state !== "Running") {
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
}

// --- peers --------------------------------------------------------------------

function renderPeers(root, name, s, tc) {
  const tp = state.peers.find((p) => p.name === name);
  const peers = tp?.peers || [];
  const selected = new Set(tc.resources || []);

  const count = h("span", { class: "hint" });
  const tbody = h("tbody");

  const filter = state.peerFilter[name] || "";
  const matches = (p) =>
    !filter ||
    (p.name || "").toLowerCase().includes(filter) ||
    (p.fqdn || "").toLowerCase().includes(filter);

  const renderRows = () => {
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
      tbody.append(
        h(
          "tr",
          { class: p.online ? "" : "is-down" },
          h("td", {}, box),
          h("td", {}, p.name || "(unnamed)"),
          h("td", {}, p.fqdn || "—"),
          h("td", {}, p.ip || "—"),
          h("td", {}, p.os || ""),
          h("td", {}, p.online ? "up" : "down"),
          h("td", {}, services),
        ),
      );
    }

    if (!shown.length) {
      const note = !tp
        ? "status unavailable"
        : tp.suffix
          ? filter
            ? "no peers match the filter"
            : "no peers"
          : "no peers — still detecting the tailnet suffix";
      tbody.append(
        h("tr", {}, h("td", { colspan: "7", class: "empty" }, note)),
      );
    } else if (!state.showAll[name] && hit.length > shown.length) {
      tbody.append(
        h(
          "tr",
          {},
          h(
            "td",
            { colspan: "7" },
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
    count.textContent = `${hit.length} shown · ${tp?.up ?? 0} up of ${peers.length}`;
  };

  const search = h("input", {
    class: "search",
    placeholder: "filter by name or fqdn",
    spellcheck: "false",
    value: filter,
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
          title: "unselect every peer and service on this tailnet",
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
  if (!s || s.state !== "Running") {
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
          h("th", {}, "fqdn"),
          h("th", {}, "tailnet ip"),
          h("th", {}, "os"),
          h("th", {}, "state"),
          h("th", {}, "services"),
        ),
      ),
      tbody,
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
  const selected = new Set(tc.resources || []);
  const tbody = h("tbody");
  const count = h("span", { class: "hint" });

  const filter = state.svcFilter[name] || "";
  const matches = (svc) =>
    !filter ||
    (svc.name || "").toLowerCase().includes(filter) ||
    (svc.display_name || "").toLowerCase().includes(filter);

  const renderRows = () => {
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
        : h("input", {
            type: "checkbox",
            "aria-label": "select " + svc.name,
            ...(selected.has(svc.name) ? { checked: true } : {}),
            onchange: (e) => togglePeer(name, svc.name, e.target.checked),
          });
      tbody.append(
        h(
          "tr",
          {},
          h("td", {}, box),
          h("td", {}, svc.name || "(unnamed)"),
          h("td", {}, svc.display_name || "—"),
          h("td", {}, (svc.vips || []).join(", ") || "—"),
          h("td", {}, (svc.ports || []).join(" ") || "—"),
        ),
      );
    }

    if (!hit.length) {
      const note = !tp
        ? "services unavailable"
        : services.length
          ? "no services match the filter"
          : "no advertised services visible — service visibility is ACL-gated on the tailnet";
      tbody.append(
        h("tr", {}, h("td", { colspan: "5", class: "empty" }, note)),
      );
    }
    const sel = services.filter((x) => selected.has(x.name)).length;
    count.textContent = `${hit.length} shown · ${sel} selected`;
  };

  const search = h("input", {
    class: "search",
    placeholder: "filter by name or display name",
    spellcheck: "false",
    value: filter,
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
        "button",
        {
          class: "btn btn--small",
          type: "button",
          title: "unselect every peer and service on this tailnet",
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
  if (!s || s.state !== "Running") {
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
          h("th", {}, "display"),
          h("th", {}, "vip"),
          h("th", {}, "ports"),
        ),
      ),
      tbody,
    ),
  );

  renderRows();
}

// --- settings -----------------------------------------------------------------

function renderSettings(root, name, s, tc) {
  const resources = tc.resources || [];

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
              h(
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
