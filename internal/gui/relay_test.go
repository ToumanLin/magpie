package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/mode"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/relay"
)

func relayHome(t *testing.T, m string) func(method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	t.Cleanup(mode.Set(m))
	relay.Forget()
	srv := Handler(webHost{func() {}}, nil)
	return func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
}

// A page left out is not what keeps a program to its own: Mag's handler
// refuses the server's routes and Pie's the client's, whatever asks, and
// magpie itself has neither's.
func TestModeGuardRefusesTheOtherPrograms(t *testing.T) {
	type call struct{ method, path, body string }
	serverSide := []call{
		{"POST", "/api/provider/save", `{"id":"x","name":"x","chat":"http://127.0.0.1:1/v1","key":"k"}`},
		{"POST", "/api/keys/add", `{}`},
		{"GET", "/api/caller-keys", ""},
		{"POST", "/api/caller-keys/add-key", `{"name":"x"}`},
		{"POST", "/api/signin", `{"agent":"claude"}`},
		{"POST", "/api/login/remove", `{}`},
		{"POST", "/api/groups/save", `{}`},
		{"POST", "/api/plugins/add", `{}`},
		{"GET", "/api/relay", ""},
		{"POST", "/api/relay/member-save", `{"member":{"name":"mallory"}}`},
		{"POST", "/api/settings/lan", `{"lan":true}`},
		{"POST", "/api/settings/reveal", `{}`},
	}
	clientSide := []call{
		{"POST", "/api/set", `{"agent":"claude","key":"model","value":"x"}`},
		{"POST", "/api/agents/connect/claude", `{}`},
		{"GET", "/api/sessions", ""},
		{"GET", "/api/sessions/manage", ""},
		{"GET", "/api/library", ""},
		{"GET", "/api/hub", ""},
		{"POST", "/api/hub/connect", `{"address":"http://127.0.0.1:1","how":"key","key":"k"}`},
	}
	refused := func(rec *httptest.ResponseRecorder) bool {
		return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "not_this_program")
	}

	do := relayHome(t, mode.Client)
	for _, c := range serverSide {
		if rec := do(c.method, c.path, c.body); !refused(rec) {
			t.Errorf("Mag answered %s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// what its own pages draw from stays: the model pickers' list, its own page
	for _, c := range []call{{"GET", "/api/providers", ""}, {"GET", "/api/hub", ""}, {"GET", "/api/state", ""}, {"GET", "/api/groups", ""}} {
		if rec := do(c.method, c.path, c.body); rec.Code != http.StatusOK {
			t.Errorf("Mag refused its own %s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if boot := do("GET", "/boot.js", "").Body.String(); !strings.Contains(boot, `"relay":"client"`) {
		t.Errorf("Mag's boot.js: %s", boot)
	}
	// and nothing was written for what it refused
	if v, err := relay.Snapshot(); err != nil || len(v.Members) != 0 {
		t.Errorf("a refused route wrote a member: %+v %v", v.Members, err)
	}
	if ps := provider.All(); len(ps) != 0 {
		t.Errorf("a refused route saved a provider: %d", len(ps))
	}

	do = relayHome(t, mode.Server)
	for _, c := range clientSide {
		if rec := do(c.method, c.path, c.body); !refused(rec) {
			t.Errorf("Pie answered %s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if boot := do("GET", "/boot.js", "").Body.String(); !strings.Contains(boot, `"relay":"server"`) {
		t.Errorf("Pie's boot.js: %s", boot)
	}
	// Pie's state lists no agent of the computer it runs on
	var st struct {
		Agents []json.RawMessage `json:"agents"`
	}
	if rec := do("GET", "/api/state", ""); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil || len(st.Agents) != 0 {
		t.Errorf("Pie's state: %d, %d agents", rec.Code, len(st.Agents))
	}

	do = relayHome(t, "")
	for _, c := range []call{{"GET", "/api/relay", ""}, {"GET", "/api/hub", ""}, {"POST", "/api/relay/member-save", `{"member":{"name":"mallory"}}`}} {
		if rec := do(c.method, c.path, c.body); !refused(rec) {
			t.Errorf("magpie answered %s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if boot := do("GET", "/boot.js", "").Body.String(); strings.Contains(boot, `"relay"`) {
		t.Errorf("magpie's boot.js names a relay mode: %s", boot)
	}
	// magpie's own routes are as they were
	if rec := do("GET", "/api/caller-keys", ""); rec.Code != http.StatusOK {
		t.Errorf("magpie's gateway keys: %d %s", rec.Code, rec.Body)
	}
}

// The administrator's page: a group, a plan and a member made, an
// invitation whose code is in the answer once and in no file, and the
// state the page draws from with nothing that signs anyone in.
func TestRelayAdminRoutes(t *testing.T) {
	do := relayHome(t, mode.Server)
	post := func(action, body string, out any) {
		t.Helper()
		rec := do("POST", "/api/relay/"+action, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body)
		}
		if out != nil {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	post("group-save", `{"group":{"name":"lab","models":["group/coding"]}}`, nil)
	post("plan-save", `{"plan":{"name":"monthly","kind":"period","price":10,"tokens":1000}}`, nil)
	var made struct {
		ID    string
		State relayJSON
	}
	post("member-save", `{"member":{"name":"alice","groups":["lab"],"plan":"monthly"}}`, &made)
	if made.ID == "" || len(made.State.Members) != 1 || made.State.Standing[made.ID].Limit == nil || made.State.Standing[made.ID].Limit.TokenLimit != 1000 {
		t.Fatalf("after adding alice: %+v", made.State.Standing)
	}
	var inv struct {
		Code  string
		State relayJSON
	}
	post("invite", `{"id":"`+made.ID+`"}`, &inv)
	if !strings.HasPrefix(inv.Code, relay.InvitePrefix) || inv.State.Members[0].Invited == 0 {
		t.Fatalf("the invitation: %v", inv.State.Members[0].Invited)
	}
	rec := do("GET", "/api/relay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("state: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, word := range []string{inv.Code, "passHash", "passSalt", `"hash"`, `"secret"`} {
		if strings.Contains(body, word) {
			t.Fatalf("the page's state holds %q", word)
		}
	}
	if rec := do("POST", "/api/relay/plan-remove", `{"id":"monthly"}`); rec.Code == http.StatusOK {
		t.Fatal("a plan a member is on was removed")
	}
	if rec := do("POST", "/api/relay/nonsense", `{}`); rec.Code == http.StatusOK {
		t.Fatal("an unknown action was taken")
	}
	post("member-remove", `{"id":"`+made.ID+`"}`, &made)
	if len(made.State.Members) != 0 {
		t.Fatal("alice is still there")
	}
}
