package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"tailscale.com/tsnet"
)

// Route maps a local listen address to a tailnet upstream.
// If SNI is set, the listener is shared with other routes on the same Listen
// address and the upstream is selected by the TLS ClientHello SNI.
// If SNI is empty, the listener is a raw TCP forwarder to Upstream;
// only one such route may exist per Listen address.
type Route struct {
	Listen   string `json:"listen"`
	SNI      string `json:"sni,omitempty"`
	Upstream string `json:"upstream"`
}

type Config struct {
	Name      string  `json:"name,omitempty"`
	Domain    string  `json:"domain,omitempty"`
	DNSListen string  `json:"dns_listen,omitempty"`
	LocalIP   string  `json:"local_ip,omitempty"`
	Routes    []Route `json:"routes"`
}

var (
	flagDir        = flag.String("dir", "", "tsnet server directory")
	flagHostname   = flag.String("hostname", "tsrouter", "hostname for the tsnet server")
	flagDebugTSNet = flag.Bool("debug-tsnet", false, "enable tsnet.Server logging")
	flagConfig     = flag.String("config", "", "path to JSON routes config")
	flagInstance   = flag.String("instance", "", "instance directory; sets -config to <dir>/routes.json and -dir to <dir>/state")
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage: ts-router [flags] [subcommand]

subcommands:
  (none)              run proxy and optional DNS responder
  peers               list tailnet peers visible to this tsnet identity
  print-resolved      print systemd-resolved drop-in to stdout
  install-resolved    write systemd-resolved drop-in (requires root)
  uninstall-resolved  remove systemd-resolved drop-in (requires root)

flags:
`)
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	flag.Parse()

	if *flagInstance != "" {
		if *flagConfig != "" {
			fmt.Fprintln(os.Stderr, "cannot use both -instance and -config")
			os.Exit(1)
		}
		if *flagDir != "" {
			fmt.Fprintln(os.Stderr, "cannot use both -instance and -dir")
			os.Exit(1)
		}
		*flagConfig = filepath.Join(*flagInstance, "routes.json")
		*flagDir = filepath.Join(*flagInstance, "state")
	}

	args := flag.Args()

	// peers doesn't need a config file — it just needs tsnet state.
	if len(args) == 1 && args[0] == "peers" {
		runPeers()
		return
	}

	if *flagConfig == "" {
		slog.Error("-config is required")
		os.Exit(1)
	}

	cfg, err := loadConfig(*flagConfig)
	if err != nil {
		slog.Error("failed to load config", slog.Any("error", err))
		os.Exit(1)
	}

	switch {
	case len(args) == 0:
		runProxy(cfg)
	case len(args) == 1 && args[0] == "print-resolved":
		if err := printResolved(cfg, os.Stdout); err != nil {
			slog.Error("print-resolved failed", slog.Any("error", err))
			os.Exit(1)
		}
	case len(args) == 1 && args[0] == "install-resolved":
		if err := installResolved(cfg); err != nil {
			slog.Error("install-resolved failed", slog.Any("error", err))
			os.Exit(1)
		}
	case len(args) == 1 && args[0] == "uninstall-resolved":
		if err := uninstallResolved(cfg); err != nil {
			slog.Error("uninstall-resolved failed", slog.Any("error", err))
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %v\n\n", args)
		usage()
		os.Exit(1)
	}
}

func runProxy(cfg *Config) {
	if *flagDir == "" {
		slog.Error("-dir is required")
		os.Exit(1)
	}

	groups, err := groupRoutes(cfg.Routes)
	if err != nil {
		slog.Error("invalid config", slog.Any("error", err))
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ts := &tsnet.Server{
		Hostname: *flagHostname,
		Dir:      *flagDir,
	}
	if *flagDebugTSNet {
		ts.Logf = func(format string, args ...any) {
			cur := slog.SetLogLoggerLevel(slog.LevelDebug)
			slog.Debug(fmt.Sprintf(format, args...))
			slog.SetLogLoggerLevel(cur)
		}
	}

	st, err := ts.Up(ctx)
	if err != nil {
		slog.Error("tsnet up failed", slog.Any("error", err))
		os.Exit(1)
	}
	slog.Info("tsnet started", slog.String("hostname", *flagHostname), slog.String("status", st.BackendState))

	for listenAddr, group := range groups {
		ln, err := net.Listen("tcp", listenAddr)
		if err != nil {
			slog.Error("listen failed", slog.String("addr", listenAddr), slog.Any("error", err))
			os.Exit(1)
		}

		if len(group) == 1 && group[0].SNI == "" {
			r := group[0]
			slog.Info("tcp route", slog.String("listen", listenAddr), slog.String("upstream", r.Upstream))
			go serveTCP(ctx, ts, ln, r.Upstream, cfg.LocalIP)
			continue
		}

		sniMap := make(map[string]string, len(group))
		for _, r := range group {
			sniMap[r.SNI] = r.Upstream
			slog.Info("sni route", slog.String("listen", listenAddr), slog.String("sni", r.SNI), slog.String("upstream", r.Upstream))
		}
		go serveSNI(ctx, ts, ln, sniMap, cfg.LocalIP)
	}

	if cfg.DNSListen != "" {
		if cfg.Name == "" || cfg.Domain == "" {
			slog.Error("dns_listen set but name or domain is empty")
			os.Exit(1)
		}
		if err := startDNS(ctx, ts, cfg); err != nil {
			slog.Error("dns start failed", slog.Any("error", err))
			os.Exit(1)
		}
	}

	<-ctx.Done()
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
	if len(c.Routes) == 0 {
		return nil, errors.New("config has no routes")
	}
	if c.LocalIP == "" {
		c.LocalIP = "127.0.0.1"
	}
	return &c, nil
}

func groupRoutes(routes []Route) (map[string][]Route, error) {
	g := make(map[string][]Route)
	for _, r := range routes {
		if r.Listen == "" {
			return nil, fmt.Errorf("route missing listen: %+v", r)
		}
		if r.Upstream == "" {
			return nil, fmt.Errorf("route %q missing upstream", r.Listen)
		}
		g[r.Listen] = append(g[r.Listen], r)
	}
	for addr, group := range g {
		var sniCount, tcpCount int
		for _, r := range group {
			if r.SNI == "" {
				tcpCount++
			} else {
				sniCount++
			}
		}
		if sniCount > 0 && tcpCount > 0 {
			return nil, fmt.Errorf("listen %q mixes tcp and sni routes", addr)
		}
		if tcpCount > 1 {
			return nil, fmt.Errorf("listen %q has %d tcp routes; only one allowed", addr, tcpCount)
		}
		seen := make(map[string]bool, len(group))
		for _, r := range group {
			if r.SNI == "" {
				continue
			}
			if seen[r.SNI] {
				return nil, fmt.Errorf("listen %q has duplicate sni %q", addr, r.SNI)
			}
			seen[r.SNI] = true
		}
	}
	return g, nil
}
