// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Command ts-multinet runs several tailnets transparently on one host at the
// same time. Each tailnet is a stock userspace tsnet node; in front of each we
// run a small gVisor TCP/IP stack on its own TUN device (tun2socks style) and
// re-dial every connection out through that tailnet. A built-in DNS responder
// hands each MagicDNS name a synthetic address from a per-tailnet, RFC 2544
// range (198.18.0.0/15) so the kernel routes it to the right TUN with a plain
// route — no ip-rule, no iptables, no fwmark.
//
// MVP scope: Linux, TCP only, run inside a container netns.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// Config is the on-disk JSON. Auth keys are NOT in here — each tailnet names an
// env var to read its key from, so secrets stay out of the file.
type Config struct {
	MTU         int           `json:"mtu,omitempty"`          // default 1280
	DNSListen   string        `json:"dns_listen,omitempty"`   // default 127.0.0.1:53
	UpstreamDNS string        `json:"upstream_dns,omitempty"` // for non-tailnet names; default: first nameserver in /etc/resolv.conf, else 1.1.1.1
	StateDir    string        `json:"state_dir,omitempty"`    // base dir for per-tailnet tsnet state; default .state
	Tailnets    []TailnetConf `json:"tailnets"`
}

type TailnetConf struct {
	Name       string `json:"name"`        // short id, used for state dir + hostname
	Suffix     string `json:"suffix"`      // MagicDNS suffix, e.g. "skynet.ts.net"
	AuthKeyEnv string `json:"authkey_env"` // env var holding the auth key
	CIDR       string `json:"cidr"`        // synthetic range, e.g. "198.18.1.0/24"
	TUN        string `json:"tun"`         // TUN device name (<=15 chars)
	StateDir   string `json:"state_dir,omitempty"`
}

func main() {
	flagConfig := flag.String("config", "/etc/ts-multinet/config.json", "path to JSON config")
	flagSetResolv := flag.Bool("set-resolv", true, "overwrite /etc/resolv.conf to point at the built-in responder")
	flagLog := flag.String("log", "info", "log level (debug|info|warn|error)")
	flagPorts := flag.String("ports", "22,80,443,8080", "ports to probe in `peers`")
	flagProbe := flag.Bool("probe", true, "probe ports of online peers in `peers`")
	flag.Usage = usage
	flag.Parse()

	setLogLevel(*flagLog)

	cfg, err := loadConfig(*flagConfig)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	// Subcommands: peers [filter] | check <host[:port]>.
	if args := flag.Args(); len(args) >= 1 {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		base := orDefault(cfg.StateDir, ".state")
		switch args[0] {
		case "peers":
			filter := ""
			if len(args) >= 2 {
				filter = args[1]
			}
			runPeers(ctx, cfg, filter, parsePorts(*flagPorts), *flagProbe)
			return
		case "check":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: ts-multinet check <host[:port]>")
				os.Exit(1)
			}
			runCheck(ctx, cfg, args[1], base)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
			usage()
			os.Exit(1)
		}
	}

	reg, err := newRegistry(cfg.Tailnets)
	if err != nil {
		slog.Error("registry", "err", err)
		os.Exit(1)
	}

	// Resolve the upstream BEFORE we clobber resolv.conf, so we can inherit
	// whatever the container was already using (e.g. Docker's 127.0.0.11).
	upstream := ensurePort(cfg.UpstreamDNS)
	if upstream == "" {
		upstream = ensurePort(firstNameserver("/etc/resolv.conf"))
	}
	if upstream == "" {
		upstream = "1.1.1.1:53"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ds := &dnsServer{reg: reg, upstream: upstream}
	if err := startDNS(cfg.DNSListen, ds); err != nil {
		slog.Error("dns", "err", err)
		os.Exit(1)
	}
	slog.Info("dns responder up", "listen", orDefault(cfg.DNSListen, "127.0.0.1:53"), "upstream", upstream)

	if *flagSetResolv {
		if err := os.WriteFile("/etc/resolv.conf", []byte("nameserver 127.0.0.1\n"), 0644); err != nil {
			slog.Warn("could not rewrite /etc/resolv.conf", "err", err)
		}
	}

	mtu := uint32(cfg.MTU)
	if mtu == 0 {
		mtu = 1280
	}
	base := orDefault(cfg.StateDir, ".state")

	var nets []*Tailnet
	for _, tc := range cfg.Tailnets {
		tn, err := startTailnet(ctx, tc, reg, mtu, base)
		if err != nil {
			slog.Error("tailnet start failed", "name", tc.Name, "err", err)
			cancel()
			os.Exit(1)
		}
		nets = append(nets, tn)
	}
	slog.Info("all tailnets up", "count", len(nets))

	<-ctx.Done()
	slog.Info("shutting down")
	for _, tn := range nets {
		tn.Close()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `ts-multinet — several tailnets transparently on one host

usage:
  ts-multinet [flags]                 run the daemon (TUNs + DNS + forwarders)
  ts-multinet [flags] peers [filter]  list peers and probe their services
  ts-multinet [flags] check <host[:port]>  diagnose one target end-to-end

flags:
`)
	flag.PrintDefaults()
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(c.Tailnets) == 0 {
		return nil, fmt.Errorf("config has no tailnets")
	}
	for i, tc := range c.Tailnets {
		if tc.Name == "" || tc.CIDR == "" || tc.TUN == "" || tc.AuthKeyEnv == "" {
			return nil, fmt.Errorf("tailnet[%d]: name, cidr, tun, authkey_env are all required (suffix is auto-detected if omitted)", i)
		}
		if len(tc.TUN) > 15 {
			return nil, fmt.Errorf("tailnet[%d]: tun name %q exceeds 15 chars", i, tc.TUN)
		}
	}
	return &c, nil
}

// firstNameserver returns the first "nameserver X" entry in a resolv.conf-style
// file, or "" if none.
func firstNameserver(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			return f[1]
		}
	}
	return ""
}

// ensurePort appends :53 if addr has no port. Empty in, empty out.
func ensurePort(addr string) string {
	if addr == "" {
		return ""
	}
	if strings.Contains(addr, ":") {
		return addr
	}
	return addr + ":53"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func setLogLevel(level string) {
	switch level {
	case "debug":
		slog.SetLogLoggerLevel(slog.LevelDebug)
	case "warn":
		slog.SetLogLoggerLevel(slog.LevelWarn)
	case "error":
		slog.SetLogLoggerLevel(slog.LevelError)
	default:
		slog.SetLogLoggerLevel(slog.LevelInfo)
	}
}
