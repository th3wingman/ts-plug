// Config patching: the daemon owns the config file, but humans edit it too,
// so mutations go through the hujson AST — comments and manual formatting
// survive every write. The only mutable keys are the per-tailnet domain,
// allow_all, native_dns, and resources; tailnet structure (name/cidr/tun) is
// static and still requires a daemon restart.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/tailscale/hujson"
)

// patchConfigFile rewrites path so parsing it yields next, editing the hujson
// AST in place. prev is the same file parsed before the change; the diff
// decides which members to set, insert, or remove. Structural changes are
// supported: added tailnets append (bare members, no comments), removed ones
// delete, and a cidr/tun change rewrites the element. Fails without writing
// if the patched bytes don't re-parse to exactly next — a rename therefore
// lands as remove+add, which is the same net effect on a running daemon.
func patchConfigFile(path string, prev, next *Config) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	v, err := hujson.Parse(b)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	root, ok := v.Value.(*hujson.Object)
	if !ok {
		return fmt.Errorf("parse %s: not a JSON object", path)
	}
	patchGlobals(root, prev, next)
	arr, err := tailnetsArray(root)
	if err != nil {
		return err
	}

	prevBy := make(map[string]*TailnetConf, len(prev.Tailnets))
	for i := range prev.Tailnets {
		prevBy[prev.Tailnets[i].Name] = &prev.Tailnets[i]
	}
	nextView := make(map[string]*TailnetConf, len(next.Tailnets))
	for i := range next.Tailnets {
		nextView[next.Tailnets[i].Name] = &next.Tailnets[i]
	}
	for _, o := range prev.Tailnets {
		if nextView[o.Name] == nil {
			removeTailnetElement(arr, o.Name)
		}
	}
	for i := range next.Tailnets {
		n := &next.Tailnets[i]
		o := prevBy[n.Name]
		switch {
		case o == nil:
			insertTailnetElement(arr, *n)
		case o.CIDR != n.CIDR || o.TUN != n.TUN || o.Suffix != n.Suffix ||
			(o.Enabled == nil) != (n.Enabled == nil) || o.Enabled != nil && n.Enabled != nil && *o.Enabled != *n.Enabled ||
			o.StateDir != n.StateDir:
			// Anything structural rewrites the whole element; only the three
			// mutable keys below are worth preserving comments on.
			removeTailnetElement(arr, n.Name)
			insertTailnetElement(arr, *n)
		default:
			obj, err := tailnetObject(arr, n.Name)
			if err != nil {
				return err
			}
			if o.Domain != n.Domain {
				if n.Domain == "" {
					removeMember(obj, "domain")
				} else {
					setMember(obj, "domain", hujson.String(n.Domain))
				}
			}
			if o.Hostname != n.Hostname {
				if n.Hostname == "" {
					removeMember(obj, "hostname")
				} else {
					setMember(obj, "hostname", hujson.String(n.Hostname))
				}
			}
			if o.AllowAll != n.AllowAll {
				if !n.AllowAll {
					removeMember(obj, "allow_all")
				} else {
					setMember(obj, "allow_all", hujson.Bool(true))
				}
			}
			if o.NativeDNS != n.NativeDNS {
				if !n.NativeDNS {
					removeMember(obj, "native_dns")
				} else {
					setMember(obj, "native_dns", hujson.Bool(true))
				}
			}
			if !slices.Equal(o.Resources, n.Resources) {
				if len(n.Resources) == 0 {
					removeMember(obj, "resources")
				} else {
					els := make([]hujson.ArrayElement, len(n.Resources))
					for j, r := range n.Resources {
						els[j] = hujson.Value{Value: hujson.String(r)}
					}
					setMember(obj, "resources", &hujson.Array{Elements: els})
				}
			}
		}
	}

	// Never write a file we can't parse back to exactly what we intended.
	// (Validate on a copy: hujson.Standardize rewrites comment bytes in place,
	// so parseConfig would clobber out if handed the same backing array.)
	out := v.Pack()
	got, err := parseConfig(slices.Clone(out), path)
	if err != nil {
		return fmt.Errorf("patched config no longer parses: %w", err)
	}
	if !configsEqual(got, next) {
		return fmt.Errorf("patched config does not match the intended change")
	}
	return atomicWrite(path, out)
}

// patchGlobals writes the daemon-wide members (mtu, listeners, upstream, hosts
// file, state dir) that changed between prev and next. A cleared value removes
// the member — the code's defaults take over. configsEqual compares these too,
// so an unpatched global would fail the round-trip check below.
func patchGlobals(root *hujson.Object, prev, next *Config) {
	if prev.MTU != next.MTU {
		if next.MTU == 0 {
			removeMember(root, "mtu")
		} else {
			setGlobal(root, "mtu", hujson.Int(int64(next.MTU)))
		}
	}
	for _, g := range [...]struct{ name, was, now string }{
		{"dns_listen", prev.DNSListen, next.DNSListen},
		{"upstream_dns", prev.UpstreamDNS, next.UpstreamDNS},
		{"ui_listen", prev.UIListen, next.UIListen},
		{"hosts_file", prev.HostsFile, next.HostsFile},
		{"state_dir", prev.StateDir, next.StateDir},
	} {
		if g.was == g.now {
			continue
		}
		if g.now == "" {
			removeMember(root, g.name)
			continue
		}
		setGlobal(root, g.name, hujson.String(g.now))
	}
}

// setGlobal replaces a root member's value, or inserts it above the tailnets
// array when new — globals read above the list of tailnets.
func setGlobal(root *hujson.Object, name string, val hujson.ValueTrimmed) {
	for i := range root.Members {
		if nameOf(root.Members[i]) == name {
			root.Members[i].Value.Value = val
			return
		}
	}
	idx := len(root.Members)
	for i := range root.Members {
		if nameOf(root.Members[i]) == "tailnets" {
			idx = i
			break
		}
	}
	m := hujson.ObjectMember{
		Name:  hujson.Value{BeforeExtra: hujson.Extra("\n  "), Value: hujson.String(name)},
		Value: hujson.Value{BeforeExtra: hujson.Extra(" "), Value: val},
	}
	root.Members = append(root.Members, hujson.ObjectMember{})
	copy(root.Members[idx+1:], root.Members[idx:])
	root.Members[idx] = m
}

// configsEqual compares the fields that round-trip through the config file;
// nil and empty slices are equal (omitempty makes them indistinguishable).
func configsEqual(a, b *Config) bool {
	if a.MTU != b.MTU || a.DNSListen != b.DNSListen || a.UpstreamDNS != b.UpstreamDNS ||
		a.StateDir != b.StateDir || a.HostsFile != b.HostsFile || a.UIListen != b.UIListen ||
		len(a.Tailnets) != len(b.Tailnets) {
		return false
	}
	for i := range a.Tailnets {
		x, y := a.Tailnets[i], b.Tailnets[i]
		if x.Name != y.Name || x.Suffix != y.Suffix || x.Domain != y.Domain ||
			x.Hostname != y.Hostname ||
			x.CIDR != y.CIDR || x.TUN != y.TUN || x.AllowAll != y.AllowAll ||
			x.StateDir != y.StateDir || !slices.Equal(x.Resources, y.Resources) {
			return false
		}
		if (x.Enabled == nil) != (y.Enabled == nil) || x.Enabled != nil && *x.Enabled != *y.Enabled {
			return false
		}
	}
	return true
}

// tailnetsArray returns the root object's "tailnets" array, creating an
// empty one when the member is absent (a zero-tailnet config is valid; the
// first runtime add needs somewhere to land).
func tailnetsArray(root *hujson.Object) (*hujson.Array, error) {
	for i := range root.Members {
		if nameOf(root.Members[i]) != "tailnets" {
			continue
		}
		arr, ok := root.Members[i].Value.Value.(*hujson.Array)
		if !ok {
			return nil, fmt.Errorf("tailnets is not an array")
		}
		return arr, nil
	}
	arr := &hujson.Array{}
	root.Members = append(root.Members, hujson.ObjectMember{
		Name:  hujson.Value{BeforeExtra: hujson.Extra("\n  "), Value: hujson.String("tailnets")},
		Value: hujson.Value{BeforeExtra: hujson.Extra(" "), Value: arr},
	})
	return arr, nil
}

// tailnetObject finds the tailnets-array element whose "name" is name.
func tailnetObject(arr *hujson.Array, name string) (*hujson.Object, error) {
	for i := range arr.Elements {
		obj, ok := arr.Elements[i].Value.(*hujson.Object)
		if !ok {
			continue
		}
		for j := range obj.Members {
			if nameOf(obj.Members[j]) == "name" {
				lit, ok := obj.Members[j].Value.Value.(hujson.Literal)
				if ok && lit.String() == name {
					return obj, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("tailnet %q not found in config", name)
}

// nameOf returns the unquoted member name.
func nameOf(m hujson.ObjectMember) string {
	lit, ok := m.Name.Value.(hujson.Literal)
	if !ok {
		return ""
	}
	return lit.String()
}

// removeTailnetElement deletes the element whose "name" is name; a no-op
// (false) when absent. Pack's comma handling absorbs the gap automatically.
func removeTailnetElement(arr *hujson.Array, name string) bool {
	for i := range arr.Elements {
		obj, ok := arr.Elements[i].Value.(*hujson.Object)
		if !ok {
			continue
		}
		for j := range obj.Members {
			if nameOf(obj.Members[j]) == "name" {
				if lit, ok := obj.Members[j].Value.Value.(hujson.Literal); ok && lit.String() == name {
					arr.Elements = append(arr.Elements[:i], arr.Elements[i+1:]...)
					return true
				}
			}
		}
	}
	return false
}

// insertTailnetElement appends a tailnet object in canonical member order
// (name, cidr, tun, then set optionals), formatted like the shipped config:
// indented members and the trailing-comma style hujson flags via a non-nil
// AfterExtra on the last element. Bare members — comments are human territory.
func insertTailnetElement(arr *hujson.Array, tc TailnetConf) {
	obj := &hujson.Object{}
	addMember := func(name string, val hujson.ValueTrimmed) {
		obj.Members = append(obj.Members, hujson.ObjectMember{
			Name:  hujson.Value{BeforeExtra: hujson.Extra("\n      "), Value: hujson.String(name)},
			Value: hujson.Value{BeforeExtra: hujson.Extra(" "), Value: val},
		})
	}
	addMember("name", hujson.String(tc.Name))
	addMember("cidr", hujson.String(tc.CIDR))
	addMember("tun", hujson.String(tc.TUN))
	if tc.Hostname != "" {
		addMember("hostname", hujson.String(tc.Hostname))
	}
	if tc.Suffix != "" {
		addMember("suffix", hujson.String(tc.Suffix))
	}
	if tc.Domain != "" {
		addMember("domain", hujson.String(tc.Domain))
	}
	if tc.Enabled != nil {
		addMember("enabled", hujson.Bool(*tc.Enabled))
	}
	if tc.AllowAll {
		addMember("allow_all", hujson.Bool(true))
	}
	if len(tc.Resources) > 0 {
		els := make([]hujson.ArrayElement, len(tc.Resources))
		for j, r := range tc.Resources {
			els[j] = hujson.Value{Value: hujson.String(r)}
		}
		addMember("resources", &hujson.Array{Elements: els})
	}
	obj.AfterExtra = hujson.Extra("\n    ")
	arr.Elements = append(arr.Elements, hujson.Value{BeforeExtra: hujson.Extra("\n    "), Value: obj})
	// hujson emits the trailing comma when the last element's AfterExtra is
	// non-nil; the file's style keeps it.
	if last := &arr.Elements[len(arr.Elements)-1]; last.AfterExtra == nil {
		last.AfterExtra = hujson.Extra("")
	}
}

// setMember replaces an existing member's value (keeping its name, comments,
// and formatting) or appends a new member.
func setMember(obj *hujson.Object, name string, val hujson.ValueTrimmed) {
	for i := range obj.Members {
		if nameOf(obj.Members[i]) == name {
			obj.Members[i].Value.Value = val
			return
		}
	}
	obj.Members = append(obj.Members, hujson.ObjectMember{
		Name:  hujson.Value{Value: hujson.String(name)},
		Value: hujson.Value{Value: val},
	})
}

// removeMember deletes a member; a no-op when absent.
func removeMember(obj *hujson.Object, name string) {
	for i := range obj.Members {
		if nameOf(obj.Members[i]) == name {
			obj.Members = append(obj.Members[:i], obj.Members[i+1:]...)
			return
		}
	}
}

// atomicWrite writes b to path via a temp file and rename, preserving the
// original mode (0600 for a new file — configs live under /etc).
func atomicWrite(path string, b []byte) error {
	mode := os.FileMode(0600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
