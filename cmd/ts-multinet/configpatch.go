// Config patching: the daemon owns the config file, but humans edit it too,
// so mutations go through the hujson AST — comments and manual formatting
// survive every write. The only mutable keys are the per-tailnet domain,
// allow_all, and resources; tailnet structure (name/cidr/tun) is static and
// still requires a daemon restart.

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
// decides which members to set, insert, or remove. Fails without writing if
// the two configs differ structurally or the patched bytes don't re-parse to
// exactly next.
func patchConfigFile(path string, prev, next *Config) error {
	if len(prev.Tailnets) != len(next.Tailnets) {
		return fmt.Errorf("config patch cannot add or remove tailnets — restart the daemon instead")
	}
	for i := range next.Tailnets {
		o, n := prev.Tailnets[i], next.Tailnets[i]
		if o.Name != n.Name || o.CIDR != n.CIDR || o.TUN != n.TUN {
			return fmt.Errorf("config patch cannot change tailnet structure — restart the daemon instead")
		}
	}

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
	arr, err := tailnetsArray(root)
	if err != nil {
		return err
	}

	for i := range next.Tailnets {
		o, n := prev.Tailnets[i], next.Tailnets[i]
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
		if o.AllowAll != n.AllowAll {
			if !n.AllowAll {
				removeMember(obj, "allow_all")
			} else {
				setMember(obj, "allow_all", hujson.Bool(true))
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

// tailnetsArray returns the root object's "tailnets" array.
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
	return nil, fmt.Errorf("config has no tailnets member")
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
