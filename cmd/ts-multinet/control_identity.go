package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strings"
)

// handleResetIdentity discards only tsnet's identity store, never the directory
// or selection pins. Persist disabled + no enrollment key before touching state
// so a failed reset or daemon crash cannot silently enroll the node again.
func (d *Daemon) handleResetIdentity(w http.ResponseWriter, r *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var req struct {
		Confirm string `json:"confirm"`
	}
	if mediaType != "application/json" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || req.Confirm != r.PathValue("name") {
		writeErr(w, http.StatusBadRequest, "send application/json with {\"confirm\": \"<tailnet name>\"}")
		return
	}

	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	cfg, err := loadConfig(d.cfgPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tc := confByName(cfg, req.Confirm)
	if tc == nil {
		writeErr(w, http.StatusNotFound, "no such tailnet")
		return
	}
	if len(tc.Locked) > 0 {
		writeErr(w, http.StatusConflict, "tailnet has locked selections — unlock them before resetting its identity")
		return
	}

	dir := tc.nodeStateDir(d.rt.baseDir)
	var live *Tailnet
	for _, tn := range d.liveTailnets() {
		if strings.EqualFold(tn.conf.Name, tc.Name) {
			live = tn
			if tn.stateDir != "" {
				configured, cerr := os.Stat(dir)
				current, lerr := os.Stat(tn.stateDir)
				if cerr != nil || lerr != nil || !os.SameFile(configured, current) {
					writeErr(w, http.StatusConflict, "state_dir differs from the running node — restore it or restart the service before resetting")
					return
				}
				dir = tn.stateDir
			}
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "open identity directory: "+err.Error())
		return
	}
	if root != nil {
		defer root.Close()
		info, err := root.Stat(".")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Custom state directories (including symlink aliases) must not let
		// a reset erase another configured or live tailnet's identity.
		for _, other := range cfg.Tailnets {
			if strings.EqualFold(other.Name, tc.Name) {
				continue
			}
			if fi, err := os.Stat(other.nodeStateDir(d.rt.baseDir)); err == nil && os.SameFile(info, fi) {
				writeErr(w, http.StatusConflict, fmt.Sprintf("state directory is shared with %q — fix state_dir before resetting", other.Name))
				return
			}
		}
		for _, tn := range d.liveTailnets() {
			if tn == live || tn.stateDir == "" {
				continue
			}
			if fi, err := os.Stat(tn.stateDir); err == nil && os.SameFile(info, fi) {
				writeErr(w, http.StatusConflict, "state directory is shared with a running tailnet")
				return
			}
		}
		if fi, err := root.Lstat("tailscaled.state"); err != nil && !os.IsNotExist(err) {
			writeErr(w, http.StatusInternalServerError, "inspect identity store: "+err.Error())
			return
		} else if err == nil && !fi.Mode().IsRegular() {
			writeErr(w, http.StatusConflict, "identity store is not a regular file — refusing reset")
			return
		}
	}

	prev, err := cloneConfig(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	off := false
	tc.Enabled = &off
	tc.AuthKey = ""
	if err := patchConfigFile(d.cfgPath, prev, cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "reset not performed: "+err.Error())
		return
	}
	if live != nil {
		d.stopTailnet(live) // Close waits for writers before the store is unlinked
	}
	d.applySelections()
	if root != nil {
		if err := root.Remove("tailscaled.state"); err != nil && !os.IsNotExist(err) {
			writeErr(w, http.StatusInternalServerError, "tailnet disabled and enrollment key cleared, but identity reset failed: "+err.Error()+"; leave disabled and retry")
			return
		}
	}
	ok := "identity reset; saved enrollment key cleared; tailnet disabled — enable it and log in again. Selections and peer pins kept. The old device may still need removal in the Tailscale admin console."
	if err := d.syncTailnets(cfg); err != nil {
		ok += "; " + err.Error()
	}
	writeJSON(w, map[string]string{"ok": ok})
}
