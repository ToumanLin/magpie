package hubclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/mode"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/relay"
	"github.com/yetone/magpie/internal/testenv"
)

// The tests run in a home of their own, never the user's real files.
func TestMain(m *testing.M) { testenv.Main(m) }

// hub stands a hub up with one vendor, one group and one member, alice,
// and answers its address and alice's id. The hub and the Mag under test
// share the process, and so its folder: what the test looks at is what Mag
// asks the hub and what it keeps.
func hub(t *testing.T) (root, alice string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	t.Cleanup(mode.Set(mode.Server))
	relay.Forget()
	budget.Forget()
	was := relay.PassRounds
	relay.PassRounds = 1000
	t.Cleanup(func() { relay.PassRounds = was })
	wasClient := client
	client = &http.Client{}
	t.Cleanup(func() { client = wasClient })

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: "upstream-secret", Chat: up.URL + "/v1", Models: []string{"m1", "m2"}}); err != nil {
		t.Fatal(err)
	}
	lab, err := relay.SaveGroup(relay.Group{Name: "lab", Models: []string{"plan/m1"}})
	if err != nil {
		t.Fatal(err)
	}
	alice, err = relay.SaveMember(relay.MemberIn{Name: "alice", Groups: []string{lab}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gateway.New().Handler())
	t.Cleanup(srv.Close)
	return srv.URL, alice
}

// An address is kept as the hub's root, however it was pasted.
func TestRoot(t *testing.T) {
	for in, want := range map[string]string{
		"relay.example.org":                       "http://relay.example.org",
		" https://relay.example.org/ ":            "https://relay.example.org",
		"https://relay.example.org/v1":            "https://relay.example.org",
		"https://relay.example.org/relay/":        "https://relay.example.org",
		"http://192.168.1.20:3425/v1/messages":    "http://192.168.1.20:3425",
		"https://example.org/tools/relay-hub/v1/": "https://example.org/tools/relay-hub",
	} {
		if got, err := Root(in); err != nil || got != want {
			t.Errorf("Root(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "https://user:pw@example.org", "https://example.org/?k=1", "https://"} {
		if got, err := Root(bad); err == nil {
			t.Errorf("Root(%q) = %q, want an error", bad, got)
		}
	}
	if !Plain("http://192.168.1.20:3425") || !Plain("http://relay.example.org") {
		t.Error("plain http over a network isn't said to be")
	}
	if Plain("https://relay.example.org") || Plain("http://127.0.0.1:3425") || Plain("http://localhost:3425") {
		t.Error("https, or this computer, is said to be plain")
	}
}

// With an invitation: the member's password is set, this computer becomes
// one device, and the device key is all that is kept — the session is
// signed out, and the password is in no file.
func TestConnectByInvitationThenLogin(t *testing.T) {
	root, alice := hub(t)
	ctx := context.Background()
	code, _, err := relay.Invite(alice)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConnectInvite(ctx, root+"/v1", code, "a long password", "alice-laptop"); err != nil {
		t.Fatal(err)
	}
	p, ok := Current()
	if !ok || !p.IsRemoteMagpie() || !strings.HasPrefix(p.Key, access.Prefix) || strings.TrimRight(p.Anthropic, "/") != root {
		t.Fatalf("the hub as kept: connected %v, remote %v, at %q", ok, p.IsRemoteMagpie(), p.Anthropic)
	}
	devs, _ := access.Devices(alice)
	if len(devs) != 1 || devs[0].Name != "alice-laptop" {
		t.Fatalf("alice's devices: %+v", devs)
	}
	st := StatusNow(ctx)
	if !st.Connected || st.Error != "" || st.Member == nil || st.Member.Name != "alice" || st.Device != "alice-laptop" || len(st.Models) != 1 || st.Models[0].ID != "plan/m1" {
		t.Fatalf("the status: %+v", st)
	}
	// the hub's models are in the local catalog under the hub's id, and only those alice may use
	var ids []string
	for _, e := range provider.Catalog() {
		if e.Provider.ID == ProviderID {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) != 1 || ids[0] != "hub/plan/m1" {
		t.Fatalf("the hub's models here: %v", ids)
	}
	if view, err := relay.Snapshot(); err != nil || !view.Members[0].HasPassword {
		t.Fatalf("alice's password: %+v %v", view.Members, err)
	}
	b, _ := os.ReadFile(provider.Path())
	if strings.Contains(string(b), "a long password") || strings.Contains(string(b), code) || strings.Contains(string(b), relay.SessionPrefix) {
		t.Fatal("providers.json holds the password, the invitation or a session")
	}
	// the sign-in was only for making the device: nothing is left signed in
	rb, _ := os.ReadFile(relay.Path())
	if strings.Contains(string(rb), `"sessions"`) {
		t.Fatal("the sign-in that made the device was left open")
	}

	// a second computer signs in by name and password, as its own device
	if err := ConnectLogin(ctx, root, "alice", "not the password", "desk"); err == nil {
		t.Fatal("connected with a wrong password")
	}
	if err := ConnectLogin(ctx, root, "alice", "a long password", "desk"); err != nil {
		t.Fatal(err)
	}
	if devs, _ := access.Devices(alice); len(devs) != 2 {
		t.Fatalf("alice's devices after a second: %+v", devs)
	}
	if st := StatusNow(ctx); st.Device != "desk" {
		t.Fatalf("the device now: %q", st.Device)
	}
	usage, err := Usage(ctx, "today")
	if err != nil || usage.Member != alice {
		t.Fatalf("usage: %+v %v", usage.Totals, err)
	}

	// its key taken away at the hub: the status says so, and nothing is served
	devs, _ = access.Devices(alice)
	for _, d := range devs {
		if d.Name == "desk" {
			if err := access.RemoveDevice("", d.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if st := StatusNow(ctx); !st.Connected || !st.Revoked || st.Error == "" {
		t.Fatalf("after the key was taken away: %+v", st)
	}
	if err := Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, ok := Current(); ok {
		t.Fatal("still connected")
	}
	if st := StatusNow(ctx); st.Connected {
		t.Fatal("the status of no hub says connected")
	}
}

// With a key made on the hub's page; a key that is no member's device, or
// an address that is no hub, connects nothing.
func TestConnectByKey(t *testing.T) {
	root, alice := hub(t)
	ctx := context.Background()
	_, key, err := access.AddDevice(alice, "made-on-the-page", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConnectKey(ctx, root, "sk-magpie-key-0000"); err == nil {
		t.Fatal("connected with a key that is no device's")
	}
	if _, ok := Current(); ok {
		t.Fatal("a refused key was kept")
	}
	notHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"something else"}`)
	}))
	defer notHub.Close()
	if err := ConnectKey(ctx, notHub.URL, key); err == nil || !strings.Contains(err.Error(), "not as an AI Relay hub") {
		t.Fatalf("an address that is no hub: %v", err)
	}
	if err := ConnectKey(ctx, root, key); err != nil {
		t.Fatal(err)
	}
	if st := StatusNow(ctx); st.Device != "made-on-the-page" || st.Member.Name != "alice" {
		t.Fatalf("the status: %+v", st)
	}
}
