// Package relay is the hub's own business in AI Relay: the members who
// share one Pie, the groups that say which models a member may use, the
// plans their use is charged by, and the invitations and sign-ins that let
// a member in to make the device keys their tools call the hub with.
//
// It adds no gateway of its own. A member's device is a gateway key
// (access.Key with Member set), so every request is authenticated, held to
// its models and counted by the code that does so for any gateway key; what
// a device may do is the member's, resolved anew on each request
// (access.MemberOf), so a group changed or a member turned off holds on
// the next one. Use is read from usage.jsonl, the ledger the Usage page
// shows: a member's own page and the administrator's totals are sums over
// the same records.
//
// Its state is relay.json beside providers.json: members, groups, plans,
// and the hashes of invitations, passwords and web sessions. No key, code
// or password is kept as given.
package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/settings"
)

// Member is one person the hub serves.
type Member struct {
	ID string `json:"id"`
	// Name is what the member signs in as: lower case, unique.
	Name    string `json:"name"`
	Display string `json:"display,omitempty"`
	Off     bool   `json:"off,omitempty"`
	// Groups are the ids of the groups the member is in: the models it
	// may use are theirs together.
	Groups []string `json:"groups,omitempty"`
	// Plan is the id of the plan the member's use is charged by, "" for
	// none: nothing charged, and no limit but Limit.
	Plan string `json:"plan,omitempty"`
	// Limit is the member's own limit, set by the administrator in place
	// of the plan's.
	Limit *access.Limit `json:"limit,omitempty"`
	// Concurrency is how many of the member's requests run at once and
	// Queue how many more wait their turn; 0 for the plan's, or the hub's
	// defaults with no plan.
	Concurrency int   `json:"concurrency,omitempty"`
	Queue       int   `json:"queue,omitempty"`
	Created     int64 `json:"created,omitempty"`
	// the password, as PBKDF2 keeps it; "" until an invitation is redeemed
	PassHash string `json:"passHash,omitempty"`
	PassSalt string `json:"passSalt,omitempty"`
	PassIter int    `json:"passIter,omitempty"`
}

// Group is a set of models, such as lab or private, that its members may
// use.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Models are patterns as a gateway key's are (access.CleanModels):
	// "<provider>/<model>", "<provider>/*", or a routing group's
	// "group/<id>".
	Models []string `json:"models,omitempty"`
}

// The kinds of plan.
const (
	// Metered charges each call by its tokens at fixed prices.
	Metered = "metered"
	// Period charges a fixed price for each period, which includes an
	// allowance of tokens: when it is used up the member waits for the
	// next period.
	Period = "period"
)

// Plan says what a member's use costs. The administrator gives a member
// one: nothing is paid through the hub.
type Plan struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// A metered plan's prices, in US dollars for a million tokens:
	// uncached input, output, and cache reads and writes.
	PriceIn         float64 `json:"priceIn,omitempty"`
	PriceOut        float64 `json:"priceOut,omitempty"`
	PriceCacheRead  float64 `json:"priceCacheRead,omitempty"`
	PriceCacheWrite float64 `json:"priceCacheWrite,omitempty"`
	// Budget caps what a metered plan charges in a window, 0 for no cap.
	Budget float64 `json:"budget,omitempty"`
	// Window is the calendar window the plan counts in: day, week or
	// month. A period plan's price is for one of them.
	Window string `json:"window,omitempty"`
	// Price is a period plan's price for each window, and Tokens the
	// tokens it includes: 0 for as many as the member uses. CacheReads
	// counts cache reads among them.
	Price      float64 `json:"price,omitempty"`
	Tokens     int64   `json:"tokens,omitempty"`
	CacheReads bool    `json:"cacheReads,omitempty"`
	// Concurrency and Queue are the plan's members' (Member.Concurrency).
	Concurrency int `json:"concurrency,omitempty"`
	Queue       int `json:"queue,omitempty"`
}

// invite is an invitation not yet used: its code's hash, never the code.
type invite struct {
	ID      string `json:"id"`
	Member  string `json:"member"`
	Hash    string `json:"hash"`
	Expires int64  `json:"expires"`
}

// session is a member signed in on the web page or from Mag.
type session struct {
	Hash    string `json:"hash"`
	Member  string `json:"member"`
	Expires int64  `json:"expires"`
}

// State is everything the hub keeps of its members.
type State struct {
	Members  []Member  `json:"members"`
	Groups   []Group   `json:"groups"`
	Plans    []Plan    `json:"plans"`
	Invites  []invite  `json:"invites,omitempty"`
	Sessions []session `json:"sessions,omitempty"`
}

// The hub's own defaults for a member no plan says otherwise of.
const (
	DefaultConcurrency = 2
	DefaultQueue       = 4
)

// QueueWait is how long a member's request waits for one of the member's
// own to finish before it is told to come back.
var QueueWait = 60 * time.Second

// Path is relay.json.
func Path() string { return filepath.Join(settings.Dir(), "relay.json") }

var (
	mu     sync.Mutex
	cached struct {
		path      string
		size, mod int64
		st        State
		ok        bool
	}
)

// load reads the state, from memory while the file is as it was last read.
// Called with mu held.
func load() (State, error) {
	p := Path()
	fi, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	if cached.ok && cached.path == p && cached.size == fi.Size() && cached.mod == fi.ModTime().UnixNano() {
		return cached.st.clone(), nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		// unknown, never empty: a file that doesn't parse lets no member in
		// and is never written over
		return State{}, fmt.Errorf("%s doesn't parse: %w", p, err)
	}
	cached.path, cached.size, cached.mod, cached.st, cached.ok = p, fi.Size(), fi.ModTime().UnixNano(), st.clone(), true
	return st, nil
}

func (s State) clone() State {
	out := State{Members: slices.Clone(s.Members), Groups: slices.Clone(s.Groups), Plans: slices.Clone(s.Plans),
		Invites: slices.Clone(s.Invites), Sessions: slices.Clone(s.Sessions)}
	for i := range out.Members {
		out.Members[i].Groups = slices.Clone(out.Members[i].Groups)
		if l := out.Members[i].Limit; l != nil {
			v := *l
			out.Members[i].Limit = &v
		}
	}
	for i := range out.Groups {
		out.Groups[i].Models = slices.Clone(out.Groups[i].Models)
	}
	return out
}

func save(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return err
	}
	cached.ok = false
	return edit.WriteAtomic(p, append(b, '\n'))
}

// change reads the state, lets fn change it and writes it back, all at
// once: nothing is written when fn fails.
func change(fn func(*State) error) error {
	mu.Lock()
	defer mu.Unlock()
	st, err := load()
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	return save(st)
}

// read is the state as it is now.
func read() (State, error) {
	mu.Lock()
	defer mu.Unlock()
	return load()
}

// Forget drops what is kept in memory, as a restart would: a test's own.
func Forget() {
	mu.Lock()
	cached.ok = false
	mu.Unlock()
	throttle.Lock()
	throttle.m = map[string]*attempts{}
	throttle.Unlock()
}

func (s *State) member(id string) *Member {
	for i := range s.Members {
		if s.Members[i].ID == id {
			return &s.Members[i]
		}
	}
	return nil
}

// memberNamed finds a member by its sign-in name or its id.
func (s *State) memberNamed(name string) *Member {
	name = strings.ToLower(strings.TrimSpace(name))
	for i := range s.Members {
		if s.Members[i].Name == name || s.Members[i].ID == name {
			return &s.Members[i]
		}
	}
	return nil
}

func (s *State) group(id string) *Group {
	for i := range s.Groups {
		if s.Groups[i].ID == id {
			return &s.Groups[i]
		}
	}
	return nil
}

func (s *State) plan(id string) *Plan {
	for i := range s.Plans {
		if s.Plans[i].ID == id {
			return &s.Plans[i]
		}
	}
	return nil
}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,39}$`)
	idRe   = regexp.MustCompile(`[^a-z0-9]+`)
)

func slug(name string) string {
	return strings.Trim(idRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// View is the state as the administrator's page and command line are
// given it: no hash of anything, each member with its devices and whether
// it has a password yet or an invitation waiting.
type View struct {
	Members []MemberView `json:"members"`
	Groups  []Group      `json:"groups"`
	Plans   []Plan       `json:"plans"`
}

// MemberView is a member without what would sign anyone in as it.
type MemberView struct {
	Member
	HasPassword bool         `json:"hasPassword"`
	Invited     int64        `json:"invited,omitempty"` // when its waiting invitation ends
	Devices     []access.Key `json:"devices"`
}

func (m Member) public() Member {
	m.PassHash, m.PassSalt, m.PassIter = "", "", 0
	return m
}

// Snapshot is the state for the administrator.
func Snapshot() (View, error) {
	st, err := read()
	if err != nil {
		return View{}, err
	}
	devices, err := access.Devices("")
	if err != nil {
		return View{}, err
	}
	now := time.Now().Unix()
	v := View{Members: []MemberView{}, Groups: st.Groups, Plans: st.Plans}
	if v.Groups == nil {
		v.Groups = []Group{}
	}
	if v.Plans == nil {
		v.Plans = []Plan{}
	}
	for _, m := range st.Members {
		mv := MemberView{Member: m.public(), HasPassword: m.PassHash != "", Devices: []access.Key{}}
		for _, i := range st.Invites {
			if i.Member == m.ID && i.Expires > now {
				mv.Invited = max(mv.Invited, i.Expires)
			}
		}
		for _, d := range devices {
			if d.Member == m.ID {
				mv.Devices = append(mv.Devices, d)
			}
		}
		v.Members = append(v.Members, mv)
	}
	return v, nil
}

// MemberIn is what the administrator says of a member.
type MemberIn struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Display     string        `json:"display"`
	Off         bool          `json:"off"`
	Groups      []string      `json:"groups"`
	Plan        string        `json:"plan"`
	Limit       *access.Limit `json:"limit"`
	Concurrency int           `json:"concurrency"`
	Queue       int           `json:"queue"`
}

// SaveMember adds a member (no ID) or changes one, and returns its id. A
// member turned off is let in nowhere from the next request on: its
// devices are refused and its web sessions ended.
func SaveMember(in MemberIn) (string, error) {
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !nameRe.MatchString(name) {
		return "", errors.New("A member's name is 2 to 40 lower-case letters, digits, dots, dashes or underscores")
	}
	if len(in.Display) > 120 {
		return "", errors.New("A member's display name is at most 120 characters")
	}
	if in.Concurrency < 0 || in.Concurrency > 64 || in.Queue < 0 || in.Queue > 256 {
		return "", errors.New("Concurrency is 0 to 64 and the queue 0 to 256")
	}
	lim, err := in.Limit.Valid()
	if err != nil {
		return "", err
	}
	id := in.ID
	err = change(func(st *State) error {
		for _, g := range in.Groups {
			if st.group(g) == nil {
				return fmt.Errorf("There is no group %q", g)
			}
		}
		if in.Plan != "" && st.plan(in.Plan) == nil {
			return fmt.Errorf("There is no plan %q", in.Plan)
		}
		if o := st.memberNamed(name); o != nil && o.ID != id {
			return fmt.Errorf("There is a member named %q already", name)
		}
		groups := slices.Compact(slices.Sorted(slices.Values(in.Groups)))
		if id == "" {
			var err error
			if id, err = random(8); err != nil {
				return err
			}
			st.Members = append(st.Members, Member{ID: id, Name: name, Display: strings.TrimSpace(in.Display), Off: in.Off,
				Groups: groups, Plan: in.Plan, Limit: lim, Concurrency: in.Concurrency, Queue: in.Queue, Created: time.Now().Unix()})
			return nil
		}
		m := st.member(id)
		if m == nil {
			return errors.New("Member not found")
		}
		m.Name, m.Display, m.Off, m.Groups, m.Plan = name, strings.TrimSpace(in.Display), in.Off, groups, in.Plan
		m.Limit, m.Concurrency, m.Queue = lim, in.Concurrency, in.Queue
		if m.Off {
			st.Sessions = slices.DeleteFunc(st.Sessions, func(s session) bool { return s.Member == id })
		}
		return nil
	})
	return id, err
}

// RemoveMember takes a member away, with its devices, invitations and
// sessions. Its use stays in the ledger.
func RemoveMember(id string) error {
	// the devices first: were the member gone and its keys left, they
	// would be refused all the same (MemberOf finds no member), so
	// neither order lets a removed member in
	if err := access.RemoveDevices(id); err != nil {
		return err
	}
	return change(func(st *State) error {
		if st.member(id) == nil {
			return errors.New("Member not found")
		}
		st.Members = slices.DeleteFunc(st.Members, func(m Member) bool { return m.ID == id })
		st.Invites = slices.DeleteFunc(st.Invites, func(i invite) bool { return i.Member == id })
		st.Sessions = slices.DeleteFunc(st.Sessions, func(s session) bool { return s.Member == id })
		return nil
	})
}

// SaveGroup adds a group (no ID) or changes one, and returns its id.
func SaveGroup(g Group) (string, error) {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" || len(g.Name) > 80 {
		return "", errors.New("A group's name is 1 to 80 characters")
	}
	ms, err := access.CleanModels(g.Models)
	if err != nil {
		return "", err
	}
	g.Models = ms
	err = change(func(st *State) error {
		if g.ID == "" {
			g.ID = slug(g.Name)
			if g.ID == "" {
				return errors.New("A group's name needs a letter or a digit")
			}
			if st.group(g.ID) != nil {
				return fmt.Errorf("There is a group %q already", g.ID)
			}
			st.Groups = append(st.Groups, g)
			return nil
		}
		o := st.group(g.ID)
		if o == nil {
			return errors.New("Group not found")
		}
		*o = g
		return nil
	})
	return g.ID, err
}

// RemoveGroup takes a group away, and its members out of it: they lose the
// models it alone gave them.
func RemoveGroup(id string) error {
	return change(func(st *State) error {
		if st.group(id) == nil {
			return errors.New("Group not found")
		}
		st.Groups = slices.DeleteFunc(st.Groups, func(g Group) bool { return g.ID == id })
		for i := range st.Members {
			st.Members[i].Groups = slices.DeleteFunc(st.Members[i].Groups, func(g string) bool { return g == id })
		}
		return nil
	})
}

// SavePlan adds a plan (no ID) or changes one, and returns its id.
func SavePlan(p Plan) (string, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 80 {
		return "", errors.New("A plan's name is 1 to 80 characters")
	}
	if p.Kind != Metered && p.Kind != Period {
		return "", errors.New("A plan is metered (a price for each token) or period (a price for each period, with tokens included)")
	}
	if p.Window == "" {
		p.Window = "month"
	}
	if !slices.Contains(access.Periods, p.Window) {
		return "", errors.New("A plan's window is day, week or month")
	}
	for _, v := range []float64{p.PriceIn, p.PriceOut, p.PriceCacheRead, p.PriceCacheWrite, p.Budget, p.Price} {
		if v < 0 || v != v || v > 1e9 {
			return "", errors.New("A plan's prices can't be negative")
		}
	}
	if p.Tokens < 0 || p.Concurrency < 0 || p.Concurrency > 64 || p.Queue < 0 || p.Queue > 256 {
		return "", errors.New("A plan's tokens can't be negative; its concurrency is 0 to 64 and its queue 0 to 256")
	}
	if p.Kind == Metered {
		p.Price, p.Tokens, p.CacheReads = 0, 0, false
	} else {
		p.PriceIn, p.PriceOut, p.PriceCacheRead, p.PriceCacheWrite, p.Budget = 0, 0, 0, 0, 0
	}
	err := change(func(st *State) error {
		if p.ID == "" {
			p.ID = slug(p.Name)
			if p.ID == "" {
				return errors.New("A plan's name needs a letter or a digit")
			}
			if st.plan(p.ID) != nil {
				return fmt.Errorf("There is a plan %q already", p.ID)
			}
			st.Plans = append(st.Plans, p)
			return nil
		}
		o := st.plan(p.ID)
		if o == nil {
			return errors.New("Plan not found")
		}
		*o = p
		return nil
	})
	return p.ID, err
}

// RemovePlan takes a plan away. One a member is on stays until the member
// is given another: taking it would lift the member's limit unasked.
func RemovePlan(id string) error {
	return change(func(st *State) error {
		if st.plan(id) == nil {
			return errors.New("Plan not found")
		}
		for _, m := range st.Members {
			if m.Plan == id {
				return fmt.Errorf("%s is on this plan: give them another first", m.Name)
			}
		}
		st.Plans = slices.DeleteFunc(st.Plans, func(p Plan) bool { return p.ID == id })
		return nil
	})
}
