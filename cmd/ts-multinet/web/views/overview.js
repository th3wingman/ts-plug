// overview.js — the read-only landing: daemon line + one summary row per
// tailnet. Every row links into the tailnet detail; no actions live here.

import {
  h,
  state,
  confFor,
  domainOf,
  hostnameOf,
  stateBadge,
  dnsDot,
  msgEl,
} from "./shared.js";

export function renderOverview(root) {
  root.replaceChildren();

  const st = state.status;
  const cfg = state.config;

  const svcCount = (name) =>
    state.services.find((x) => x.name === name)?.services?.length || 0;
  const running = st.filter((s) => s.state === "Running").length;
  const peersUp = st.reduce((n, s) => n + (s.up || 0), 0);
  const peersTotal = st.reduce((n, s) => n + (s.peers || 0), 0);
  const svcs = st.reduce((n, s) => n + svcCount(s.name), 0);
  const selected = st.reduce((n, s) => n + (s.selected || 0), 0);

  root.append(
    h(
      "div",
      { class: "overview__head" },
      h(
        "p",
        { class: "hint" },
        `${st.length} tailnet${st.length === 1 ? "" : "s"} · ${running} running`,
        ` · ${peersUp}/${peersTotal} peers up`,
        ` · ${svcs} service${svcs === 1 ? "" : "s"}`,
        ` · ${selected} selected`,
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
    const advertised = svcCount(s.name);
    rows.append(
      h(
        "a",
        { class: "row", href: `#/tailnet/${encodeURIComponent(s.name)}` },
        h(
          "div",
          { class: "row__main" },
          h("span", { class: "row__name" }, s.name),
          stateBadge(s.state),
        ),
        h(
          "div",
          { class: "row__facts" },
          fact("suffix", s.suffix || "(detecting)"),
          fact("domain", domainOf(tc)),
          fact("hostname", s.hostname || hostnameOf(tc)),
          fact("node ip", s.assigned_ip || "—"),
          fact("peers", `${s.up} up / ${s.peers} total`),
          fact("services", String(advertised)),
          fact("selected", String(s.selected ?? 0)),
        ),
        h(
          "div",
          { class: "row__side" },
          h(
            "span",
            { class: "row__dns" },
            dnsDot(s.dns_registered),
            h("span", { class: "hint" }, "dns"),
          ),
          h(
            "span",
            { class: "row__chev", title: `open ${s.name}` },
            "→",
          ),
        ),
      ),
    );
  }
  root.append(rows);
}

function fact(label, value) {
  return h(
    "span",
    { class: "fact" },
    h("span", { class: "fact__label" }, label),
    h("span", { class: "fact__value" }, value),
  );
}
