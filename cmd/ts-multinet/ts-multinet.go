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
	Name      string   `json:"name"`                // short id, used for state dir
	Suffix    string   `json:"suffix"`              // MagicDNS suffix, e.g. "skynet.ts.net"; auto-detected when empty
	Domain    string   `json:"domain,omitempty"`    // friendly DNS suffix, e.g. "skynet"; default: the tailnet name
	CIDR      string   `json:"cidr"`                // synthetic range, e.g. "198.18.1.0/24"
	TUN       string   `json:"tun"`                 // TUN device name (<=15 chars)
	Hostname  string   `json:"hostname,omitempty"`  // node name in the tailnet; default "ts-multinet-<name>"
	Enabled   *bool    `json:"enabled,omitempty"`   // default true
	AllowAll  bool     `json:"allow_all,omitempty"` // select every non-Mullvad peer instead of listing resources
	Resources []string `json:"resources,omitempty"` // short names to select (hosts entries + synthetic IPs)
	StateDir  string   `json:"state_dir,omitempty"`
}

func (tc TailnetConf) enabled() bool {
	return tc.Enabled == nil || *tc.Enabled
}

// nodeHostname is the name this node reports inside its tailnet.
func (tc TailnetConf) nodeHostname() string {
	return orDefault(tc.Hostname, "ts-multinet-"+slugify(tc.Name))
}

// domainName is the friendly suffix short names resolve under (my-server.skynet):
// the configured domain, else the tailnet name.
func (tc TailnetConf) domainName() string {
	return orDefault(tc.Domain, slugify(tc.Name))
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
	flagJSON := flag.Bool("json", false, "machine-readable output for all commands (errors still go to stderr as text)")
	flagDetails := flag.Bool("details", false, "extended output — applies to the human and json forms both")
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
		cli := cliOpts{sock: *flagSock, json: *flagJSON, details: *flagDetails}
		switch args[0] {
		case "status":
			runStatusClient(cli)
		case "peers":
			filter := ""
			if len(args) >= 2 {
				filter = args[1]
			}
			ports := *flagPorts
			if !*flagProbe {
				ports = ""
			}
			runPeersClient(cli, filter, ports)
		case "services":
			tailnet := ""
			if len(args) >= 2 {
				tailnet = args[1]
			}
			runServicesClient(cli, tailnet)
		case "check":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: ts-multinet check <host[:port]>")
				os.Exit(1)
			}
			runCheckClient(cli, args[1])
		case "login":
			tailnet := ""
			if len(args) >= 2 {
				tailnet = args[1]
			}
			runLoginClient(cli, tailnet)
		case "reload":
			runReloadClient(cli)
		case "select", "forget":
			if len(args) < 3 {
				fmt.Fprintf(os.Stderr, "usage: ts-multinet %s <tailnet> <peer> [peer...]\n", args[0])
				os.Exit(1)
			}
			runSelectForgetClient(cli, args[1], args[0], args[2:])
		case "allow-all":
			tailnet, arg := "", ""
			if len(args) >= 2 {
				tailnet = args[1]
			}
			if len(args) >= 3 {
				arg = args[2]
			}
			runAllowAllClient(cli, tailnet, arg)
		case "domain":
			tailnet, arg := "", ""
			if len(args) >= 2 {
				tailnet = args[1]
			}
			if len(args) >= 3 {
				arg = args[2]
			}
			runDomainClient(cli, tailnet, arg)
		case "hostname":
			tailnet, arg := "", ""
			if len(args) >= 2 {
				tailnet = args[1]
			}
			if len(args) >= 3 {
				arg = args[2]
			}
			runHostnameClient(cli, tailnet, arg)
		case "config":
			runConfigClient(cli)
		case "add":
			if len(args) < 2 || len(args) == 3 || len(args) > 4 {
				fmt.Fprintln(os.Stderr, "usage: ts-multinet add <name> [cidr tun] — cidr/tun optional, as a pair")
				os.Exit(1)
			}
			cidr, tun := "", ""
			if len(args) == 4 {
				cidr, tun = args[2], args[3]
			}
			runAddClient(cli, args[1], cidr, tun)
		case "remove":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: ts-multinet remove <name>")
				os.Exit(1)
			}
			runRemoveClient(cli, args[1])
		default:
			fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
			usage()
			os.Exit(1)
		}
		return
	}

	reg, err := newRegistry(nil) // entries appear as tailnets start (syncTailnets)
	if err != nil {
		slog.Error("registry", "err", err)
		os.Exit(1)
	}

	// Resolve the upstream BEFORE we clobber resolv.conf, so we can inherit
	// whatever the container was already using (e.g. Docker's 127.0.0.11).
	upstream := ensurePort(cfg.UpstreamDNS)
	if upstream == "" && !inContainer() {
		// On resolved hosts /etc/resolv.conf points at the 127.0.0.53 stub —
		// forwarding to it risks a loop whenever resolved routes a general
		// query our way (restart windows, mis-registered links). The runtime
		// file lists the real upstream servers; prefer those.
		upstream = ensurePort(firstNameserver("/run/systemd/resolve/resolv.conf"))
	}
	if upstream == "" {
		upstream = ensurePort(firstNameserver("/etc/resolv.conf"))
	}
	if upstream == "" {
		upstream = "1.1.1.1:53"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ds := &dnsServer{reg: reg, upstream: upstream}
	dnsAddr := orDefault(cfg.DNSListen, "127.0.0.1:53")
	dnsUp := true
	if err := startDNS(dnsAddr, ds); err != nil {
		// In a container resolv.conf points at us — there is no falling back.
		if inContainer() {
			slog.Error("dns", "listen", dnsAddr, "err", err)
			os.Exit(1)
		}
		dnsUp = false
		slog.Warn("dns listen failed: continuing without host DNS; /etc/hosts block still works", "listen", dnsAddr, "err", err)
	} else {
		slog.Info("dns responder up", "listen", dnsAddr, "upstream", upstream)
	}
	rs := newResolvedSync(dnsAddr, dnsUp)

	if *flagSetResolv {
		// search = friendly domains, so `ping host` expands to host.<domain>
		// and resolves against whichever tailnet actually has it.
		names := make([]string, 0, len(cfg.Tailnets))
		for _, tc := range cfg.Tailnets {
			names = append(names, tc.domainName())
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
	// apply-selections callback the moment it reaches Running. Startup is the
	// same sync path the control API uses — a boot-time add and an API add
	// are indistinguishable.
	daemon := newDaemon(nil, reg, *flagConfig, orDefault(cfg.HostsFile, "/etc/hosts"))
	daemon.rt.ctx = ctx
	daemon.rt.mtu = mtu
	daemon.rt.baseDir = base
	daemon.rt.rs = rs
	if err := daemon.syncTailnets(cfg); err != nil {
		slog.Error("tailnet start failed", "err", err)
		cancel()
		os.Exit(1)
	}
	slog.Info("tailnets up", "count", len(daemon.liveTailnets()))

	go daemon.serveControl(ctx, *flagSock)
	go serveUI(ctx, daemon, orDefault(cfg.UIListen, "127.0.0.1:8123"))

	// SIGHUP reloads the config file and syncs tailnets to it.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			slog.Info("reload", "msg", daemon.reload())
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	for _, tn := range daemon.liveTailnets() {
		tn.Close()
	}
	rs.revertAll()
}

func usage() {
	fmt.Fprintf(os.Stderr, `ts-multinet — several tailnets transparently on one host

usage:
  ts-multinet [flags]                 run the daemon (TUNs + DNS + forwarders + control socket)
  ts-multinet [flags] status          show tailnets, states, assigned IPs, selections
  ts-multinet [flags] peers [filter]  list peers and probe their services
  ts-multinet [flags] services [tailnet]  list advertised VIP services (never probed)
  ts-multinet [flags] check <host[:port]>  diagnose one target end-to-end
  ts-multinet [flags] login [tailnet]  start browser login for a tailnet (or list what needs one)
  ts-multinet [flags] reload           re-read the config and apply it live — tailnets included
  ts-multinet [flags] select <tailnet> <peer|svc:label>...   expose peers/services
  ts-multinet [flags] forget <tailnet> <peer|svc:label>...   stop exposing them
  ts-multinet [flags] allow-all <tailnet> [on|off]  select every non-Mullvad peer (default on)
  ts-multinet [flags] domain <tailnet> [name|-]     set (or print) the friendly DNS suffix; - clears it
  ts-multinet [flags] hostname <tailnet> [name|-]   set (or print) the node name in the tailnet; - clears it
  ts-multinet [flags] config                        print the effective config
  ts-multinet [flags] add <name> [cidr tun]         add a tailnet live (cidr/tun auto-picked when omitted)
  ts-multinet [flags] remove <name>                 stop a tailnet and drop it from config (node state kept)

(every verb above talks to the running daemon over its control socket; only a
bare "ts-multinet" invocation runs the daemon itself. Config edits — tailnets
included — apply live via 'reload'; only globals (mtu, dns_listen,
upstream_dns, state_dir, hosts_file, ui_listen) need a service restart.)

output flags (accepted as -json/--json, -details/--details):
  --json     machine-readable output on stdout for every command; errors stay
             on stderr as text and exit codes are unchanged. status/peers/
             check emit the raw server reply (already full detail); mutations
             emit their {ok,...} reply; config emits compact one-line JSON;
             multi-peer select/forget emit one object per line (JSONL).
  --details  extended human output: status gains a per-tailnet block
             (state, hostname, domain, login URL), peers gains the FQDN
             column, check appends raw fields. With --json the server reply
             already carries these fields, so --details is a human-form flag.

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
	// Zero tailnets is valid: the daemon comes up with just the control
	// socket and web UI, and tailnets are added at runtime (startup uses the
	// same sync path). Each entry that does exist must be complete.
	for i, tc := range c.Tailnets {
		if tc.Name == "" || tc.CIDR == "" || tc.TUN == "" {
			return nil, fmt.Errorf("tailnet[%d]: name, cidr, tun are all required (suffix is auto-detected if omitted; login is via `ts-multinet login %s`)", i, tc.Name)
		}
		if !validTailnetName(tc.Name) {
			return nil, fmt.Errorf("tailnet[%d]: invalid name %q (printable, no slashes, 1-63 chars)", i, tc.Name)
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
