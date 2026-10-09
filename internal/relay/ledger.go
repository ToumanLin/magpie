package relay

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

// The ledger is usage.jsonl: one record for each call, with the member,
// the plan and what the plan charges written on it as it is appended, so
// a member's page and the administrator's totals add up the same rows.
//
// A member's request goes through three steps:
//
//   - Reserved. Before any provider is asked, package budget checks the
//     member's limit and holds an estimate of the request against it, and
//     the request is written to relay-open.json (Open).
//   - Settled. The call's record says how the reservation ended (Stamp,
//     the record's Settle): "settled" with the usage the vendor reported;
//     "estimated" when the call was answered, or cut off after its vendor
//     was reached, and reported no usage — the tokens reserved are counted
//     and charged in its place, so unknown is never free; "released" when
//     it failed with nothing used (refused here, or an error from the
//     vendor).
//   - Released. When the request ends its reservation is given back and
//     it leaves relay-open.json (Close), whatever happened to it.
//
// A hub stopped in the middle of a request writes no record for it. What
// relay-open.json still holds when the hub next starts is written to the
// ledger then (Recover) as "interrupted", counted at the reservation too.
//
// No request or reply text is written anywhere by this package: a record
// has the model, the tokens, the time and the status.

// How a reservation ended (usage.Record.Settle).
const (
	Settled     = "settled"
	Estimated   = "estimated"
	Released    = "released"
	Interrupted = "interrupted"
)

// open is a member's request in flight, as relay-open.json keeps it.
type open struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"t"`
	Member   string    `json:"member"`
	Name     string    `json:"name"`
	KeyID    string    `json:"key"`
	KeyName  string    `json:"keyName"`
	Plan     string    `json:"plan,omitempty"`
	Model    string    `json:"model,omitempty"`
	Endpoint string    `json:"ep,omitempty"`
	Est      int64     `json:"est"`
}

// Open is a member's request let in: what its record is stamped by, and
// what closes it.
type Open struct {
	open
	mu sync.Mutex
	// charged: the reservation has stood in for a record's unknown usage
	// once, and doesn't again for another record of the same request
	charged bool
}

func openPath() string { return filepath.Join(settings.Dir(), "relay-open.json") }

var journal struct {
	sync.Mutex
	path string
	m    map[string]open
}

// journalLoad reads relay-open.json into memory the first time, or when
// the folder it is in has changed (a test's). Called with journal held.
func journalLoad() {
	p := openPath()
	if journal.m != nil && journal.path == p {
		return
	}
	journal.path, journal.m = p, map[string]open{}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var list []open
	if json.Unmarshal(b, &list) != nil {
		return
	}
	for _, o := range list {
		journal.m[o.ID] = o
	}
}

func journalSave() {
	list := make([]open, 0, len(journal.m))
	for _, o := range journal.m {
		list = append(list, o)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Time.Before(list[j].Time) })
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(journal.path), 0o755); err != nil {
		return
	}
	_ = edit.WriteAtomic(journal.path, append(b, '\n'))
}

// Begin notes a member's request as in flight, est tokens reserved for it;
// nil for a request no member made.
func Begin(who access.Identity, model, endpoint string, est int64, now time.Time) *Open {
	if who.Member == "" {
		return nil
	}
	id, err := random(8)
	if err != nil {
		return nil
	}
	o := &Open{open: open{ID: id, Time: now, Member: who.Member, Name: who.MemberName, KeyID: who.KeyID, KeyName: who.KeyName,
		Plan: who.Plan, Model: model, Endpoint: endpoint, Est: est}}
	journal.Lock()
	journalLoad()
	journal.m[id] = o.open
	journalSave()
	journal.Unlock()
	return o
}

// Close takes the request out of those in flight, when it has ended.
func (o *Open) Close() {
	if o == nil {
		return
	}
	journal.Lock()
	journalLoad()
	if _, ok := journal.m[o.ID]; ok {
		delete(journal.m, o.ID)
		journalSave()
	}
	journal.Unlock()
}

// Recover writes to the ledger the requests a hub that was stopped left in
// flight, each as interrupted and counted at what was reserved for it, and
// returns how many there were. Only the hub calls it, once, as it starts
// serving: nothing is in flight then.
func Recover() int {
	journal.Lock()
	journal.m = nil
	journalLoad()
	left := journal.m
	journal.m = map[string]open{}
	if len(left) > 0 {
		journalSave()
	}
	journal.Unlock()
	for _, o := range left {
		rec := usage.Record{Time: o.Time, Agent: "relay", Requested: o.Model, Model: o.Model, Endpoint: o.Endpoint,
			CallerKeyID: o.KeyID, CallerKeyName: o.KeyName, Member: o.Member, MemberName: o.Name, Plan: o.Plan,
			Settle: Interrupted, Est: o.Est, Error: "the hub stopped while this request was in flight", ErrType: "relay_interrupted"}
		if p, ok := PlanOf(o.Plan); ok {
			rec.Charge = p.Charge(rec)
		}
		budget.Append(rec)
	}
	return len(left)
}

// Stamp writes on a call's record who made it and how its reservation
// ended, before it is appended: the member, the plan and its charge, and
// Settle. o is the request the record is of, nil for a record written
// with none in flight.
func Stamp(who access.Identity, o *Open, rec *usage.Record) {
	if who.Member == "" {
		return
	}
	rec.Member, rec.MemberName, rec.Plan = who.Member, who.MemberName, who.Plan
	used := rec.Input+rec.Output+rec.CacheRead+rec.CacheWrite > 0
	answered := rec.Status >= 200 && rec.Status < 300
	// cut off — by the member's tool, or the connection — after a vendor
	// had the request: it may have used what it never got to report
	cut := rec.Status == 499 && rec.Provider != ""
	switch {
	case rec.IsRejected():
		rec.Settle = Released
	case used:
		rec.Settle = Settled
	case answered || cut:
		rec.Settle, rec.UsageUnknown = Estimated, true
		if o != nil {
			o.mu.Lock()
			if !o.charged {
				o.charged, rec.Est = true, o.Est
			}
			o.mu.Unlock()
		}
	default:
		rec.Settle = Released
	}
	if p, ok := PlanOf(who.Plan); ok {
		rec.Charge = p.Charge(*rec)
	}
}

// Totals are sums over some of a member's calls.
type Totals struct {
	// Calls that reached a provider; Refused those turned away here
	// (a limit, a model not allowed), which use nothing.
	Calls   int `json:"calls"`
	Refused int `json:"refused"`
	Failed  int `json:"failed"`
	// Unknown are the calls counted at their reservation (Est) because
	// no usage was reported for them; Interrupted those the hub was
	// stopped in the middle of.
	Unknown     int   `json:"unknown"`
	Interrupted int   `json:"interrupted"`
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	Est         int64 `json:"est"`
	// Charge is what the member's plan charged for the calls.
	Charge float64 `json:"charge"`
	// Cost is the hub's own estimate of what the calls cost upstream, at
	// the Usage page's prices: the administrator's alone.
	Cost float64 `json:"cost,omitempty"`
}

func (t *Totals) add(r usage.Record, cost float64) {
	if r.IsRejected() {
		t.Refused++
		return
	}
	t.Calls++
	if r.Status >= 400 {
		t.Failed++
	}
	if r.UsageUnknown {
		t.Unknown++
	}
	if r.Settle == Interrupted {
		t.Interrupted++
	}
	t.Input += int64(r.Input)
	t.Output += int64(r.Output)
	t.CacheRead += int64(r.CacheRead)
	t.CacheWrite += int64(r.CacheWrite)
	t.Est += r.Est
	t.Charge += r.Charge
	t.Cost += cost
}

// Line is the totals of one model, device, day or member.
type Line struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
	Totals
}

// Entry is one call as its member is shown it: no account, no provider
// key, no vendor's words, and none of what was asked or answered.
type Entry struct {
	Time      time.Time `json:"time"`
	Model     string    `json:"model"`
	Served    string    `json:"served,omitempty"`
	Device    string    `json:"device,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Via       string    `json:"via,omitempty"`
	Endpoint  string    `json:"endpoint,omitempty"`
	Status    int       `json:"status"`
	ErrType   string    `json:"errType,omitempty"`
	Millis    int64     `json:"ms"`
	Input     int       `json:"input"`
	Output    int       `json:"output"`
	CacheRead int       `json:"cacheRead,omitempty"`
	Est       int64     `json:"est,omitempty"`
	Settle    string    `json:"settle,omitempty"`
	Charge    float64   `json:"charge"`
	Refused   bool      `json:"refused,omitempty"`
}

// Statement is a member's use over a time: the totals, by model, by
// device and by day, the latest calls, and what a period plan's price
// comes to over it.
type Statement struct {
	Member string    `json:"member"`
	Name   string    `json:"name"`
	Period string    `json:"period"`
	Since  time.Time `json:"since"`
	Totals
	// Fee is a period plan's price for each of its windows that began
	// in the time, the one now in among them; Due is Fee and Charge.
	Fee      float64 `json:"fee"`
	Due      float64 `json:"due"`
	ByModel  []Line  `json:"byModel"`
	ByDevice []Line  `json:"byDevice"`
	ByDay    []Line  `json:"byDay"`
	Recent   []Entry `json:"recent"`
}

// Periods are the times a statement can be asked for.
var Periods = []string{"today", "7d", "30d", "month", "all"}

// Since is when a period began, and the period as it is named: 30d for
// one that isn't known.
func Since(period string, now time.Time) (time.Time, string) {
	y, m, d := now.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch period {
	case "today":
		return day, period
	case "7d":
		return day.AddDate(0, 0, -6), period
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, now.Location()), period
	case "all":
		return time.Time{}, period
	}
	return day.AddDate(0, 0, -29), "30d"
}

const recentKept = 100

type lines struct {
	order []string
	m     map[string]*Line
}

func (l *lines) at(key, name string) *Line {
	if l.m == nil {
		l.m = map[string]*Line{}
	}
	ln := l.m[key]
	if ln == nil {
		ln = &Line{Key: key, Name: name}
		l.m[key] = ln
		l.order = append(l.order, key)
	}
	if name != "" {
		ln.Name = name
	}
	return ln
}

func (l *lines) list() []Line {
	out := make([]Line, 0, len(l.order))
	for _, k := range l.order {
		out = append(out, *l.m[k])
	}
	return out
}

func entryOf(r usage.Record) Entry {
	model := r.Requested
	if model == "" {
		model = r.Model
	}
	return Entry{Time: r.Time, Model: model, Served: r.Served, Device: r.CallerKeyName, Agent: r.Agent, Via: r.Via, Endpoint: r.Endpoint,
		Status: r.Status, ErrType: r.ErrType, Millis: r.Millis, Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, Est: r.Est,
		Settle: r.Settle, Charge: r.Charge, Refused: r.IsRejected()}
}

// StatementOf is the statement of one member, by id, over a period. It
// reads only that member's records: nothing of another's is in it. admin
// adds the hub's own cost estimate.
func StatementOf(member, period string, now time.Time, admin bool) (Statement, error) {
	if member == "" {
		return Statement{}, errors.New("Member not found")
	}
	since, period := Since(period, now)
	s := Statement{Member: member, Period: period, Since: since, Recent: []Entry{}}
	var models, devices, days lines
	cost := func(usage.Record) (float64, bool) { return 0, false }
	if admin {
		cost = usage.NewCoster()
	}
	usage.Visit(since, func(r usage.Record) {
		if r.Member != member || r.Computer != "" || r.Time.Before(since) {
			return
		}
		c, _ := cost(r)
		s.Totals.add(r, c)
		e := entryOf(r)
		models.at(e.Model, "").add(r, c)
		devices.at(r.CallerKeyID, r.CallerKeyName).add(r, c)
		days.at(r.Time.In(now.Location()).Format("2006-01-02"), "").add(r, c)
		s.Recent = append(s.Recent, e)
		if r.MemberName != "" {
			s.Name = r.MemberName
		}
	})
	sort.SliceStable(s.Recent, func(i, j int) bool { return s.Recent[i].Time.After(s.Recent[j].Time) })
	if len(s.Recent) > recentKept {
		s.Recent = s.Recent[:recentKept]
	}
	s.ByModel, s.ByDevice, s.ByDay = models.list(), devices.list(), days.list()
	sort.SliceStable(s.ByDay, func(i, j int) bool { return s.ByDay[i].Key < s.ByDay[j].Key })
	if st, err := read(); err == nil {
		if m := st.member(member); m != nil {
			s.Name = m.Name
			if p := st.plan(m.Plan); p != nil && p.Kind == Period {
				s.Fee = p.Price * float64(windows(p.Window, since, m.Created, now))
			}
		}
	}
	s.Due = s.Fee + s.Charge
	return s, nil
}

// windows counts a plan's windows that the time from since to now touches,
// none of them from before the member was made.
func windows(period string, since time.Time, created int64, now time.Time) int {
	if c := time.Unix(created, 0); created > 0 && c.After(since) {
		since = c
	}
	if since.IsZero() || since.After(now) {
		return 1
	}
	n := 0
	for t := now; n < 1200; n++ {
		start, _ := access.Window(period, t)
		if !start.After(since) {
			return n + 1
		}
		t = start.Add(-time.Second)
	}
	return n
}

// Overview is every member's totals over a period, for the administrator:
// the same records the members' own statements are of. A member since
// removed is there by the name its records carry.
func Overview(period string, now time.Time) ([]Line, Totals) {
	since, _ := Since(period, now)
	var members lines
	var all Totals
	cost := usage.NewCoster()
	usage.Visit(since, func(r usage.Record) {
		if r.Member == "" || r.Computer != "" || r.Time.Before(since) {
			return
		}
		c, _ := cost(r)
		members.at(r.Member, r.MemberName).add(r, c)
		all.add(r, c)
	})
	out := members.list()
	if st, err := read(); err == nil {
		for i := range out {
			if m := st.member(out[i].Key); m != nil {
				out[i].Name = m.Name
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, all
}
