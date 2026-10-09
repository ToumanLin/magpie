package backup

import (
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/mode"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// Mag's backup is its hub connection and nothing of a server's: whatever a
// bundle holds — one a magpie or a Pie made — a Mag neither writes out nor
// takes in an upstream provider's key, a gateway key or a search key, and
// its hub connection comes back with the device key it was sealed with.
func TestClientBundleHoldsNoUpstreamKey(t *testing.T) {
	home(t)
	appdir.UseExecutable("")
	// a magpie's own setup: an upstream provider, a hub, a gateway key
	for _, p := range []provider.Provider{
		{ID: "vendor", Name: "Vendor", Key: "sk-upstream-secret", Chat: "https://api.vendor.example/v1", Models: []string{"m1"}},
		{ID: "hub", Name: "AI Relay Hub", Preset: provider.RemoteMagpiePreset, Key: "sk-magpie-key-device", Chat: "http://192.168.1.20:3425", Models: []string{"group/coding"}},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := access.Update("add-key", access.Change{Name: "a server's key"}); err != nil {
		t.Fatal(err)
	}
	s := settings.Load()
	s.GitHubToken, s.Theme = "ghp-local-secret", "dark"
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	full, err := Collect(true, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Providers) != 2 || full.GatewayKeys == nil {
		t.Fatalf("the whole bundle: %d providers, gateway keys %v", len(full.Providers), full.GatewayKeys != nil)
	}

	b := full
	b.ForClient()
	if len(b.Providers) != 1 || b.Providers[0].ID != "hub" || b.Providers[0].Key != "sk-magpie-key-device" {
		t.Fatalf("a client's bundle keeps %+v", b.Providers)
	}
	if b.GatewayKeys != nil || b.Searches != nil || b.Groups != nil || b.Settings == nil || b.Settings.GitHubToken != "" || b.Settings.Theme != "dark" {
		t.Fatalf("a client's bundle carries a server's part: keys %v, settings %+v", b.GatewayKeys != nil, b.Settings)
	}
	if len(full.Providers) != 2 || full.Settings.GitHubToken != "ghp-local-secret" {
		t.Fatal("ForClient changed the bundle it was made from")
	}
	sealed, err := Seal(b, "a passphrase")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-upstream-secret", "sk-magpie-key-device", "ghp-local-secret"} {
		if strings.Contains(string(sealed), secret) {
			t.Fatalf("the sealed file shows %s", secret)
		}
	}
	opened, err := Open(sealed, "a passphrase")
	if err != nil || len(opened.Providers) != 1 {
		t.Fatalf("opening it: %v", err)
	}

	// a Mag on another computer is given the whole bundle: it takes in the hub alone
	home(t)
	appdir.UseExecutable("")
	t.Cleanup(mode.Set(mode.Client))
	cur := settings.Load()
	cur.GitHubToken = "this-computers-own"
	if err := settings.Save(cur); err != nil {
		t.Fatal(err)
	}
	in := full
	in.ForClient()
	res, err := Restore(in, All)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 {
		t.Fatalf("restored %d providers, want the hub alone", res.Added)
	}
	raw, _ := os.ReadFile(provider.Path())
	if strings.Contains(string(raw), "sk-upstream-secret") || strings.Contains(string(raw), "api.vendor.example") {
		t.Fatal("an upstream provider or its key was written into a client")
	}
	ps := provider.All()
	if len(ps) != 1 || ps[0].ID != "hub" || !ps[0].IsRemoteMagpie() || ps[0].Key != "sk-magpie-key-device" {
		t.Fatalf("the client's providers after the restore: %d", len(ps))
	}
	if keys, _ := access.List(); len(keys) != 0 {
		t.Fatalf("a server's gateway keys were restored into a client: %d", len(keys))
	}
	if got := settings.Load(); got.GitHubToken != "this-computers-own" || got.Theme != "dark" {
		t.Fatalf("settings after the restore: token kept %v, theme %q", got.GitHubToken == "this-computers-own", got.Theme)
	}
	// and a client refuses a provider that is no hub, however it is asked
	if err := provider.Save(provider.Provider{ID: "vendor2", Name: "V", Key: "k", Chat: "https://api.vendor.example/v1"}); err == nil {
		t.Fatal("a client saved an upstream provider")
	}
}
