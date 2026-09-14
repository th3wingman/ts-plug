// api.js — thin same-origin fetch wrapper for the ts-multinet control API.
// The UI is served by the daemon itself, so all paths are relative and the
// browser hits the exact endpoints the CLI does.

export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

async function request(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    throw new ApiError(e.message || "network error", 0);
  }
  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { /* non-JSON error body */ }
  }
  if (!res.ok) {
    throw new ApiError((data && data.error) || res.statusText || "error " + res.status, res.status);
  }
  return data;
}

export const get = (path) => request("GET", path);
export const post = (path, body) => request("POST", path, body || {});
