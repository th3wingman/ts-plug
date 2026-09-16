// config.js — the editing surface for daemon globals and for adding tailnets.
//
// Globals post to /config (comment-preserving patch in the daemon) and report
// which fields need a service restart. state_dir is deliberately not editable:
// moving it would orphan every node's login state.

import { h, state, msgEl, fieldRow, textInput, addTailnet, saveGlobals } from "./shared.js";

// GLOBALS mirrors the daemon's editable subset; the placeholder shows the
// built-in default so an empty field reads as "leave as is".
const GLOBALS = [
  { key: "mtu", label: "mtu", placeholder: "1280", restart: true },
  { key: "dns_listen", label: "dns listen", placeholder: "127.0.0.1:53", restart: true },
  { key: "upstream_dns", label: "upstream dns", placeholder: "1.1.1.1:53", restart: true },
  { key: "ui_listen", label: "ui listen", placeholder: "127.0.0.1:8123", restart: true },
  { key: "hosts_file", label: "hosts file", placeholder: "/etc/hosts", restart: true },
];

export function renderConfig(root) {
  root.replaceChildren();

  if (state.pageMsg) root.append(msgEl(state.pageMsg));

  // --- globals ---
  const inputs = {};
  const rows = GLOBALS.map((g) => {
    const input = textInput(state.config?.[g.key] ?? "", {
      class: "input",
      placeholder: g.placeholder,
    });
    inputs[g.key] = input;
    return fieldRow(
      g.label,
      input,
      null,
      g.restart ? { note: "needs a service restart" } : {},
    );
  });

  root.append(
    h(
      "section",
      { class: "panel" },
      h("h2", {}, "daemon globals"),
      ...rows,
      h(
        "div",
        { class: "field" },
        h("span", { class: "field__label" }, "state dir"),
        h(
          "span",
          { class: "field__value" },
          state.config?.state_dir || "—",
          " ",
          h(
            "span",
            { class: "hint" },
            "— manual edit only (moving it orphans node logins)",
          ),
        ),
      ),
      h(
        "div",
        { class: "field" },
        h("span", { class: "field__label" }, ""),
        h(
          "button",
          {
            class: "btn btn--primary",
            type: "button",
            onclick: () => {
              const values = {};
              for (const g of GLOBALS) {
                const v = inputs[g.key].value;
                // only send what changed from the effective value
                if (v !== (state.config?.[g.key] ?? "")) values[g.key] = v;
              }
              saveGlobals(values);
            },
          },
          "save globals",
        ),
      ),
      h(
        "p",
        { class: "hint" },
        "empty fields keep their current/default value. Changes patch the config file in place (comments survive); fields marked restart-only take effect after ",
        h("code", {}, "sudo systemctl restart ts-multinet"),
        ".",
      ),
    ),
  );

  // --- add tailnet ---
  const addInputs = {};
  const addField = (key, label, placeholder) => {
    const input = textInput("", { class: "input", placeholder });
    addInputs[key] = input;
    return fieldRow(label, input, null);
  };
  root.append(
    h(
      "section",
      { class: "panel" },
      h("h2", {}, "add tailnet"),
      addField("name", "name", "skynet"),
      addField("cidr", "cidr", "198.18.5.0/24 (auto)"),
      addField("tun", "tun", "tsm5 (auto)"),
      addField("domain", "domain", "skynet (auto: slug of name)"),
      addField("hostname", "hostname", "ts-multinet-skynet (auto)"),
      h(
        "div",
        { class: "field" },
        h("span", { class: "field__label" }, ""),
        h(
          "button",
          {
            class: "btn btn--primary",
            type: "button",
            onclick: () => {
              const values = {};
              for (const [k, el] of Object.entries(addInputs)) values[k] = el.value.trim();
              if (!values.name) {
                state.pageMsg = { kind: "error", text: "name is required" };
                renderConfig(root);
                return;
              }
              addTailnet(values);
            },
          },
          "add tailnet",
        ),
      ),
      h(
        "p",
        { class: "hint" },
        "empty cidr/tun are picked from the next free synthetic /24 and tun name; domain and hostname are slugged from the name when omitted. New tailnets boot into NeedsLogin — use the Login button on their row.",
      ),
    ),
  );

  // --- effective config ---
  root.append(
    h(
      "details",
      { class: "panel" },
      h("summary", {}, "effective config (read-only)"),
      h("pre", {}, state.config ? JSON.stringify(state.config, null, 2) : "—"),
    ),
  );
}