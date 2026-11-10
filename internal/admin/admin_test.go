package admin_test

import (
	"testing"

	"github.com/backplane-labs/backplane-courier/internal/admin"
)

func TestHostsText(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		hosts    []string
		insecure bool
		want     string
	}{
		"default":         {want: "fallback"},
		"hosts":           {hosts: []string{"a.example", "*.b.example"}, want: "a.example, *.b.example"},
		"insecure":        {insecure: true, want: "any host; http and internal addresses allowed"},
		"insecure, hosts": {hosts: []string{"push-fake"}, insecure: true, want: "push-fake; http and internal addresses allowed"},
	} {
		if got := admin.HostsText(c.hosts, c.insecure, "fallback"); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
