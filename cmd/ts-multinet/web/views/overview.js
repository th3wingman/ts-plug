// overview.js — summary rows with detail links and enable/disable actions.

import {
  h,
  state,
  confFor,
  domainOf,
  hostnameOf,
  stateBadge,
  dnsDot,
  msgEl,
  enabledButton,
} from "./shared.js";

export function renderOverview(root) {
  root.replaceChildren();

  const st = state.status;
  const cfg = state.config;

  // selected = the effective set (explicit, locked, allow-all, auto rules —
  // the server-side flag); exposed = what is live in the hosts block now.
  // exposed < selected means some selections failed to resolve.
  const peersOf = (name) =>
    state.peers.find((x) => x.name === name)?.peers || [];
  const svcsOf = (name) =>
    state.services.find((x) => x.name === name)?.services || [];
  const nSel = (xs) => xs.filter((x) => x.selected).length;
  const sum = (f) => st.reduce((n, s) => n + f(s), 0);
  const running = st.filter((s) => s.state === "Running").length;
  const peersUp = sum((s) => s.up || 0);
  const peersTotal = sum((s) => s.peers || 0);
  const svcs = sum((s) => svcsOf(s.name).length);
  const peersSel = sum((s) => nSel(peersOf(s.name)));
  const svcsSel = sum((s) => nSel(svcsOf(s.name)));
  const exposed = sum((s) => s.selected || 0);

  root.append(
    h(
      "div",
      { class: "overview__head" },
      h(
        "p",
        { class: "hint" },
        `${st.length} tailnet${st.length === 1 ? "" : "s"} · ${running} running`,
        ` · ${peersUp}/${peersTotal} peers up, ${peersSel} selected`,
        ` · ${svcs} service${svcs === 1 ? "" : "s"}, ${svcsSel} selected`,
        ` · ${exposed} exposed`,
      ),
      h(
        "p",
        { class: "hint" },
        cfg?.dns_listen ? `DNS ${cfg.dns_listen}` : "DNS 127.0.0.1:53",
        cfg?.hosts_file ? ` · hosts ${cfg.hosts_file}` : "",
        " · click a tailnet to manage its peers, services and settings",
      ),
    ),
  );

  if (state.pageMsg) root.append(msgEl(state.pageMsg));

  if (!st.length) {
    root.append(
      h(
        "div",
        { class: "empty" },
        h("p", {}, "no tailnets configured yet"),
        h(
          "a",
          { class: "btn btn--primary", href: "#/config" },
          "add your first tailnet",
        ),
      ),
    );
    return;
  }

  // one table row per tailnet: the numbers get their own aligned columns so
  // tailnets compare at a glance; identity facts (suffix, domain, hostname,
  // node ip) follow. A disabled tailnet has no live numbers — dashes, not
  // rows of zeros.
  const cols = [
    ["tailnet"],
    ["dns"],
    ["peers up", "num"],
    ["peers sel", "num"],
    ["services", "num"],
    ["svc sel", "num"],
    ["exposed", "num"],
    ["rules"],
    ["suffix"],
    ["domain"],
    ["hostname"],
    ["node ip"],
    [""],
  ];
  const tbody = h("tbody");
  for (const s of st) {
    const tc = confFor(s.name);
    const live = s.state === "Running";
    const peers = peersOf(s.name);
    const services = svcsOf(s.name);
    const pSel = nSel(peers);
    const sSel = nSel(services);
    const exp = s.selected ?? 0;
    const short = live && exp < pSel + sSel;
    const rules = (tc.auto_select || []).length;
    const href = `#/tailnet/${encodeURIComponent(s.name)}`;
    const dash = (v) => (live ? String(v) : "—");
    const num = (v, extra = {}) => h("td", { class: "num", ...extra }, v);
    tbody.append(
      h(
        "tr",
        {
          class: "ov-row" + (live ? "" : " is-down"),
          onclick: (e) => {
            if (!e.target.closest("button, a")) location.hash = href;
          },
        },
        h(
          "td",
          {},
          h(
            "span",
            { class: "ov-name" },
            h("a", { class: "row__name", href }, s.name),
            stateBadge(s.state),
          ),
        ),
        h("td", {}, dnsDot(live ? s.dns_registered : undefined)),
        num(live ? `${s.up} / ${s.peers}` : "—"),
        num(dash(pSel)),
        num(dash(services.length)),
        num(dash(sSel)),
        num(
          live ? `${exp} / ${pSel + sSel}` : "—",
          short
            ? {
                class: "num is-short",
                title: `${pSel + sSel - exp} selected but not live — open the tailnet for the errors`,
              }
            : { title: "live in the hosts block / selected" },
        ),
        h(
          "td",
          {},
          [tc.allow_all && "allow-all", rules > 0 && `${rules} auto`]
            .filter(Boolean)
            .join(" · ") || "—",
        ),
        h("td", {}, s.suffix || (tc.enabled === false ? "—" : "(detecting)")),
        h("td", {}, domainOf(tc)),
        h("td", {}, s.hostname || hostnameOf(tc)),
        h("td", {}, s.assigned_ip || "—"),
        h("td", { class: "ov-action" }, enabledButton(s.name)),
      ),
    );
    if (state.msgs[s.name]) {
      tbody.append(
        h(
          "tr",
          { class: "ov-msg" },
          h(
            "td",
            { colspan: "" + cols.length, role: "status" },
            msgEl(state.msgs[s.name]),
          ),
        ),
      );
    }
  }
  root.append(
    h(
      "div",
      { class: "table-wrap" },
      h(
        "table",
        { class: "ov-table" },
        h(
          "thead",
          {},
          h(
            "tr",
            {},
            ...cols.map(([label, cls]) => h("th", cls ? { class: cls } : {}, label)),
          ),
        ),
        tbody,
      ),
    ),
  );
}
