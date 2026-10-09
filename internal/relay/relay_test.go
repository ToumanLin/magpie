package relay

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/testenv"
	"github.com/yetone/magpie/internal/usage"
)

// The tests run in a home of their own, never the user's real files.
func TestMain(m *testing.M) { testenv.Main(m) }

// fresh gives a test a folder of its own, with nothing kept from another.
func fresh(t *testing.T) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	Forget()
	budget.Forget()
	was := PassRounds
	PassRounds = 1000
	t.Cleanup(func() { PassRounds = was })
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// A member's models are its groups' together, and a group's change is the
// member's at once; a member in no group has none.
func TestModelsAreTheGroups(t *testing.T) {
	fresh(t)
	lab := must[string](t)(SaveGroup(Group{Name: "Lab", Models: []string{"group/coding", " anthropic/* ", "group/coding"}}))
	private := must[string](t)(SaveGroup(Group{Name: "private", Models: []string{"other/m2"}}))
	if lab != "lab" || private != "private" {
		t.Fatalf("group ids: %q %q", lab, private)
	}
	both := must[string](t)(SaveMember(MemberIn{Name: "Alice", Groups: []string{private, lab, lab}}))
	none := must[string](t)(SaveMember(MemberIn{Name: "nobody"}))
	who, ok := memberOf(access.Key{Member: both})
	if !ok || who.MemberName != "alice" || strings.Join(who.Models, ",") != "group/coding,anthropic/*,other/m2" {
		t.Fatalf("alice: %+v %v", who, ok)
	}
	if who, ok := memberOf(access.Key{Member: none}); !ok || len(who.Models) != 0 {
		t.Fatalf("a member in no group: %+v %v", who, ok)
	}
	if err := RemoveGroup(lab); err != nil {
		t.Fatal(err)
	}
	if who, _ := memberOf(access.Key{Member: both}); strings.Join(who.Models, ",") != "other/m2" {
		t.Fatalf("after the group went: %v", who.Models)
	}
	if _, ok := memberOf(access.Key{Member: "gone"}); ok {
		t.Fatal("a member that isn't is resolved")
	}
	if _, err := SaveMember(MemberIn{Name: "carol", Groups: []string{"nosuch"}}); err == nil {
		t.Fatal("a member was put in a group that isn't")
	}
	if _, err := SaveMember(MemberIn{Name: "alice"}); err == nil {
		t.Fatal("two members took one name")
	}
	for _, bad := range []string{"", "a", "has space", "UPPER/slash", strings.Repeat("x", 41)} {
		if _, err := SaveMember(MemberIn{Name: bad}); err == nil {
			t.Fatalf("the name %q was taken", bad)
		}
	}
}

// A state that doesn't parse lets no member in and is never written over.
func TestUnreadableStateLetsNobodyIn(t *testing.T) {
	fresh(t)
	id := must[string](t)(SaveMember(MemberIn{Name: "alice"}))
	if _, ok := memberOf(access.Key{Member: id}); !ok {
		t.Fatal("alice before")
	}
	if err := os.WriteFile(Path(), []byte(`{"members": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	Forget()
	if _, ok := memberOf(access.Key{Member: id}); ok {
		t.Fatal("a member was let in on a state that doesn't parse")
	}
	if _, err := SaveMember(MemberIn{Name: "bob"}); err == nil {
		t.Fatal("a state that doesn't parse was written over")
	}
	if b, _ := os.ReadFile(Path()); string(b) != `{"members": [` {
		t.Fatalf("the file was changed: %s", b)
	}
}

// A plan's limit is the member's unless the administrator set the member
// one of their own; a metered plan charges by its prices and a period plan
// by its window.
func TestPlansLimitsAndCharges(t *testing.T) {
	fresh(t)
	period := must[string](t)(SavePlan(Plan{Name: "Lab monthly", Kind: Period, Price: 10, Tokens: 200000, Concurrency: 3}))
	metered := must[string](t)(SavePlan(Plan{Name: "Pay as you go", Kind: Metered, PriceIn: 3, PriceOut: 15, PriceCacheRead: 0.3, Budget: 5, Window: "week", Price: 99, Tokens: 7}))
	if _, err := SavePlan(Plan{Name: "odd", Kind: "free"}); err == nil {
		t.Fatal("a plan of no kind was taken")
	}
	if _, err := SavePlan(Plan{Name: "neg", Kind: Metered, PriceIn: -1}); err == nil {
		t.Fatal("a negative price was taken")
	}
	a := must[string](t)(SaveMember(MemberIn{Name: "alice", Plan: period}))
	b := must[string](t)(SaveMember(MemberIn{Name: "bob", Plan: metered, Concurrency: 5}))
	c := must[string](t)(SaveMember(MemberIn{Name: "carol", Plan: period, Limit: &access.Limit{Period: "day", Tokens: 50}}))
	d := must[string](t)(SaveMember(MemberIn{Name: "dave"}))

	who, _ := memberOf(access.Key{Member: a})
	if l := who.MemberLimit; l == nil || l.Period != "month" || l.Tokens != 200000 || who.Concurrency != 3 || who.Queue != DefaultQueue || who.ChargeRate != 0 {
		t.Fatalf("alice on the period plan: %+v %+v", who.MemberLimit, who)
	}
	who, _ = memberOf(access.Key{Member: b})
	if l := who.MemberLimit; l == nil || l.Period != "week" || l.Charge != 5 || l.Tokens != 0 || who.Concurrency != 5 || who.ChargeRate != 3e-6 {
		t.Fatalf("bob on the metered plan: %+v %+v", who.MemberLimit, who)
	}
	who, _ = memberOf(access.Key{Member: c})
	if l := who.MemberLimit; l == nil || l.Period != "day" || l.Tokens != 50 {
		t.Fatalf("carol's own limit: %+v", who.MemberLimit)
	}
	who, _ = memberOf(access.Key{Member: d})
	if who.MemberLimit != nil || who.Concurrency != DefaultConcurrency || who.Queue != DefaultQueue {
		t.Fatalf("dave with no plan: %+v", who)
	}

	mp, _ := PlanOf(metered)
	if mp.Price != 0 || mp.Tokens != 0 {
		t.Fatalf("a metered plan kept a period's price: %+v", mp)
	}
	rec := usage.Record{Input: 1_000_000, Output: 100_000, CacheRead: 1_000_000}
	if got := mp.Charge(rec); got < 4.799 || got > 4.801 {
		t.Fatalf("the metered charge: %v, want 3 + 1.5 + 0.3", got)
	}
	if got := mp.Charge(usage.Record{UsageUnknown: true, Est: 500_000}); got < 1.499 || got > 1.501 {
		t.Fatalf("a call with no usage reported is charged %v, want its reservation at the input price", got)
	}
	pp, _ := PlanOf(period)
	if got := pp.Charge(rec); got != 0 {
		t.Fatalf("a period plan charged a call %v", got)
	}
	if err := RemovePlan(period); err == nil {
		t.Fatal("a plan members are on was removed")
	}
}

// An invitation is one member's, works once and for a while, and a new one
// replaces the last; nothing of the code, the password or the session is
// kept as given.
func TestInviteRedeemSignIn(t *testing.T) {
	fresh(t)
	id := must[string](t)(SaveMember(MemberIn{Name: "alice"}))
	if _, _, err := Invite("nobody"); err == nil {
		t.Fatal("an invitation for no member")
	}
	old, _, err := Invite(id)
	if err != nil {
		t.Fatal(err)
	}
	code, exp, err := Invite(id)
	if err != nil || !strings.HasPrefix(code, InvitePrefix) || time.Until(exp) < InviteLife-time.Minute {
		t.Fatalf("the invitation: %v %v", exp, err)
	}
	if _, _, err := Redeem(old, "a long password", "1.1.1.1"); err == nil {
		t.Fatal("an invitation a newer one replaced was taken")
	}
	if _, _, err := Login("alice", "a long password"); err == nil {
		t.Fatal("a member with no password yet signed in")
	}
	token, m, err := Redeem(code, "a long password", "2.2.2.2")
	if err != nil || m.Name != "alice" || m.PassHash != "" || !strings.HasPrefix(token, SessionPrefix) {
		t.Fatalf("redeeming: %+v %v", m, err)
	}
	if got, ok := SessionMember(token); !ok || got.ID != id || got.PassHash != "" {
		t.Fatalf("the session: %+v %v", got, ok)
	}
	if _, _, err := Redeem(code, "yet another password", "4.4.4.4"); err == nil {
		t.Fatal("an invitation was used twice")
	}
	if _, ok := SessionMember(token); !ok {
		t.Fatal("a second try of a used invitation ended the member's sign-in")
	}
	b, _ := os.ReadFile(Path())
	for _, secret := range []string{code, old, token, "a long password"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("relay.json holds a code, a session or a password as given")
		}
	}
	second, _, err := Login("Alice ", "a long password")
	if err != nil {
		t.Fatalf("signing in: %v", err)
	}
	if err := ChangePassword(id, "not it", "another password", second); err == nil {
		t.Fatal("a password changed without the old one")
	}
	if err := ChangePassword(id, "a long password", "another password", second); err != nil {
		t.Fatal(err)
	}
	if _, ok := SessionMember(token); ok {
		t.Fatal("a sign-in made with the old password outlived it")
	}
	if _, ok := SessionMember(second); !ok {
		t.Fatal("the sign-in that changed the password was ended")
	}
	if err := Logout(second); err != nil {
		t.Fatal(err)
	}
	if _, ok := SessionMember(second); ok {
		t.Fatal("a session after signing out")
	}
	// a member turned off is signed out and can't sign in
	third, _, err := Login("alice", "another password")
	if err != nil {
		t.Fatal(err)
	}
	must[string](t)(SaveMember(MemberIn{ID: id, Name: "alice", Off: true}))
	if _, ok := SessionMember(third); ok {
		t.Fatal("a member turned off is still signed in")
	}
	if _, _, err := Login("alice", "another password"); err == nil {
		t.Fatal("a member turned off signed in")
	}
}

// An invitation past its time is no invitation.
func TestInviteRunsOut(t *testing.T) {
	fresh(t)
	id := must[string](t)(SaveMember(MemberIn{Name: "alice"}))
	code, _, err := Invite(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := change(func(st *State) error {
		st.Invites[0].Expires = time.Now().Add(-time.Second).Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Redeem(code, "a long password", "3.3.3.3"); err == nil {
		t.Fatal("an invitation past its time was taken")
	}
}

// Wrong tries are held off: five for a name, five from an address.
func TestWrongTriesAreHeldOff(t *testing.T) {
	fresh(t)
	id := must[string](t)(SaveMember(MemberIn{Name: "alice"}))
	code, _, _ := Invite(id)
	for i := 0; i < throttleTries; i++ {
		if _, _, err := Redeem("inv-wrong", "a long password", "9.9.9.9"); err == nil || err == ErrThrottled {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	if _, _, err := Redeem(code, "a long password", "9.9.9.9"); err != ErrThrottled {
		t.Fatalf("the right code after five wrong from one address: %v", err)
	}
	// from elsewhere the invitation still works, and a short password keeps it
	if _, _, err := Redeem(code, "short", "8.8.8.8"); err == nil {
		t.Fatal("a short password was taken")
	}
	if _, _, err := Redeem(code, "a long password", "8.8.8.8"); err != nil {
		t.Fatalf("the invitation after a short password: %v", err)
	}
}

// A member's statement is of that member's records alone, and the
// administrator's overview adds up the same ones.
func TestStatementsAreOfOneLedger(t *testing.T) {
	fresh(t)
	plan := must[string](t)(SavePlan(Plan{Name: "monthly", Kind: Period, Price: 10, Tokens: 1000}))
	a := must[string](t)(SaveMember(MemberIn{Name: "alice", Plan: plan}))
	b := must[string](t)(SaveMember(MemberIn{Name: "bob"}))
	now := time.Now()
	for _, r := range []usage.Record{
		{Time: now, Member: a, MemberName: "alice", CallerKeyID: "k1", CallerKeyName: "laptop", Provider: "p", Model: "m1", Requested: "group/coding", Input: 100, Output: 10, Status: 200, Settle: Settled},
		{Time: now, Member: a, MemberName: "alice", CallerKeyID: "k2", CallerKeyName: "desk", Provider: "p", Model: "m1", Requested: "group/coding", Status: 200, Settle: Estimated, UsageUnknown: true, Est: 40},
		{Time: now, Member: a, MemberName: "alice", CallerKeyID: "k1", CallerKeyName: "laptop", Requested: "other/m2", Status: 403, Rejected: true, Settle: Released},
		{Time: now, Member: b, MemberName: "bob", CallerKeyID: "k3", CallerKeyName: "phone", Provider: "p", Model: "m2", Requested: "other/m2", Input: 7, Output: 3, Status: 200, Settle: Settled, Charge: 0.5,
			Error: "vendor said: account bob@example.com", ProviderAccount: "pool@example.com"},
		{Time: now, CallerKeyID: "k9", Provider: "p", Model: "m1", Input: 999, Status: 200}, // no member's
	} {
		budget.Append(r)
	}
	st, err := StatementOf(a, "today", now, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Calls != 2 || st.Refused != 1 || st.Input != 100 || st.Output != 10 || st.Unknown != 1 || st.Est != 40 || st.Fee != 10 || st.Due != 10 || st.Cost != 0 {
		t.Fatalf("alice's statement: %+v fee %v", st.Totals, st.Fee)
	}
	if len(st.ByDevice) != 2 || len(st.ByModel) != 2 || len(st.Recent) != 3 {
		t.Fatalf("alice's lines: %d devices, %d models, %d recent", len(st.ByDevice), len(st.ByModel), len(st.Recent))
	}
	bs, _ := StatementOf(b, "today", now, false)
	if bs.Calls != 1 || bs.Input != 7 || bs.Charge != 0.5 || len(bs.Recent) != 1 {
		t.Fatalf("bob's statement: %+v", bs.Totals)
	}
	// a member is shown the call, not the account behind it or the vendor's words
	e := bs.Recent[0]
	if e.Device != "phone" || e.Model != "other/m2" {
		t.Fatalf("bob's entry: %+v", e)
	}
	lines, all := Overview("today", now)
	if len(lines) != 2 || all.Calls != 3 || all.Input != 107 || all.Refused != 1 || all.Est != 40 {
		t.Fatalf("the overview: %+v %+v", lines, all)
	}
	if _, err := StatementOf("", "today", now, false); err == nil {
		t.Fatal("a statement of no member")
	}
	// the member's limit counts what the statement counts: tokens used and those estimated
	stand, ok := StandingOf(a, now)
	if !ok || stand.Limit == nil || stand.Limit.Tokens != 150 || stand.Limit.TokenLimit != 1000 {
		t.Fatalf("alice's standing: %+v", stand.Limit)
	}
}

// How a call's reservation ended is said by what it came to.
func TestStampSettles(t *testing.T) {
	fresh(t)
	plan := must[string](t)(SavePlan(Plan{Name: "payg", Kind: Metered, PriceIn: 2, PriceOut: 4}))
	id := must[string](t)(SaveMember(MemberIn{Name: "alice", Plan: plan}))
	who, _ := memberOf(access.Key{Member: id})
	who.KeyID, who.KeyName = "k1", "laptop"
	open := Begin(who, "p/m", "/v1/chat/completions", 1000, time.Now())
	if open == nil {
		t.Fatal("nothing begun")
	}
	for _, c := range []struct {
		name   string
		rec    usage.Record
		settle string
		est    int64
		charge float64
	}{
		{"usage reported", usage.Record{Provider: "p", Status: 200, Input: 500_000, Output: 250_000}, Settled, 0, 2},
		{"answered, none reported", usage.Record{Provider: "p", Status: 200}, Estimated, 1000, 0.002},
		{"a second record of the same request", usage.Record{Provider: "p", Status: 200}, Estimated, 0, 0},
		{"refused here", usage.Record{Status: 429, Rejected: true}, Released, 0, 0},
		{"the vendor's error", usage.Record{Provider: "p", Status: 500}, Released, 0, 0},
		{"cut off with usage", usage.Record{Provider: "p", Status: 499, Input: 1_000_000}, Settled, 0, 2},
	} {
		rec := c.rec
		Stamp(who, open, &rec)
		if rec.Member != id || rec.Plan != plan || rec.Settle != c.settle || rec.Est != c.est || rec.Charge < c.charge*0.999 || rec.Charge > c.charge*1.001+1e-12 {
			t.Errorf("%s: settle %q est %d charge %v", c.name, rec.Settle, rec.Est, rec.Charge)
		}
	}
	// cut off after its vendor had it, with nothing reported: counted at the reservation
	other := Begin(who, "p/m", "/v1/chat/completions", 300, time.Now())
	rec := usage.Record{Provider: "p", Status: 499}
	Stamp(who, other, &rec)
	if rec.Settle != Estimated || rec.Est != 300 || !rec.UsageUnknown {
		t.Errorf("cut off with nothing reported: %+v", rec)
	}
	// a record of a caller that is no member's is left as it is
	plain := usage.Record{Status: 200}
	Stamp(access.Identity{KeyID: "k"}, nil, &plain)
	if plain.Member != "" || plain.Settle != "" {
		t.Errorf("a record of no member was stamped: %+v", plain)
	}
	open.Close()
	other.Close()
	if n := Recover(); n != 0 {
		t.Fatalf("%d requests left in flight after both ended", n)
	}
}

// A period plan's price is for each of its windows the statement touches,
// none from before the member was made.
func TestFeeCountsWindows(t *testing.T) {
	loc := time.FixedZone("x", 0)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, loc)
	for _, c := range []struct {
		period  string
		since   time.Time
		created time.Time
		want    int
	}{
		{"month", time.Date(2026, 10, 1, 0, 0, 0, 0, loc), time.Time{}, 1},
		{"month", time.Date(2026, 8, 15, 0, 0, 0, 0, loc), time.Time{}, 3},
		{"month", time.Time{}, time.Date(2026, 9, 20, 0, 0, 0, 0, loc), 2},
		{"day", time.Date(2026, 10, 2, 0, 0, 0, 0, loc), time.Time{}, 7},
		{"week", time.Date(2026, 9, 9, 0, 0, 0, 0, loc), time.Date(2026, 9, 30, 0, 0, 0, 0, loc), 2},
		{"month", time.Time{}, time.Time{}, 1},
	} {
		var created int64
		if !c.created.IsZero() {
			created = c.created.Unix()
		}
		if got := windows(c.period, c.since, created, now); got != c.want {
			t.Errorf("%s windows from %v (made %v): %d, want %d", c.period, c.since, c.created, got, c.want)
		}
	}
}

// Behind a proxy the administrator says to trust, wrong tries are counted
// by the client the proxy names; otherwise by the address that connected,
// whatever a header says.
func TestFromBehindATrustedProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/relay/api/redeem", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	t.Setenv("MAGPIE_TRUST_PROXY", "")
	if got := from(r); got != "127.0.0.1" {
		t.Fatalf("with no proxy trusted, a header named the caller: %q", got)
	}
	t.Setenv("MAGPIE_TRUST_PROXY", "1")
	if got := from(r); got != "203.0.113.7" {
		t.Fatalf("behind a trusted proxy: %q", got)
	}
	r.Header.Del("X-Forwarded-For")
	if got := from(r); got != "127.0.0.1" {
		t.Fatalf("a trusted proxy that names nobody: %q", got)
	}
}
