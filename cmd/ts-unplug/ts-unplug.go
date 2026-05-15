package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"tailscale.com/tsnet"
)

var (
	flagDir         = flag.String("dir", "", "tsnet server directory")
	flagHostname    = flag.String("hostname", "tsunplug", "hostname for the tsnet server")
	flagDebugTSNet  = flag.Bool("debug-tsnet", false, "enable tsnet.Server logging")
	flagPort        = flag.Int("port", 80, "local port to listen on")
	flagMode        = flag.String("mode", "http", "proxy mode: http (L7 HTTP reverse proxy) or tcp (raw passthrough)")
	flagTLS         = flag.Bool("tls", false, "upstream speaks HTTPS (http mode only)")
	flagTLSInsecure = flag.Bool("tls-insecure", false, "skip upstream TLS certificate verification (http mode only)")
)

func main() {

	flag.Parse()
	if *flagDir == "" {
		slog.Error("dir is required")
		os.Exit(1)
	}

	args := flag.Args()
	if len(args) < 1 {
		slog.Error("remote-addr is required as first positional argument")
		os.Exit(1)
	}

	remoteAddr := args[0]

	// Ensure remoteAddr has a port, default to 80 if not specified
	if _, _, err := net.SplitHostPort(remoteAddr); err != nil {
		remoteAddr = net.JoinHostPort(remoteAddr, "80")
	}

	if *flagMode != "http" && (*flagTLS || *flagTLSInsecure) {
		slog.Warn("-tls/-tls-insecure ignored outside http mode", slog.String("mode", *flagMode))
	}

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	ts := &tsnet.Server{
		Hostname: *flagHostname,
		Dir:      *flagDir,
	}

	if *flagDebugTSNet {
		ts.Logf = func(format string, args ...any) {
			cur := slog.SetLogLoggerLevel(slog.LevelDebug) // force debug if this option is on
			slog.Debug(fmt.Sprintf(format, args...))
			slog.SetLogLoggerLevel(cur)
		}
	}

	st, err := ts.Up(ctx)
	if err != nil {
		slog.Error("error starting tsnet server", slog.Any("error", err))
		cancelCtx()
		os.Exit(1)
	}

	slog.Info("tsnet server started", slog.String("status", st.BackendState))

	listenAddr := fmt.Sprintf("localhost:%d", *flagPort)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		slog.Error("failed to listen", slog.String("addr", listenAddr), slog.Any("error", err))
		os.Exit(1)
	}
	defer listener.Close()

	switch *flagMode {
	case "http":
		serveHTTP(ctx, ts, listener, remoteAddr)
	case "tcp":
		serveTCP(ctx, ts, listener, remoteAddr)
	default:
		slog.Error("unknown mode", slog.String("mode", *flagMode))
		os.Exit(1)
	}
}

func serveHTTP(ctx context.Context, ts *tsnet.Server, listener net.Listener, remoteAddr string) {
	scheme := "http"
	if *flagTLS {
		scheme = "https"
	}
	target, err := url.Parse(scheme + "://" + remoteAddr)
	if err != nil {
		slog.Error("invalid remote address", slog.Any("error", err))
		os.Exit(1)
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return ts.Dial(ctx, network, remoteAddr)
		},
	}
	if *flagTLS && *flagTLSInsecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport

	slog.Info("HTTP proxy listening", slog.String("local", listener.Addr().String()), slog.String("remote", remoteAddr))

	if err := http.Serve(listener, proxy); err != nil {
		log.Fatal(err)
	}
}

func serveTCP(ctx context.Context, ts *tsnet.Server, listener net.Listener, remoteAddr string) {
	slog.Info("TCP proxy listening", slog.String("local", listener.Addr().String()), slog.String("remote", remoteAddr))
	for {
		c, err := listener.Accept()
		if err != nil {
			slog.Error("accept failed", slog.Any("error", err))
			return
		}
		go pipeTCP(ctx, ts, c, remoteAddr)
	}
}

func pipeTCP(ctx context.Context, ts *tsnet.Server, client net.Conn, remoteAddr string) {
	defer client.Close()
	up, err := ts.Dial(ctx, "tcp", remoteAddr)
	if err != nil {
		slog.Error("dial upstream failed", slog.String("remote", remoteAddr), slog.Any("error", err))
		return
	}
	defer up.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(up, client); done <- struct{}{} }()
	go func() { io.Copy(client, up); done <- struct{}{} }()
	<-done
}
