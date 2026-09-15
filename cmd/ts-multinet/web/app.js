// app.js — the UI shell: hash router, 5s polling, and the topbar wiring.
// Views live in views/ (overview, tailnet detail, config) and pull shared
// state/actions from views/shared.js.

import {
  $,
  refresh,
  reloadDaemon,
  setRerender,
  POLL_MS,
} from "./views/shared.js";
import { renderOverview } from "./views/overview.js";
import { renderTailnet } from "./views/tailnet.js";
import { renderConfig } from "./views/config.js";

const app = $("#app");

// parseRoute maps the hash to a view + params. Unknown shapes fall back to
// the overview rather than rendering nothing.
function parseRoute() {
  const raw = (location.hash || "").replace(/^#\/?/, "").replace(/\/+$/, "");
  if (!raw) return { view: "overview" };
  const parts = raw.split("/");
  if (parts[0] === "config") return { view: "config" };
  if (parts[0] === "tailnet" && parts[1]) {
    return {
      view: "tailnet",
      name: decodeURIComponent(parts[1]),
      tab: parts[2] === "settings" ? "settings" : "peers",
    };
  }
  return { view: "overview" };
}

function renderCurrent() {
  const r = parseRoute();
  document.title =
    r.view === "tailnet" ? `${r.name} — ts-multinet` : "ts-multinet";
  if (r.view === "config") renderConfig(app);
  else if (r.view === "tailnet") renderTailnet(app, r.name, r.tab);
  else renderOverview(app);
}

setRerender(renderCurrent);

$("#reload-btn").addEventListener("click", reloadDaemon);
window.addEventListener("hashchange", renderCurrent);

// Enter in a field saves it (the old cards behaved this way) — pick the
// sibling save button of the same field, if it has one.
document.addEventListener("keydown", (e) => {
  if (e.key !== "Enter" || !e.target.matches("input.input")) return;
  const save = e.target.closest(".field")?.querySelector("button");
  if (save) {
    e.preventDefault();
    save.click();
  }
});

// First paint paints before data lands: refresh() re-renders when it does.
renderCurrent();
refresh();
setInterval(() => {
  if (!document.hidden) refresh();
}, POLL_MS);
