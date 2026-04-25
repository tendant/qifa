package deploy

import (
	"testing"

	"github.com/gokamal/gocart/internal/config"
)

func TestOrderedRolesWebFirst(t *testing.T) {
	roles := orderedRoles(map[string]config.Server{
		"worker": {},
		"cron":   {},
		"web":    {},
	})
	if len(roles) != 3 {
		t.Fatalf("unexpected roles: %#v", roles)
	}
	if roles[0] != "web" {
		t.Fatalf("expected web first, got %#v", roles)
	}
}

func TestServerUsesProxy(t *testing.T) {
	falseValue := false
	trueValue := true

	tests := []struct {
		name   string
		role   string
		server config.Server
		want   bool
	}{
		{
			name: "web defaults true",
			role: "web",
			want: true,
		},
		{
			name: "non web defaults false",
			role: "app",
			want: false,
		},
		{
			name: "web can disable proxy",
			role: "web",
			server: config.Server{
				Proxy: &falseValue,
			},
			want: false,
		},
		{
			name: "non web can enable proxy",
			role: "app",
			server: config.Server{
				Proxy: &trueValue,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serverUsesProxy(tt.role, tt.server); got != tt.want {
				t.Fatalf("serverUsesProxy(%q) = %v, want %v", tt.role, got, tt.want)
			}
		})
	}
}
