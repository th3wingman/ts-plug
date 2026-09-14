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

	"github.com/tailscale/hujson"
)

// Config is the on-disk JSON. Nodes authenticate with a one-time browser login
// and persist state under state_dir — no authkeys, no secrets in the file.
type Config struct {
	MTU         int           `json:"mtu,omitempty"`          // default 1280
	DNSListen   string        `json:"dns_listen,omitempty"`   // default 127.0.0.1:53
	UpstreamDNS string        `json:"upstream_dns,omitempty"` // for non-tailnet names; default: first nameserver in /etc/resolv.conf, else 1.1.1.1
	StateDir    string        `json:"state_dir,omitempty"`    // base dir for per-tailnet tsnet state; default .state
	HostsFile   string        `json:"hosts_file,omitempty"`   // managed-block target; default /etc/hosts
	UIListen    string        `json:"ui_listen,omitempty"`    // web UI listener; default 127.0.0.1:8123
	Tailnets    []TailnetConf `json:"tailnets"`
}

type TailnetConf struct {
	Name      string   `json:"name"`                // short id, used for state dir + hostname
	Suffix    string   `json:"suffix"`              // MagicDNS suffix, e.g. "skynet.ts.net"; auto-detected when empty
	Domain    string   `json:"domain,omitempty"`    // friendly DNS suffix, e.g. "skynet"; default: the tailnet name
	CIDR      string   `json:"cidr"`                // synthetic range, e.g. "198.18.1.0/24"
	TUN       string   `json:"tun"`                 // TUN device name (<=15 chars)
	Enabled   *bool    `json:"enabled,omitempty"`   // default true
	AllowAll  bool     `json:"allow_all,omitempty"` // select every non-Mullvad peer instead of listing resources
	Resources []string `json:"resources,omitempty"` // short names to select (hosts entries + synthetic IPs)
	StateDir  string   `json:"state_dir,omitempty"`
}

func (tc TailnetConf) enabled() bool {
	return tc.Enabled == nil || *tc.Enabled
}

// domainName is the friendly suffix short names resolve under (my-server.skynet):
// the configured domain, else the tailnet name.
func (tc TailnetConf) domainName() string {
	return orDefault(tc.Domain, tc.Name)
}

// inContainer reports whether we're running inside a container netns (Docker
// or Podman), where hijacking resolv.conf is the established behavior. On the
// host, the default is to never touch system DNS — selected resources resolve
// through the /etc/hosts managed block instead.
func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	_, err := os.Stat("/run/.containerenv")
	return err == nil
}

func main() {
	flagConfig := flag.String("config", "/etc/ts-multinet/config.json", "path to JSON config")
	flagSetResolv := flag.Bool("set-resolv", inContainer(), "point /etc/resolv.conf at the built-in responder (default: true in containers, false on the host)")
	flagLog := flag.String("log", "info", "log level (debug|info|warn|error)")
	flagPorts := flag.String("ports", "22,80,443,8080", "ports to probe in `peers`")
	flagProbe := flag.Bool("probe", true, "probe ports of online peers in `peers`")
	flagSock := flag.String("control-sock", "/run/ts-multinet/control.sock", "control socket path (daemon serves it; peers/check/status query it)")
	flag.Usage = usage
	flag.Parse()

	setLogLevel(*flagLog)

	cfg, err := loadConfig(*flagConfig)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	// Subcommands are thin clients that query the running daemon's control
	// socket — they never bring up their own tsnet stacks.
	if args := flag.Args(); len(args) >= 1 {
		switch args[0] {
		case "status":
			runStatusClient(*flagSock)
		case "peers":
			filter := ""
			if len(args) >= 2 {
				filter = args[1]
			}
			ports := *flagPorts
			if !*flagProbe {
				ports = ""
			}
			runPeersClient(*flagSock, filter, ports)
		case "check":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: ts-multinet check <host[:port]>")
				os.Exit(1)
			}
			runCheckClient(*flagSock, args[1])
		case "login":
			tailnet := ""
			if len(args) >= 2 {
				tailnet = args[1]
			}
			runLoginClient(*flagSock, tailnet)
		case "reload":
			runReloadClient(*flagSock)
		case "select", "forget":
			if len(args) < 3 {
				fmt.Fprintf(os.Stderr, "usage: ts-multinet %s <tailnet> <peer> [peer...]\n", args[0])
				os.Exit(1)
			}
			runSelectForgetClient(*flagSock, args[1], args[0], args[2:])
		case "allow-all":
			tailnet, arg := "", ""
			if len(args) >= 2 {
				tailnet = args[1]
			}
			if len(args) >= 3 {
				arg = args[2]
			}
			runAllowAllClient(*flagSock, tailnet, arg)
		case "domain":
			tailnet, arg := "", ""
			if len(args) >= 2 {
				tailnet = args[1]
			}
			if len(args) >= 3 {
				arg = args[2]
			}
			runDomainClient(*flagSock, tailnet, arg)
		case "config":
			runConfigClient(*flagSock)
		default:
			fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
			usage()
			os.Exit(1)
		}
		return
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
		// search = friendly tailnet names, so `ping host` expands to
		// host.<tailnet> and resolves against whichever tailnet actually has it.
		names := make([]string, 0, len(cfg.Tailnets))
		for _, tc := range cfg.Tailnets {
			names = append(names, tc.Name)
		}
		resolv := "nameserver 127.0.0.1\n"
		if len(names) > 0 {
			resolv += "search " + strings.Join(names, " ") + "\n"
		}
		if err := os.WriteFile("/etc/resolv.conf", []byte(resolv), 0644); err != nil {
			slog.Warn("could not rewrite /etc/resolv.conf", "err", err)
		}
	}

	mtu := uint32(cfg.MTU)
	if mtu == 0 {
		mtu = 1280
	}
	base := orDefault(cfg.StateDir, ".state")

	// The daemon exists before the tailnets so each one can fire its
	// apply-selections callback the moment it reaches Running.
	daemon := newDaemon(nil, reg, *flagConfig, orDefault(cfg.HostsFile, "/etc/hosts"))

	var nets []*Tailnet
	for _, tc := range cfg.Tailnets {
		if !tc.enabled() {
			slog.Info("tailnet disabled, skipping", "name", tc.Name)
			continue
		}
		tn, err := startTailnet(ctx, tc, reg, mtu, base, daemon.applySelections)
		if err != nil {
			slog.Error("tailnet start failed", "name", tc.Name, "err", err)
			cancel()
			os.Exit(1)
		}
		nets = append(nets, tn)
	}
	daemon.setTailnets(nets)
	slog.Info("all tailnets up", "count", len(nets))

	go daemon.serveControl(ctx, *flagSock)

	// SIGHUP reloads selection config and rewrites the hosts block.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			daemon.reload()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	for _, tn := range nets {
		tn.Close()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `ts-multinet — several tailnets transparently on one host

usage:
  ts-multinet [flags]                 run the daemon (TUNs + DNS + forwarders + control socket)
  ts-multinet [flags] status          show tailnets, states, assigned IPs, selections
  ts-multinet [flags] peers [filter]  list peers and probe their services
  ts-multinet [flags] check <host[:port]>  diagnose one target end-to-end
  ts-multinet [flags] login [tailnet]  start browser login for a tailnet (or list what needs one)
  ts-multinet [flags] reload           re-read selection config and rewrite the hosts block
  ts-multinet [flags] select <tailnet> <peer>...   expose peers on a tailnet
  ts-multinet [flags] forget <tailnet> <peer>...   stop exposing peers
  ts-multinet [flags] allow-all <tailnet> [on|off]  select every non-Mullvad peer (default on)
  ts-multinet [flags] domain <tailnet> [name|-]     set (or print) the friendly DNS suffix; - clears it
  ts-multinet [flags] config                        print the effective config

(every verb above talks to the running daemon over its control socket; only a
bare "ts-multinet" invocation runs the daemon itself. Structural changes —
adding tailnets, editing cidr/tun — still need a config edit + restart.)

flags:
`)
	flag.PrintDefaults()
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfig(b, path)
}

// parseConfig is loadConfig on bytes (also used to re-validate a patched
// config before it is written).
func parseConfig(b []byte, path string) (*Config, error) {
	// HuJSON: // comments and trailing commas are allowed, so the config can
	// document itself (config.example.jsonc is written that way).
	std, err := hujson.Standardize(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(std, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(c.Tailnets) == 0 {
		return nil, fmt.Errorf("config has no tailnets")
	}
	for i, tc := range c.Tailnets {
		if tc.Name == "" || tc.CIDR == "" || tc.TUN == "" {
			return nil, fmt.Errorf("tailnet[%d]: name, cidr, tun are all required (suffix is auto-detected if omitted; login is via `ts-multinet login %s`)", i, tc.Name)
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
