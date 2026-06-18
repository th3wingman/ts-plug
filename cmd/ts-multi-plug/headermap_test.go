package main

import "testing"

func TestHeaderMapFlagSet(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    HeaderMapFlag
		wantErr bool
	}{
		{
			name:  "border0 preset",
			value: "login=X-Auth-Email,name=X-Auth-Name,pic=X-Auth-Picture",
			want:  HeaderMapFlag{Login: "X-Auth-Email", Name: "X-Auth-Name", Pic: "X-Auth-Picture"},
		},
		{
			name:  "subset",
			value: "login=X-Auth-Email",
			want:  HeaderMapFlag{Login: "X-Auth-Email"},
		},
		{
			name:    "unknown field",
			value:   "email=X-Auth-Email",
			wantErr: true,
		},
		{
			name:    "duplicate field",
			value:   "login=X-Auth-Email,login=X-Other",
			wantErr: true,
		},
		{
			name:    "missing header name",
			value:   "login=",
			wantErr: true,
		},
		{
			name:    "missing separator",
			value:   "login",
			wantErr: true,
		},
		{
			name:    "invalid header name",
			value:   "login=X-Auth Email",
			wantErr: true,
		},
		{
			name:    "empty",
			value:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got HeaderMapFlag
			err := got.Set(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) = nil error, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q) failed: %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("Set(%q) = %+v, want %+v", tt.value, got, tt.want)
			}
		})
	}
}

func TestHeaderMapFlagRepeatedSetMerges(t *testing.T) {
	var h HeaderMapFlag
	if err := h.Set("login=X-Auth-Email"); err != nil {
		t.Fatal(err)
	}
	if err := h.Set("name=X-Auth-Name"); err != nil {
		t.Fatal(err)
	}
	want := HeaderMapFlag{Login: "X-Auth-Email", Name: "X-Auth-Name"}
	if h != want {
		t.Errorf("merged flag = %+v, want %+v", h, want)
	}
	if err := h.Set("login=X-Other"); err == nil {
		t.Error("re-mapping login across invocations should error")
	}
}
