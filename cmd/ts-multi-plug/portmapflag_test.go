package main

import "testing"

func TestPortMapFlagSet(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
		in, out int
		outPath string
	}{
		{name: "empty uses defaults", value: "", in: 4, out: 5},
		{name: "single port", value: "8080", in: 8080, out: 8080},
		{name: "in:out", value: "80:3000", in: 80, out: 3000},
		{name: "unix upstream", value: "22:unix:/run/ssh-unix-local/socket", in: 22, outPath: "/run/ssh-unix-local/socket"},
		{name: "unix path with colon", value: "2375:unix:/tmp/weird:name.sock", in: 2375, outPath: "/tmp/weird:name.sock"},
		{name: "relative unix path", value: "22:unix:run/x.sock", wantErr: true},
		{name: "empty unix path", value: "22:unix:", wantErr: true},
		{name: "non-numeric in", value: "x:80", wantErr: true},
		{name: "non-numeric out", value: "80:x", wantErr: true},
		{name: "triple numeric (old silent bug)", value: "1:2:3", wantErr: true},
		{name: "in port zero", value: "0:80", wantErr: true},
		{name: "out port too big", value: "80:70000", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPortMapFlag(4, 5)
			err := p.Set(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) = nil, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q) = %v, want nil", tt.value, err)
			}
			if p.In != tt.in || p.Out != tt.out || p.OutPath != tt.outPath {
				t.Errorf("Set(%q) = In:%d Out:%d OutPath:%q, want In:%d Out:%d OutPath:%q",
					tt.value, p.In, p.Out, p.OutPath, tt.in, tt.out, tt.outPath)
			}
			if !p.IsSet() {
				t.Errorf("IsSet() = false after Set(%q)", tt.value)
			}
		})
	}
}

func TestPortMapFlagUpstream(t *testing.T) {
	tcp := NewPortMapFlag(22, 22)
	if err := tcp.Set("22:2022"); err != nil {
		t.Fatal(err)
	}
	if got := tcp.UpstreamNetwork(); got != "tcp" {
		t.Errorf("UpstreamNetwork() = %q, want tcp", got)
	}
	if got := tcp.UpstreamAddr(); got != "127.0.0.1:2022" {
		t.Errorf("UpstreamAddr() = %q, want 127.0.0.1:2022", got)
	}
	if got := tcp.String(); got != "22:2022" {
		t.Errorf("String() = %q, want 22:2022", got)
	}

	unix := NewPortMapFlag(22, 22)
	if err := unix.Set("22:unix:/run/test.sock"); err != nil {
		t.Fatal(err)
	}
	if got := unix.UpstreamNetwork(); got != "unix" {
		t.Errorf("UpstreamNetwork() = %q, want unix", got)
	}
	if got := unix.UpstreamAddr(); got != "/run/test.sock" {
		t.Errorf("UpstreamAddr() = %q, want /run/test.sock", got)
	}
	if got := unix.UpstreamLabel(); got != "unix:/run/test.sock" {
		t.Errorf("UpstreamLabel() = %q, want unix:/run/test.sock", got)
	}
	if got := unix.String(); got != "22:unix:/run/test.sock" {
		t.Errorf("String() = %q, want 22:unix:/run/test.sock", got)
	}
}
