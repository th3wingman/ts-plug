package main

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"tailscale.com/tsnet"
)

func listPeers(ctx context.Context, ts *tsnet.Server, w io.Writer) error {
	lc, err := ts.LocalClient()
	if err != nil {
		return err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return err
	}

	rows := make([]peerRow, 0, len(st.Peer)+1)
	if st.Self != nil {
		rows = append(rows, peerRow{
			DNSName: trimDot(st.Self.DNSName),
			IPv4:    pickIP(st.Self.TailscaleIPs, true),
			IPv6:    pickIP(st.Self.TailscaleIPs, false),
			Online:  "self",
		})
	}
	for _, p := range st.Peer {
		rows = append(rows, peerRow{
			DNSName: trimDot(p.DNSName),
			IPv4:    pickIP(p.TailscaleIPs, true),
			IPv6:    pickIP(p.TailscaleIPs, false),
			Online:  yesno(p.Online),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].DNSName < rows[j].DNSName })

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DNS NAME\tIPV4\tIPV6\tONLINE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.DNSName, r.IPv4, r.IPv6, r.Online)
	}
	return tw.Flush()
}

type peerRow struct {
	DNSName string
	IPv4    string
	IPv6    string
	Online  string
}

func pickIP(ips []netip.Addr, wantV4 bool) string {
	for _, a := range ips {
		if a.Is4() == wantV4 {
			return a.String()
		}
	}
	return "-"
}

func trimDot(s string) string {
	return strings.TrimSuffix(s, ".")
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// runPeers is the subcommand entry: brings up tsnet just long enough to
// query the peer list, prints it, exits.
func runPeers() {
	if *flagDir == "" {
		fmt.Fprintln(os.Stderr, "-dir is required")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts := &tsnet.Server{
		Hostname: *flagHostname,
		Dir:      *flagDir,
	}
	defer ts.Close()

	if _, err := ts.Up(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "tsnet up failed: %v\n", err)
		os.Exit(1)
	}

	if err := listPeers(ctx, ts, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "list peers failed: %v\n", err)
		os.Exit(1)
	}
}
