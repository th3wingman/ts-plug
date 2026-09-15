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

  root.append(
    h(
      "div",
      { class: "overview__head" },
      h(
        "p",
        { class: "hint" },
        `${st.length} tailnet${st.length === 1 ? "" : "s"}`,
        cfg?.dns_listen ? ` · DNS ${cfg.dns_listen}` : " · DNS 127.0.0.1:53",
        cfg?.hosts_file ? ` · hosts ${cfg.hosts_file}` : "",
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
          h("span", { class: "row__chev" }, "›"),
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
