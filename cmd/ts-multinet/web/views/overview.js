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

  const rows = h("div", { class: "rows" });
  for (const s of st) {
    const tc = confFor(s.name);
    const peers = peersOf(s.name);
    const services = svcsOf(s.name);
    const pSel = nSel(peers);
    const sSel = nSel(services);
    const exp = s.selected ?? 0;
    const short = s.state === "Running" && exp < pSel + sSel;
    const rules = (tc.auto_select || []).length;
    rows.append(
      h(
        "div",
        { class: "row" },
        h(
          "a",
          { class: "row__link", href: `#/tailnet/${encodeURIComponent(s.name)}` },
          h(
            "div",
            { class: "row__main" },
            h("span", { class: "row__name" }, s.name),
            stateBadge(s.state),
          ),
          h(
            "div",
            { class: "row__facts" },
            fact("suffix", s.suffix || (tc.enabled === false ? "—" : "(detecting)")),
            fact("domain", domainOf(tc)),
            fact("hostname", s.hostname || hostnameOf(tc)),
            fact("node ip", s.assigned_ip || "—"),
            fact("peers", `${s.up} up / ${s.peers} total · ${pSel} selected`),
            fact("services", `${services.length} · ${sSel} selected`),
            fact(
              "exposed",
              `${exp} / ${pSel + sSel}`,
              short
                ? {
                    cls: "is-short",
                    title: `${pSel + sSel - exp} selected but not live — open the tailnet for the errors`,
                  }
                : { title: "live in the hosts block / selected" },
            ),
            tc.allow_all && fact("allow-all", "on"),
            rules > 0 && fact("auto", `${rules} rule${rules === 1 ? "" : "s"}`),
          ),
          h(
            "div",
            { class: "row__side" },
            h(
              "span",
              { class: "row__dns" },
              dnsDot(s.state === "Running" ? s.dns_registered : undefined),
              h("span", { class: "hint" }, "dns"),
            ),
            h("span", { class: "row__chev", title: `open ${s.name}` }, "→"),
          ),
        ),
        enabledButton(s.name),
        state.msgs[s.name] && h(
          "div",
          { class: "row__message", role: "status" },
          msgEl(state.msgs[s.name]),
        ),
      ),
    );
  }
  root.append(rows);
}

function fact(label, value, { cls, title } = {}) {
  return h(
    "span",
    { class: "fact" + (cls ? " " + cls : ""), ...(title ? { title } : {}) },
    h("span", { class: "fact__label" }, label),
    h("span", { class: "fact__value" }, value),
  );
}
