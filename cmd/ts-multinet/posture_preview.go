package main

import (
	"context"
	"net/http"
	"time"

	"tailscale.com/posture"
	"tailscale.com/types/logger"
	"tailscale.com/util/syspolicy/policyclient"
)

type postureIdentityJSON struct {
	SerialNumbers []string `json:"serial_numbers"`
	MACAddresses  []string `json:"mac_addresses"`
	SerialError   string   `json:"serial_error,omitempty"`
	MACError      string   `json:"mac_error,omitempty"`
}

type posturePreviewJSON struct {
	postureIdentityJSON
	CollectedAt string `json:"collected_at"`
	Configured  bool   `json:"configured"`
	Applied     *bool  `json:"applied"` // nil: stopped or local preference unavailable
	NodeState   string `json:"node_state"`
	PrefsError  string `json:"prefs_error,omitempty"`
}

// collectPostureIdentity uses the same collectors as the upstream posture
// feature. On Linux the serial collector reads SMBIOS, not system policies.
// Nothing is logged, saved, or sent to the control plane by this preview.
func collectPostureIdentity() postureIdentityJSON {
	var out postureIdentityJSON
	var err error
	out.SerialNumbers, err = posture.GetSerialNumbers(policyclient.NoPolicyClient{}, logger.Discard)
	if err != nil {
		out.SerialError = err.Error()
	}
	out.MACAddresses, err = posture.GetHardwareAddrs()
	if err != nil {
		out.MACError = err.Error()
	}
	return out
}

// handlePosturePreview is deliberately separate from /status and its poll:
// hardware identifiers are collected only when the user requests a preview.
func (d *Daemon) handlePosturePreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out, err := func() (posturePreviewJSON, error) {
		d.cfgMu.Lock()
		defer d.cfgMu.Unlock()
		cfg, err := loadConfig(d.cfgPath)
		if err != nil {
			return posturePreviewJSON{}, err
		}
		tc := confByName(cfg, r.PathValue("name"))
		if tc == nil {
			return posturePreviewJSON{}, clientError{"no such tailnet"}
		}
		out := posturePreviewJSON{Configured: tc.ReportPosture, NodeState: "not started"}
		if !tc.enabled() {
			out.NodeState = "disabled"
		}
		d.mu.Lock()
		tn := d.tailnetByName(tc.Name)
		d.mu.Unlock()
		if tn != nil {
			out.NodeState, _ = tn.status()
			if tn.lc == nil {
				out.PrefsError = "node local client unavailable"
			} else {
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				prefs, err := tn.lc.GetPrefs(ctx)
				if err != nil {
					out.PrefsError = "could not read node posture preference: " + err.Error()
				} else {
					out.Applied = &prefs.PostureChecking
				}
			}
		}
		return out, nil
	}()
	if err != nil {
		if _, ok := err.(clientError); ok {
			writeErr(w, http.StatusNotFound, err.Error())
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	collect := d.postureIdentity
	if collect == nil {
		collect = collectPostureIdentity
	}
	out.postureIdentityJSON = collect()
	out.CollectedAt = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, out)
}
