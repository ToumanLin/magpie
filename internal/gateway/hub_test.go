package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/mode"
	"github.com/yetone/magpie/internal/provider"
	hub "github.com/yetone/magpie/internal/relay"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

// These tests stand a hub up as Pie serves it — lanGuard in front of the
// handler, on a real listener — with stand-in vendors behind it. No test
// here asks a real vendor, and none prints a key, a code or a password.

// hubT is a hub under test.
type hubT struct {
	t   *testing.T
	url string
}

// newHub starts a hub with nothing in it.
func newHub(t *testing.T) *hubT {
	t.Helper()
	fresh(t)
	t.Cleanup(mode.Set(mode.Server))
	budget.Forget()
	hub.Forget()
	was := hub.PassRounds
	hub.PassRounds = 1000 // a test signs in often; the rounds are the member's record's
	t.Cleanup(func() { hub.PassRounds = was })
	memberLanes.mu.Lock()
	memberLanes.m = nil
	memberLanes.mu.Unlock()
	srv := httptest.NewServer(lanGuard(New().Handler()))
	t.Cleanup(srv.Close)
	return &hubT{t: t, url: srv.URL}
}

// vendor is a stand-in provider with one model, answered by reply.
func vendor(t *testing.T, id, model string, reply http.HandlerFunc) {
	t.Helper()
	up := httptest.NewServer(reply)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: id, Name: id, Key: "upstream-" + id, Chat: up.URL + "/v1", Models: []string{model}}); err != nil {
		t.Fatal(err)
	}
}

// answers replies as a Chat Completions vendor that reports its usage.
func answers(in, out int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, in, out)
	}
}

func (h *hubT) group(name string, models ...string) string {
	h.t.Helper()
	id, err := hub.SaveGroup(hub.Group{Name: name, Models: models})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// member adds a member, has it redeem an invitation and make a device,
// all through the hub's own routes, and answers its id and device key.
func (h *hubT) member(in hub.MemberIn) (id, key string) {
	h.t.Helper()
	id, err := hub.SaveMember(in)
	if err != nil {
		h.t.Fatal(err)
	}
	code, _, err := hub.Invite(id)
	if err != nil {
		h.t.Fatal(err)
	}
	var signed struct{ Token string }
	if status := h.post("", "/relay/api/redeem", map[string]string{"code": code, "password": "a long password"}, &signed); status != 200 || signed.Token == "" {
		h.t.Fatalf("redeeming %s's invitation: %d", in.Name, status)
	}
	var dev struct{ ID, Key string }
	if status := h.post(signed.Token, "/relay/api/devices", map[string]string{"name": in.Name + "-laptop"}, &dev); status != 200 || dev.Key == "" {
		h.t.Fatalf("making %s's device: %d", in.Name, status)
	}
	return id, dev.Key
}

func (h *hubT) do(method, token, path string, body any, out any) int {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, h.url+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Relay", "1")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if out != nil {
		if s, ok := out.(*string); ok {
			*s = string(b)
		} else {
			_ = json.Unmarshal(b, out)
		}
	}
	return res.StatusCode
}

func (h *hubT) post(token, path string, body any, out any) int {
	return h.do("POST", token, path, body, out)
}
func (h *hubT) get(token, path string, out any) int { return h.do("GET", token, path, nil, out) }

func chatFor(model string) map[string]any {
	return map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
}

func modelIDs(t *testing.T, h *hubT, key string) []string {
	t.Helper()
	var list struct {
		Data []struct{ ID string }
	}
	if status := h.get(key, "/v1/models", &list); status != 200 {
		t.Fatalf("/v1/models: %d", status)
	}
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// The whole of it: the administrator makes two groups and invites two
// members; each signs in and makes a device key; each is served only the
// models of its group, sees only its own use, and one's key taken away
// leaves the other's working.
func TestHubTwoMembers(t *testing.T) {
	h := newHub(t)
	vendor(t, "plan", "m1", answers(30, 5))
	vendor(t, "other", "m2", answers(7, 3))
	if err := provider.SaveGroup(provider.Group{Name: "Coding", Members: []string{"plan/m1"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	lab := h.group("lab", "group/coding")
	private := h.group("private", "other/m2")
	alice, aliceKey := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}})
	bob, bobKey := h.member(hub.MemberIn{Name: "bob", Groups: []string{private}})

	// every request for a model is signed, from this computer too
	if status := h.post("", "/v1/chat/completions", chatFor("group/coding"), nil); status != 401 {
		t.Fatalf("a request with no key: %d, want 401", status)
	}
	if status := h.post("magpie", "/v1/chat/completions", chatFor("group/coding"), nil); status != 401 {
		t.Fatalf("a request with magpie's own token: %d, want 401", status)
	}
	if status := h.get("", "/v1/models", nil); status != 401 {
		t.Fatalf("the model list with no key: %d, want 401", status)
	}

	// each is listed and served its group's models, and no other's
	if ids := modelIDs(t, h, aliceKey); len(ids) != 1 || ids[0] != "group/coding" {
		t.Fatalf("alice is listed %v, want the coding group alone", ids)
	}
	if ids := modelIDs(t, h, bobKey); len(ids) != 1 || ids[0] != "other/m2" {
		t.Fatalf("bob is listed %v, want other/m2 alone", ids)
	}
	var out string
	if status := h.post(aliceKey, "/v1/chat/completions", chatFor("group/coding"), &out); status != 200 {
		t.Fatalf("alice on her group: %d %s", status, out)
	}
	if status := h.post(bobKey, "/v1/chat/completions", chatFor("other/m2"), &out); status != 200 {
		t.Fatalf("bob on his model: %d %s", status, out)
	}
	for _, m := range []string{"group/coding", "plan/m1"} {
		if status := h.post(bobKey, "/v1/chat/completions", chatFor(m), &out); status != 403 {
			t.Fatalf("bob on %s: %d, want 403", m, status)
		}
	}
	if status := h.post(aliceKey, "/v1/chat/completions", chatFor("other/m2"), &out); status != 403 {
		t.Fatalf("alice on bob's model: %d, want 403", status)
	}

	// the hub's own reports name its accounts: no member's device is told them
	for _, p := range []string{"/v1/magpie/quotas", "/v1/magpie/quotas/cards", "/v1/magpie/concurrency", "/v1/magpie/route?session=x", "/backend-api/codex/models"} {
		if status := h.get(aliceKey, p, &out); status != 403 {
			t.Fatalf("%s for a member: %d, want 403 (%s)", p, status, out)
		}
	}

	// each sees its own use, from the one ledger
	var as, bs hub.Statement
	if status := h.get(aliceKey, "/relay/api/usage?period=today", &as); status != 200 {
		t.Fatalf("alice's usage: %d", status)
	}
	if status := h.get(bobKey, "/relay/api/usage?period=today", &bs); status != 200 {
		t.Fatalf("bob's usage: %d", status)
	}
	if as.Member != alice || as.Calls != 1 || as.Input != 30 || as.Output != 5 || as.Refused != 1 {
		t.Fatalf("alice's statement: %+v", as.Totals)
	}
	if bs.Member != bob || bs.Calls != 1 || bs.Input != 7 || bs.Output != 3 || bs.Refused != 2 {
		t.Fatalf("bob's statement: %+v", bs.Totals)
	}
	for _, e := range as.Recent {
		if e.Device != "alice-laptop" {
			t.Fatalf("alice's statement has %q's call", e.Device)
		}
	}
	// and the administrator's totals are of the same records
	lines, all := hub.Overview("today", time.Now())
	if len(lines) != 2 || all.Calls != 2 || all.Input != 37 || all.Output != 8 {
		t.Fatalf("the administrator's overview: %+v %+v", lines, all)
	}

	// bob's device taken away: his next request is refused, alice's goes on
	devs, err := access.Devices(bob)
	if err != nil || len(devs) != 1 {
		t.Fatalf("bob's devices: %v %v", devs, err)
	}
	if err := access.RemoveDevice("", devs[0].ID); err != nil {
		t.Fatal(err)
	}
	if status := h.post(bobKey, "/v1/chat/completions", chatFor("other/m2"), &out); status != 401 {
		t.Fatalf("bob after his key was taken away: %d, want 401", status)
	}
	if status := h.get(bobKey, "/relay/api/usage", nil); status != 401 {
		t.Fatalf("bob's page with the key taken away: %d, want 401", status)
	}
	if status := h.post(aliceKey, "/v1/chat/completions", chatFor("group/coding"), &out); status != 200 {
		t.Fatalf("alice after bob's key was taken away: %d %s", status, out)
	}

	// nothing a member said or was answered is in the ledger, and no key
	b, _ := os.ReadFile(usage.Path())
	for _, secret := range []string{aliceKey, bobKey, "upstream-plan", `"hi"`} {
		if strings.Contains(string(b), secret) {
			t.Fatal("the ledger holds a key or a request's text")
		}
	}
	for _, f := range []string{access.Path(), hub.Path()} {
		b, _ := os.ReadFile(f)
		for _, secret := range []string{aliceKey, bobKey, "a long password"} {
			if strings.Contains(string(b), secret) {
				t.Fatalf("%s holds a key or a password as given", filepath.Base(f))
			}
		}
	}
}

// A member in no group, or in a group that gives no model, is served
// nothing: no models is never every model.
func TestHubMemberWithNoModels(t *testing.T) {
	h := newHub(t)
	vendor(t, "plan", "m1", answers(30, 5))
	empty := h.group("empty")
	_, none := h.member(hub.MemberIn{Name: "nogroup"})
	_, hollow := h.member(hub.MemberIn{Name: "hollow", Groups: []string{empty}})
	for _, key := range []string{none, hollow} {
		if ids := modelIDs(t, h, key); len(ids) != 0 {
			t.Fatalf("a member with no models is listed %v", ids)
		}
		if status := h.post(key, "/v1/chat/completions", chatFor("plan/m1"), nil); status != 403 {
			t.Fatalf("a member with no models asked for one: %d, want 403", status)
		}
	}
}

// What a member may use is the member's now: a group's change, the member
// turned off and the member removed each hold on the next request.
func TestHubChangesHoldAtOnce(t *testing.T) {
	h := newHub(t)
	vendor(t, "plan", "m1", answers(30, 5))
	lab := h.group("lab", "plan/m1")
	id, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}})
	ask := func() int { return h.post(key, "/v1/chat/completions", chatFor("plan/m1"), nil) }
	if status := ask(); status != 200 {
		t.Fatalf("before any change: %d", status)
	}
	if _, err := hub.SaveGroup(hub.Group{ID: lab, Name: "lab", Models: []string{"plan/other"}}); err != nil {
		t.Fatal(err)
	}
	if status := ask(); status != 403 {
		t.Fatalf("after the group lost the model: %d, want 403", status)
	}
	if _, err := hub.SaveGroup(hub.Group{ID: lab, Name: "lab", Models: []string{"plan/*"}}); err != nil {
		t.Fatal(err)
	}
	if status := ask(); status != 200 {
		t.Fatalf("after the group got it back: %d", status)
	}
	if _, err := hub.SaveMember(hub.MemberIn{ID: id, Name: "alice", Groups: []string{lab}, Off: true}); err != nil {
		t.Fatal(err)
	}
	if status := ask(); status != 401 {
		t.Fatalf("a member turned off: %d, want 401", status)
	}
	if _, err := hub.SaveMember(hub.MemberIn{ID: id, Name: "alice", Groups: []string{lab}}); err != nil {
		t.Fatal(err)
	}
	if status := ask(); status != 200 {
		t.Fatalf("a member turned on again: %d", status)
	}
	if err := hub.RemoveMember(id); err != nil {
		t.Fatal(err)
	}
	if status := ask(); status != 401 {
		t.Fatalf("a member removed: %d, want 401", status)
	}
}

// Signing in: an invitation works once, a wrong password is refused, too
// many wrong ones are held off, and a device key can read but can neither
// make a device nor take one away.
func TestHubSignIn(t *testing.T) {
	h := newHub(t)
	id, err := hub.SaveMember(hub.MemberIn{Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	other, otherKey := h.member(hub.MemberIn{Name: "bob"})
	code, _, err := hub.Invite(id)
	if err != nil {
		t.Fatal(err)
	}
	if status := h.post("", "/relay/api/redeem", map[string]string{"code": code, "password": "short"}, nil); status != 400 {
		t.Fatalf("a short password: %d, want 400", status)
	}
	var signed struct{ Token string }
	if status := h.post("", "/relay/api/redeem", map[string]string{"code": code, "password": "a long password"}, &signed); status != 200 {
		t.Fatalf("redeeming: %d", status)
	}
	if status := h.post("", "/relay/api/redeem", map[string]string{"code": code, "password": "another password"}, nil); status != 400 {
		t.Fatalf("an invitation used twice: %d, want 400", status)
	}
	// a request without the page's header is no sign-in: another site's form can't make one
	req, _ := http.NewRequest("POST", h.url+"/relay/api/login", strings.NewReader(`{"name":"alice","password":"a long password"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("a sign-in without X-Relay: %d, want 400", res.StatusCode)
	}
	if status := h.post("", "/relay/api/login", map[string]string{"name": "alice", "password": "a long password"}, &signed); status != 200 || signed.Token == "" {
		t.Fatalf("signing in: %d", status)
	}
	var dev struct{ ID, Key string }
	if status := h.post(signed.Token, "/relay/api/devices", map[string]string{"name": "desk"}, &dev); status != 200 {
		t.Fatalf("a device: %d", status)
	}
	// a device key reads its member's page, and changes nothing
	if status := h.get(dev.Key, "/relay/api/me", nil); status != 200 {
		t.Fatalf("a device key on /me: %d", status)
	}
	if status := h.post(dev.Key, "/relay/api/devices", map[string]string{"name": "more"}, nil); status != 401 {
		t.Fatalf("a device key making a device: %d, want 401", status)
	}
	if status := h.post(dev.Key, "/relay/api/devices/revoke", map[string]string{"id": dev.ID}, nil); status != 401 {
		t.Fatalf("a device key taking a device away: %d, want 401", status)
	}
	// a member takes away only a device of its own
	bobs, _ := access.Devices(other)
	if status := h.post(signed.Token, "/relay/api/devices/revoke", map[string]string{"id": bobs[0].ID}, nil); status != 400 {
		t.Fatalf("alice taking bob's device away: %d, want 400", status)
	}
	if status := h.get(otherKey, "/relay/api/me", nil); status != 200 {
		t.Fatalf("bob's device after alice tried: %d", status)
	}
	// signed out, the session is over
	if status := h.post(signed.Token, "/relay/api/logout", map[string]string{}, nil); status != 200 {
		t.Fatalf("signing out: %d", status)
	}
	if status := h.get(signed.Token, "/relay/api/me", nil); status != 401 {
		t.Fatalf("a session after signing out: %d, want 401", status)
	}
	// wrong passwords are refused alike for a member and for no member, and held off
	for i := 0; i < 5; i++ {
		if status := h.post("", "/relay/api/login", map[string]string{"name": "alice", "password": "not the password"}, nil); status != 401 {
			t.Fatalf("a wrong password: %d, want 401", status)
		}
	}
	if status := h.post("", "/relay/api/login", map[string]string{"name": "alice", "password": "a long password"}, nil); status != 429 {
		t.Fatalf("the right password after five wrong ones: %d, want 429", status)
	}
	if status := h.post("", "/relay/api/login", map[string]string{"name": "nobody", "password": "not the password"}, nil); status != 401 {
		t.Fatalf("no such member: %d, want 401", status)
	}
}

// A streamed reply with a tool call reaches the member as its vendor sent
// it, chunk by chunk, and is settled with the usage the vendor reported.
func TestHubStreamsToolCalls(t *testing.T) {
	h := newHub(t)
	gate := make(chan struct{})
	vendor(t, "plan", "m1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`+"\n\n")
		f.Flush()
		<-gate // the rest only after the member has read the first of it
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":40,"completion_tokens":12}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	openGate := opener(t, gate)
	lab := h.group("lab", "plan/m1")
	id, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}})
	body := `{"model":"plan/m1","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"read a.go"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	req, _ := http.NewRequest("POST", h.url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("the stream's start: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var all strings.Builder
	first := false
	for sc.Scan() {
		line := sc.Text()
		all.WriteString(line + "\n")
		if !first && strings.Contains(line, "read_file") {
			first = true
			openGate() // read while the vendor still holds the rest: nothing is buffered to the end
		}
	}
	if !first {
		t.Fatalf("the tool call never came: %s", all.String())
	}
	got := all.String()
	if !strings.Contains(got, `call_1`) || !strings.Contains(got, `a.go`) || !strings.Contains(got, "tool_calls") || !strings.Contains(got, "[DONE]") {
		t.Fatalf("the stream as the member got it: %s", got)
	}
	st, err := hub.StatementOf(id, "today", time.Now(), false)
	if err != nil || st.Calls != 1 || st.Input != 40 || st.Output != 12 || st.Unknown != 0 {
		t.Fatalf("the streamed call's settlement: %+v %v", st.Totals, err)
	}
	if e := st.Recent[0]; e.Settle != hub.Settled || e.Status != 200 {
		t.Fatalf("the streamed call's entry: %+v", e)
	}
}

// A call answered with no usage reported is counted at what was reserved
// for it, and said to be: unknown is never counted as nothing.
func TestHubUnknownUsageIsEstimated(t *testing.T) {
	h := newHub(t)
	vendor(t, "plan", "m1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	lab := h.group("lab", "plan/m1")
	plan, err := hub.SavePlan(hub.Plan{Name: "Metered", Kind: hub.Metered, PriceIn: 2, PriceOut: 8})
	if err != nil {
		t.Fatal(err)
	}
	id, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}, Plan: plan})
	long := map[string]any{"model": "plan/m1", "messages": []map[string]string{{"role": "user", "content": strings.Repeat("word ", 400)}}}
	if status := h.post(key, "/v1/chat/completions", long, nil); status != 200 {
		t.Fatalf("the call: %d", status)
	}
	st, _ := hub.StatementOf(id, "today", time.Now(), false)
	if st.Calls != 1 || st.Unknown != 1 || st.Est < 400 || st.Input != 0 {
		t.Fatalf("a call with no usage reported: %+v", st.Totals)
	}
	e := st.Recent[0]
	if e.Settle != hub.Estimated || e.Est != st.Est {
		t.Fatalf("its entry: %+v", e)
	}
	if want := float64(st.Est) * 2 / 1e6; st.Charge < want*0.999 || st.Charge > want*1.001 {
		t.Fatalf("its charge: %v, want the reservation at the plan's input price, %v", st.Charge, want)
	}
}

// A vendor's error uses nothing and charges nothing: its reservation is
// released.
func TestHubFailureReleases(t *testing.T) {
	h := newHub(t)
	vendor(t, "plan", "m1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	lab := h.group("lab", "plan/m1")
	id, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}, Limit: &access.Limit{Period: "day", Tokens: 100000}})
	if status := h.post(key, "/v1/chat/completions", chatFor("plan/m1"), nil); status < 500 {
		t.Fatalf("the failing call: %d", status)
	}
	st, _ := hub.StatementOf(id, "today", time.Now(), false)
	if st.Est != 0 || st.Charge != 0 || st.Input != 0 || len(st.Recent) == 0 || st.Recent[0].Settle != hub.Released {
		t.Fatalf("a failed call: %+v %+v", st.Totals, st.Recent)
	}
	var me struct {
		Member hub.Standing
	}
	h.get(key, "/relay/api/me", &me)
	if me.Member.Limit == nil || me.Member.Limit.Tokens != 0 || me.Member.Limit.InFlight != 0 {
		t.Fatalf("the member's limit after a failed call: %+v", me.Member.Limit)
	}
	if b, _ := os.ReadFile(filepath.Join(settings.Dir(), "relay-open.json")); strings.Contains(string(b), id) {
		t.Fatal("a finished request is still among those in flight")
	}
}

// A member's limit holds over all its devices, and requests arriving
// together can't each take the last of it: the member is refused with a
// 429 that says when it resets, and another member goes on.
func TestHubMemberLimitUnderLoad(t *testing.T) {
	h := newHub(t)
	now := pinLimitClock(t)
	var calls atomic.Int64
	vendor(t, "plan", "m1", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		answers(300, 50)(w, r)
	})
	lab := h.group("lab", "plan/m1")
	// 350 tokens a call: the third call would start past 700
	id, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}, Concurrency: 16, Limit: &access.Limit{Period: "day", Tokens: 700}})
	_, bobKey := h.member(hub.MemberIn{Name: "bob", Groups: []string{lab}})
	// a second device of alice's: the limit is the member's, not a key's
	_, second, err := access.AddDevice(id, "alice-desk", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range []string{key, second} {
		if status := h.post(k, "/v1/chat/completions", chatFor("plan/m1"), nil); status != 200 {
			t.Fatalf("call %d within the limit: %d", i+1, status)
		}
	}
	before := calls.Load()
	var wg sync.WaitGroup
	var ok, refused atomic.Int64
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := key
			if i%2 == 1 {
				k = second
			}
			switch h.post(k, "/v1/chat/completions", chatFor("plan/m1"), nil) {
			case 200:
				ok.Add(1)
			case 429:
				refused.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 0 || refused.Load() != 12 || calls.Load() != before {
		t.Fatalf("12 requests together past the limit: %d served, %d refused, %d reached the vendor", ok.Load(), refused.Load(), calls.Load()-before)
	}
	req, _ := http.NewRequest("POST", h.url+"/v1/chat/completions", strings.NewReader(`{"model":"plan/m1","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	_, reset := access.Window("day", now)
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" || res.Header.Get("X-Magpie-Limit-Reset") != reset.Format(time.RFC3339) || !strings.Contains(string(b), `alice`) {
		t.Fatalf("the refusal: %d %v %s", res.StatusCode, res.Header, b)
	}
	if status := h.post(bobKey, "/v1/chat/completions", chatFor("plan/m1"), nil); status != 200 {
		t.Fatalf("another member while alice is past her limit: %d", status)
	}
	// what was used is read from the ledger again after a restart
	budget.Forget()
	if status := h.post(key, "/v1/chat/completions", chatFor("plan/m1"), nil); status != 429 {
		t.Fatalf("after a restart: %d, want 429", status)
	}
}

// With room for one more call, requests arriving together are let in only
// while what is used and what is reserved stay under the limit.
func TestHubReservationsHoldTheLimit(t *testing.T) {
	h := newHub(t)
	pinLimitClock(t)
	var calls atomic.Int64
	release := make(chan struct{})
	vendor(t, "plan", "m1", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		answers(300, 50)(w, r)
	})
	letGo := opener(t, release)
	lab := h.group("lab", "plan/m1")
	body := map[string]any{"model": "plan/m1", "messages": []map[string]string{{"role": "user", "content": strings.Repeat("word ", 400)}}}
	// each request is ~500 tokens of body: two reservations fill 1000
	_, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}, Concurrency: 32, Queue: 64, Limit: &access.Limit{Period: "day", Tokens: 1000}})
	var wg sync.WaitGroup
	var ok, refused atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch h.post(key, "/v1/chat/completions", body, nil) {
			case 200:
				ok.Add(1)
			case 429:
				refused.Add(1)
			}
		}()
	}
	// those refused are refused at once, with the others still in flight
	deadline := time.Now().Add(10 * time.Second)
	for refused.Load() < 18 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	letGo()
	wg.Wait()
	if ok.Load() != 2 || refused.Load() != 18 || calls.Load() != 2 {
		t.Fatalf("20 requests together with room for 2: %d served, %d refused, %d reached the vendor", ok.Load(), refused.Load(), calls.Load())
	}
}

// A member's requests run as many at once as the member may, the next
// wait their turn in order, and one more than may wait is told to come
// back: another member is not held up by it.
func TestHubMemberConcurrencyAndQueue(t *testing.T) {
	h := newHub(t)
	var running, most atomic.Int64
	release := make(chan struct{})
	vendor(t, "plan", "m1", func(w http.ResponseWriter, r *http.Request) {
		n := running.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		answers(10, 2)(w, r)
	})
	letGo := opener(t, release)
	lab := h.group("lab", "plan/m1")
	id, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}, Concurrency: 1, Queue: 1})
	_, bobKey := h.member(hub.MemberIn{Name: "bob", Groups: []string{lab}, Concurrency: 1, Queue: 1})
	results := make(chan int, 3)
	go func() { results <- h.post(key, "/v1/chat/completions", chatFor("plan/m1"), nil) }()
	waitFor(t, func() bool { return MemberLanes()[id].Busy == 1 && running.Load() == 1 })
	go func() { results <- h.post(key, "/v1/chat/completions", chatFor("plan/m1"), nil) }()
	waitFor(t, func() bool { return MemberLanes()[id].Waiting == 1 })
	// the queue is full: a third is told to come back, at once
	req, _ := http.NewRequest("POST", h.url+"/v1/chat/completions", strings.NewReader(`{"model":"plan/m1","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("a third request with one running and one waiting: %d", res.StatusCode)
	}
	// another member runs beside alice's
	go func() { results <- h.post(bobKey, "/v1/chat/completions", chatFor("plan/m1"), nil) }()
	waitFor(t, func() bool { return running.Load() == 2 })
	letGo()
	for i := 0; i < 3; i++ {
		if status := <-results; status != 200 {
			t.Fatalf("a request that ran or waited its turn: %d", status)
		}
	}
	if most.Load() != 2 {
		t.Fatalf("%d ran at once, want 2: one of alice's and bob's", most.Load())
	}
	waitFor(t, func() bool { l := MemberLanes()[id]; return l.Busy == 0 && l.Waiting == 0 })
}

// opener answers what closes ch, once: a test calls it where it lets its
// vendor go, and it is called again as the test ends, so a check that
// fails first never leaves a vendor, and the servers closing after it,
// waiting for ever.
func opener(t *testing.T, ch chan struct{}) func() {
	var once sync.Once
	open := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(open)
	return open
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("waited 10s for what never came")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A member's tool that goes away mid-stream frees the member's slot and
// its reservation, and the call is in the ledger as cut off, counted at
// what was reserved since its vendor never got to report.
func TestHubCancelFreesEverything(t *testing.T) {
	h := newHub(t)
	started := make(chan struct{})
	gone := make(chan struct{})
	vendor(t, "plan", "m1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"working"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // the hub hangs up on the vendor when its caller is gone
		close(gone)
	})
	lab := h.group("lab", "plan/m1")
	id, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}, Concurrency: 1, Queue: 1, Limit: &access.Limit{Period: "day", Tokens: 1 << 30}})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", h.url+"/v1/chat/completions", strings.NewReader(`{"model":"plan/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	buf := make([]byte, 64)
	res.Body.Read(buf)
	cancel()
	res.Body.Close()
	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Fatal("the vendor's call went on after the member went away")
	}
	waitFor(t, func() bool { return MemberLanes()[id].Busy == 0 })
	waitFor(t, func() bool {
		st, _ := hub.StatementOf(id, "today", time.Now(), false)
		return len(st.Recent) == 1
	})
	st, _ := hub.StatementOf(id, "today", time.Now(), false)
	e := st.Recent[0]
	if e.Status != 499 || e.Settle != hub.Estimated || e.Est == 0 && st.Unknown != 1 {
		t.Fatalf("the cut-off call: %+v", e)
	}
	var me struct {
		Member hub.Standing
	}
	h.get(key, "/relay/api/me", &me)
	if me.Member.Limit == nil || me.Member.Limit.InFlight != 0 || me.Member.Limit.Reserved != 0 {
		t.Fatalf("the member's reservations after the cut: %+v", me.Member.Limit)
	}
	if b, _ := os.ReadFile(filepath.Join(settings.Dir(), "relay-open.json")); strings.Contains(string(b), id) {
		t.Fatal("the cut-off request is still among those in flight")
	}
}

// A vendor that hangs up mid-stream leaves the member's slot free and the
// call in the ledger.
func TestHubVendorHangsUp(t *testing.T) {
	h := newHub(t)
	vendor(t, "plan", "m1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"half"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
			}
		}
	})
	lab := h.group("lab", "plan/m1")
	id, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}, Concurrency: 1})
	req, _ := http.NewRequest("POST", h.url+"/v1/chat/completions", strings.NewReader(`{"model":"plan/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	waitFor(t, func() bool { return MemberLanes()[id].Busy == 0 })
	waitFor(t, func() bool {
		st, _ := hub.StatementOf(id, "today", time.Now(), false)
		return len(st.Recent) == 1
	})
	st, _ := hub.StatementOf(id, "today", time.Now(), false)
	if e := st.Recent[0]; e.Settle == "" || e.Device != "alice-laptop" {
		t.Fatalf("the broken-off call: %+v", e)
	}
}

// A hub stopped in the middle of a request finds it when it next starts:
// it is in the ledger as interrupted, counted at what was reserved, and
// what the member had used before is still counted.
func TestHubRestartSettlesWhatWasInFlight(t *testing.T) {
	h := newHub(t)
	pinLimitClock(t)
	vendor(t, "plan", "m1", answers(300, 50))
	lab := h.group("lab", "plan/m1")
	id, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}, Limit: &access.Limit{Period: "day", Tokens: 1000}})
	if status := h.post(key, "/v1/chat/completions", chatFor("plan/m1"), nil); status != 200 {
		t.Fatalf("the call before the stop: %d", status)
	}
	// a request let in, and the hub gone before it ended
	who, ok := access.Authenticate(key)
	if !ok {
		t.Fatal("alice's key")
	}
	if hub.Begin(who, "plan/m1", "/v1/chat/completions", 600, time.Now()) == nil {
		t.Fatal("no request begun")
	}
	budget.Forget()
	hub.Forget()
	if n := hub.Recover(); n != 1 {
		t.Fatalf("%d requests found in flight, want 1", n)
	}
	if n := hub.Recover(); n != 0 {
		t.Fatalf("%d found again: one in flight is written once", n)
	}
	st, _ := hub.StatementOf(id, "today", time.Now(), false)
	if st.Calls != 2 || st.Interrupted != 1 || st.Est != 600 || st.Input != 300 {
		t.Fatalf("after the restart: %+v", st.Totals)
	}
	// 350 used and 600 interrupted: nothing more is let in under 1000 once a third is asked
	if status := h.post(key, "/v1/chat/completions", chatFor("plan/m1"), nil); status != 200 {
		t.Fatalf("950 of 1000 used, one more is let in: %d", status)
	}
	if status := h.post(key, "/v1/chat/completions", chatFor("plan/m1"), nil); status != 429 {
		t.Fatalf("past the limit with the interrupted call counted: %d, want 429", status)
	}
	// the key works across the restart, and one taken away stays away
	devs, _ := access.Devices(id)
	if err := access.RemoveDevice(id, devs[0].ID); err != nil {
		t.Fatal(err)
	}
	budget.Forget()
	hub.Forget()
	if status := h.get(key, "/v1/models", nil); status != 401 {
		t.Fatalf("a key taken away, after a restart: %d, want 401", status)
	}
}

// Two members who name the same session, with the same first words, keep
// their conversations apart: what the hub remembers of one is not the
// other's.
func TestHubSessionsAreTheCallers(t *testing.T) {
	h := newHub(t)
	vendor(t, "plan", "m1", answers(30, 5))
	lab := h.group("lab", "plan/m1")
	_, aliceKey := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}})
	_, bobKey := h.member(hub.MemberIn{Name: "bob", Groups: []string{lab}})
	for _, key := range []string{aliceKey, bobKey} {
		req, _ := http.NewRequest("POST", h.url+"/v1/chat/completions", strings.NewReader(`{"model":"plan/m1","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Session-Id", "the-same-session")
		req.Header.Set(SessionHeader, "the-same-session")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("the call: %d", res.StatusCode)
		}
	}
	var keys []string
	read := func() int {
		sticks.Lock()
		defer sticks.Unlock()
		keys = keys[:0]
		for k := range sticks.m {
			keys = append(keys, k)
		}
		return len(keys)
	}
	// who answered is kept once the reply has ended, after the caller has it
	for end := time.Now().Add(3 * time.Second); read() < 2 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	if len(keys) != 2 {
		t.Fatalf("two members on one session id are kept as %d conversation(s): %q", len(keys), keys)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "m:") || !strings.Contains(k, "the-same-session") {
			t.Fatalf("a conversation kept without its member: %q", k)
		}
	}
}

// Off a hub nothing is told apart by its caller: magpie's own gateway
// keeps a conversation by its session as it always did.
func TestScopeOnlyOnAHub(t *testing.T) {
	fresh(t)
	ctx := access.WithIdentity(context.Background(), access.Identity{KeyID: "k1"})
	if got := scoped(ctx, "plan/m1"); got != "plan/m1" {
		t.Fatalf("a gateway key's scope off a hub: %q", got)
	}
	t.Cleanup(mode.Set(mode.Server))
	if got := scoped(ctx, "plan/m1"); got == "plan/m1" {
		t.Fatal("a hub's own key shares its conversations with every caller")
	}
	if got := scoped(context.Background(), "plan/m1"); got != "plan/m1" {
		t.Fatalf("no caller: %q", got)
	}
}

// Which of the hub's accounts answers is the hub's to say: a member's
// device that names one is served as if it had named none, and a key of
// the hub's own still names one.
func TestHubMemberNamesNoAccount(t *testing.T) {
	h := newHub(t)
	vendor(t, "plan", "m1", answers(30, 5))
	lab := h.group("lab", "plan/m1")
	_, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}})
	seen := make(chan string, 2)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen <- r.Header.Get(AccountHeader) })
	guard := lanGuard(inner)
	ask := func(k string) string {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
		r.RemoteAddr = "192.168.1.9:5000"
		r.Header.Set("Authorization", "Bearer "+k)
		r.Header.Set(AccountHeader, "someone@example.com")
		guard.ServeHTTP(httptest.NewRecorder(), r)
		select {
		case v := <-seen:
			return v
		default:
			t.Fatal("the request was not let in")
			return ""
		}
	}
	if got := ask(key); got != "" {
		t.Fatalf("a member's device named the account %q", got)
	}
	own, err := access.Update("add-key", access.Change{Name: "the hub's own"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ask(own); got != "someone@example.com" {
		t.Fatalf("a key of the hub's own lost the account it named: %q", got)
	}
}

// A member's models hold on every API the hub serves, not chat alone: a
// model its groups don't give is refused on each, with its vendor never
// asked, and the one they do give is not refused for being a member's.
func TestHubModelsHoldOnEveryAPI(t *testing.T) {
	h := newHub(t)
	var mine, others atomic.Int64
	vendor(t, "plan", "m1", func(w http.ResponseWriter, r *http.Request) {
		mine.Add(1)
		answers(30, 5)(w, r)
	})
	vendor(t, "other", "m2", func(w http.ResponseWriter, r *http.Request) {
		others.Add(1)
		answers(7, 3)(w, r)
	})
	lab := h.group("lab", "plan/m1")
	_, key := h.member(hub.MemberIn{Name: "alice", Groups: []string{lab}})
	msgs := []map[string]string{{"role": "user", "content": "hi"}}
	calls := []struct {
		path string
		body func(model string) map[string]any
	}{
		{"/v1/chat/completions", func(m string) map[string]any { return map[string]any{"model": m, "messages": msgs} }},
		{"/chat/completions", func(m string) map[string]any { return map[string]any{"model": m, "messages": msgs} }},
		{"/v1/messages", func(m string) map[string]any { return map[string]any{"model": m, "max_tokens": 16, "messages": msgs} }},
		{"/v1/messages/count_tokens", func(m string) map[string]any { return map[string]any{"model": m, "messages": msgs} }},
		{"/v1/responses", func(m string) map[string]any { return map[string]any{"model": m, "input": "hi"} }},
		{"/v1/embeddings", func(m string) map[string]any { return map[string]any{"model": m, "input": "hi"} }},
		{"/v1/rerank", func(m string) map[string]any {
			return map[string]any{"model": m, "query": "hi", "documents": []string{"a", "b"}}
		}},
		{"/v1/images/generations", func(m string) map[string]any { return map[string]any{"model": m, "prompt": "a bird"} }},
		{"/v1/videos", func(m string) map[string]any { return map[string]any{"model": m, "prompt": "a bird"} }},
		{"/v1/systemone", func(m string) map[string]any { return map[string]any{"model": m, "messages": msgs} }},
		{"/v1beta/models/%s:generateContent", func(m string) map[string]any {
			return map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": "hi"}}}}}
		}},
		{"/backend-api/codex/responses", func(m string) map[string]any { return map[string]any{"model": m, "input": "hi"} }},
	}
	for _, c := range calls {
		path := c.path
		if strings.Contains(path, "%s") {
			path = fmt.Sprintf(path, "other/m2")
		}
		var out string
		status := h.post(key, path, c.body("other/m2"), &out)
		if others.Load() != 0 {
			t.Fatalf("%s reached a vendor the member's groups don't give (%d %s)", c.path, status, out)
		}
		if status < 400 {
			t.Errorf("%s for a model the member may not use: %d %s", c.path, status, out)
		}
	}
	// and with no key at all, none of them is answered
	for _, c := range calls {
		path := c.path
		if strings.Contains(path, "%s") {
			path = fmt.Sprintf(path, "plan/m1")
		}
		if status := h.post("", path, c.body("plan/m1"), nil); status != 401 {
			t.Errorf("%s with no key: %d, want 401", c.path, status)
		}
	}
	if others.Load() != 0 || mine.Load() != 0 {
		t.Fatalf("a vendor was asked: %d of the member's, %d of another's", mine.Load(), others.Load())
	}
	// the member's own model is still served
	var out string
	if status := h.post(key, "/v1/chat/completions", map[string]any{"model": "plan/m1", "messages": msgs}, &out); status != 200 {
		t.Errorf("the member's own model: %d %s", status, out)
	}
	if mine.Load() != 1 || others.Load() != 0 {
		t.Fatalf("the member's vendor was asked %d times, another's %d", mine.Load(), others.Load())
	}
}
