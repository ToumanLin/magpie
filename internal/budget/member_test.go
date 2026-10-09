package budget

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/testenv"
	"github.com/yetone/magpie/internal/usage"
)

// The tests run in a home of their own, never the user's real files.
func TestMain(m *testing.M) { testenv.Main(m) }

func home(t *testing.T) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	Forget()
}

var noon = time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)

// A member's limit is counted over every device of the member's, beside
// each key's own, and requests arriving together are let in only while
// what is used and reserved stays under it.
func TestMemberLimitOverDevices(t *testing.T) {
	home(t)
	l := &access.Limit{Period: "day", Tokens: 1000}
	laptop := access.Identity{KeyID: "k1", KeyName: "laptop", Member: "m1", MemberName: "alice", MemberLimit: l}
	desk := access.Identity{KeyID: "k2", KeyName: "desk", Member: "m1", MemberName: "alice", MemberLimit: l}
	bob := access.Identity{KeyID: "k3", KeyName: "phone", Member: "m2", MemberName: "bob", MemberLimit: l}

	h1, refused := ReserveFor(laptop, 1600, "p/m", noon)
	if refused != nil || h1.Est != 400 {
		t.Fatalf("the first: %+v %v", h1, refused)
	}
	h2, refused := ReserveFor(desk, 1600, "p/m", noon)
	if refused != nil {
		t.Fatalf("the second, on another device: %v", refused)
	}
	h3, refused := ReserveFor(desk, 1600, "p/m", noon)
	if refused != nil {
		t.Fatalf("the third, 800 reserved of 1000: %v", refused)
	}
	if _, refused := ReserveFor(laptop, 1600, "p/m", noon); refused == nil || refused.Member != "alice" || refused.Spent || refused.InFlight != 3 {
		t.Fatalf("a fourth with 1200 reserved of 1000: %+v", refused)
	}
	if _, refused := ReserveFor(bob, 1600, "p/m", noon); refused != nil {
		t.Fatalf("another member: %v", refused)
	}
	// settled: what was used counts, what was reserved is given back
	Append(usage.Record{Time: noon.Add(time.Minute), CallerKeyID: "k1", Member: "m1", Provider: "p", Status: 200, Input: 300, Output: 50})
	h1.Release()
	h1.Release() // twice is once
	st := OfMember("m1", l, noon)
	if st.Tokens != 350 || st.InFlight != 2 || st.Reserved < 800 {
		t.Fatalf("after one settled: %+v", st)
	}
	h2.Release()
	h3.Release()
	// a call with no usage reported counts at its reservation; an interrupted one too
	Append(usage.Record{Time: noon.Add(time.Minute), CallerKeyID: "k2", Member: "m1", Provider: "p", Status: 200, UsageUnknown: true, Est: 400, Settle: "estimated"})
	Append(usage.Record{Time: noon.Add(time.Minute), CallerKeyID: "k2", Member: "m1", Settle: "interrupted", Est: 300})
	// a refusal and another member's call count nothing of alice's
	Append(usage.Record{Time: noon.Add(time.Minute), CallerKeyID: "k1", Member: "m1", Status: 429, Rejected: true})
	Append(usage.Record{Time: noon.Add(time.Minute), CallerKeyID: "k3", Member: "m2", Provider: "p", Status: 200, Input: 5000})
	if st := OfMember("m1", l, noon); st.Tokens != 1050 || !st.Spent || st.InFlight != 0 {
		t.Fatalf("with the unknown and the interrupted counted: %+v", st)
	}
	if _, refused := ReserveFor(desk, 10, "p/m", noon); refused == nil || !refused.Spent {
		t.Fatalf("past the limit: %+v", refused)
	}
	// read again from the ledger, as after a restart
	Forget()
	if st := OfMember("m1", l, noon); st.Tokens != 1050 {
		t.Fatalf("read from the ledger: %+v", st)
	}
	if _, refused := ReserveFor(laptop, 10, "p/m", noon); refused == nil {
		t.Fatal("let in after a restart, past the limit")
	}
	if OfMember("m1", nil, noon) != nil || OfMember("", l, noon) != nil {
		t.Fatal("a status with no limit or no member")
	}
}

// A device with a limit of its own is held to both: whichever is reached
// first refuses, and a refusal reserves against neither.
func TestKeyAndMemberLimitsTogether(t *testing.T) {
	home(t)
	who := access.Identity{KeyID: "k1", KeyName: "laptop", Limit: &access.Limit{Period: "day", Tokens: 300},
		Member: "m1", MemberName: "alice", MemberLimit: &access.Limit{Period: "day", Tokens: 100000}}
	h, refused := ReserveFor(who, 1600, "p/m", noon)
	if refused != nil {
		t.Fatal(refused)
	}
	if _, refused := ReserveFor(who, 1600, "p/m", noon); refused == nil || refused.Member != "" || refused.Key != "laptop" {
		t.Fatalf("the key's own limit reached first: %+v", refused)
	}
	// the refused one left nothing reserved against the member
	if st := OfMember("m1", who.MemberLimit, noon); st.InFlight != 1 || st.Reserved != 400 {
		t.Fatalf("the member's reservations after a refusal: %+v", st)
	}
	h.Release()
	if st := OfMember("m1", who.MemberLimit, noon); st.InFlight != 0 || st.Reserved != 0 {
		t.Fatalf("after release: %+v", st)
	}
}

// A cap on what the plan charges holds as tokens' does.
func TestChargeCap(t *testing.T) {
	home(t)
	l := &access.Limit{Period: "month", Charge: 1}
	who := access.Identity{KeyID: "k1", Member: "m1", MemberName: "alice", MemberLimit: l, ChargeRate: 2e-6}
	Append(usage.Record{Time: noon, CallerKeyID: "k1", Member: "m1", Provider: "p", Status: 200, Input: 100, Charge: 0.6})
	h, refused := ReserveFor(who, 400_000, "p/m", noon) // 100k tokens at $2/M: 0.2 reserved
	if refused != nil {
		t.Fatal(refused)
	}
	if st := OfMember("m1", l, noon); st.Charge != 0.6 || st.ReservedCharge < 0.199 || st.ChargeLeft < 0.399 || st.Spent {
		t.Fatalf("status: %+v", st)
	}
	h2, refused := ReserveFor(who, 400_000, "p/m", noon)
	if refused != nil {
		t.Fatal(refused)
	}
	if _, refused := ReserveFor(who, 400_000, "p/m", noon); refused == nil || refused.Member != "alice" {
		t.Fatalf("0.6 charged and 0.4 reserved of 1: %+v", refused)
	}
	h.Release()
	h2.Release()
	Append(usage.Record{Time: noon, CallerKeyID: "k1", Member: "m1", Provider: "p", Status: 200, Input: 100, Charge: 0.5})
	if _, refused := ReserveFor(who, 4, "p/m", noon); refused == nil || !refused.Spent {
		t.Fatalf("1.1 charged of 1: %+v", refused)
	}
}

// Many at once: never more let in than the limit has room for.
func TestReservationsUnderLoad(t *testing.T) {
	home(t)
	who := access.Identity{KeyID: "k1", Member: "m1", MemberName: "alice", MemberLimit: &access.Limit{Period: "day", Tokens: 1000}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	in := 0
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, refused := ReserveFor(who, 400, "p/m", noon); refused == nil { // 100 tokens each
				mu.Lock()
				in++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if in != 10 {
		t.Fatalf("%d let in, want 10: 100 tokens each of 1000", in)
	}
}
