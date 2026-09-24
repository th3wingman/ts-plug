package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// purgeTarget validates the entire tree before deletion. In particular, a
// custom state_dir must never turn deleting one tailnet into deleting another
// tailnet, the shared state base, or the daemon's config/hosts file.
func (d *Daemon) purgeTarget(cfg *Config, tc *TailnetConf) (*os.Root, string, error) {
	dir := tc.nodeStateDir(d.rt.baseDir)
	for _, tn := range d.liveTailnets() {
		if strings.EqualFold(tn.conf.Name, tc.Name) && tn.stateDir != "" {
			a, ae := os.Stat(dir)
			b, be := os.Stat(tn.stateDir)
			if ae != nil || be != nil || !os.SameFile(a, b) {
				return nil, "", fmt.Errorf("state_dir differs from the running node — restore it before deleting")
			}
		}
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, "", nil // never started, or a previous purge already finished
	}
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("state_dir must be a directory, not a file or symlink")
	}
	protected := []string{orDefault(d.rt.baseDir, "."), d.cfgPath, d.hostsFile}
	for _, other := range cfg.Tailnets {
		if !strings.EqualFold(other.Name, tc.Name) {
			protected = append(protected, other.nodeStateDir(d.rt.baseDir))
		}
	}
	for _, tn := range d.liveTailnets() {
		if !strings.EqualFold(tn.conf.Name, tc.Name) && tn.stateDir != "" {
			protected = append(protected, tn.stateDir)
		}
	}
	for _, path := range protected {
		// Walk ancestors by inode, not just string prefix: this also catches
		// symlink aliases and not-yet-created child state directories.
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, "", err
		}
		for {
			if real, err := filepath.EvalSymlinks(path); err == nil {
				path = real
			} else if !os.IsNotExist(err) {
				return nil, "", err
			}
			fi, err := os.Stat(path)
			if err != nil && !os.IsNotExist(err) {
				return nil, "", err
			}
			if err == nil && os.SameFile(info, fi) {
				return nil, "", fmt.Errorf("state_dir contains shared or protected data (%s) — refusing purge", path)
			}
			parent := filepath.Dir(path)
			if parent == path {
				break
			}
			path = parent
		}
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, "", err
	}
	parent, err := os.OpenRoot(filepath.Dir(dir))
	return parent, filepath.Base(dir), err
}

// handleRemoveTailnet purges state before forgetting the config entry. Failure
// leaves a disabled, keyless entry available for retry; it never reports a
// successful deletion while leaving reusable credentials behind.
func (d *Daemon) handleRemoveTailnet(w http.ResponseWriter, r *http.Request) {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	cfg, err := loadConfig(d.cfgPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tc := confByName(cfg, r.PathValue("name"))
	if tc == nil {
		writeErr(w, http.StatusNotFound, "no such tailnet")
		return
	}
	if len(tc.Locked) > 0 {
		writeErr(w, http.StatusConflict, "tailnet has locked selections — unlock them before deleting")
		return
	}
	parent, leaf, err := d.purgeTarget(cfg, tc)
	if err != nil {
		writeErr(w, http.StatusConflict, "delete not performed: "+err.Error())
		return
	}
	if parent != nil {
		defer parent.Close()
	}
	prev, err := cloneConfig(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := tc.Name
	off := false
	tc.Enabled, tc.AuthKey = &off, ""
	if err := patchConfigFile(d.cfgPath, prev, cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete not performed: "+err.Error())
		return
	}
	for _, tn := range d.liveTailnets() {
		if strings.EqualFold(tn.conf.Name, name) {
			d.stopTailnet(tn)
		}
	}
	d.applySelections()
	if parent != nil {
		if err := parent.RemoveAll(leaf); err != nil {
			writeErr(w, http.StatusInternalServerError, "tailnet disabled and enrollment key cleared, but state purge failed; config entry kept — retry delete: "+err.Error())
			return
		}
	}
	prev, err = cloneConfig(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg.Tailnets = slices.DeleteFunc(cfg.Tailnets, func(t TailnetConf) bool { return t.Name == name })
	if err := patchConfigFile(d.cfgPath, prev, cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "state purged, but disabled config entry could not be removed — retry delete: "+err.Error())
		return
	}
	ok := "tailnet deleted; local keys, saved login, selections and settings purged. Re-adding requires fresh authentication. The old device must be removed separately in the Tailscale admin console."
	if err := d.syncTailnets(cfg); err != nil {
		ok += "; " + err.Error()
	}
	writeJSON(w, map[string]string{"ok": ok})
}
