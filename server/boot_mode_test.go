package server

import "testing"

func TestIsMigrateOnlyMode(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "unset defaults to serve", set: false, want: false},
		{name: "migrate selects migrate-only", value: "migrate", set: true, want: true},
		{name: "serve stays serve", value: "serve", set: true, want: false},
		{name: "unrecognized value stays serve", value: "Migrate", set: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("SERVER_MODE", tc.value)
			}
			if got := IsMigrateOnlyMode(); got != tc.want {
				t.Errorf("IsMigrateOnlyMode() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldSkipAutoMigrate(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "unset keeps default AutoMigrate", set: false, want: false},
		{name: "1 skips AutoMigrate", value: "1", set: true, want: true},
		{name: "true does not skip", value: "true", set: true, want: false},
		{name: "0 does not skip", value: "0", set: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("SKIP_AUTOMIGRATE", tc.value)
			}
			if got := ShouldSkipAutoMigrate(); got != tc.want {
				t.Errorf("ShouldSkipAutoMigrate() = %v, want %v", got, tc.want)
			}
		})
	}
}
