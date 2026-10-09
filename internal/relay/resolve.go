package relay

import (
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/usage"
)

func init() { access.MemberOf = memberOf }

// memberOf is what a member's device may do now (access.MemberOf): the
// models of the member's groups, the member's limit and how many of its
// requests run at once. A member that is gone or turned off, or a state
// that can't be read, lets the key in nowhere.
func memberOf(k access.Key) (access.Identity, bool) {
	st, err := read()
	if err != nil {
		return access.Identity{}, false
	}
	m := st.member(k.Member)
	if m == nil || m.Off {
		return access.Identity{}, false
	}
	who := access.Identity{Member: m.ID, MemberName: m.Name, Models: st.modelsOf(*m), Plan: m.Plan}
	p := st.plan(m.Plan)
	who.MemberLimit = limitOf(*m, p)
	who.Concurrency, who.Queue = lanesOf(*m, p)
	if p != nil && p.Kind == Metered {
		who.ChargeRate = p.PriceIn / 1e6
	}
	return who, true
}

// modelsOf is the models a member may use: its groups' patterns together.
// None, for a member in no group or in groups that give none, is said as
// such by access.Authenticate (access.NoModels), never as every model.
func (s *State) modelsOf(m Member) []string {
	var out []string
	for _, id := range m.Groups {
		g := s.group(id)
		if g == nil {
			continue
		}
		for _, p := range g.Models {
			if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, p) }) {
				out = append(out, p)
			}
		}
	}
	return out
}

// limitOf is the limit a member is held to: the administrator's own for
// the member, else the plan's — a period plan's included tokens, a metered
// plan's budget — else none.
func limitOf(m Member, p *Plan) *access.Limit {
	if m.Limit.Limited() {
		return m.Limit
	}
	if p == nil {
		return nil
	}
	var l access.Limit
	switch p.Kind {
	case Period:
		l = access.Limit{Period: p.Window, Tokens: p.Tokens, CacheReads: p.CacheReads}
	case Metered:
		l = access.Limit{Period: p.Window, Charge: p.Budget}
	}
	if !l.Limited() {
		return nil
	}
	return &l
}

// lanesOf is how many of a member's requests run at once, and how many
// wait: the member's own, else the plan's, else the hub's defaults.
func lanesOf(m Member, p *Plan) (concurrency, queue int) {
	concurrency, queue = m.Concurrency, m.Queue
	if p != nil {
		if concurrency == 0 {
			concurrency = p.Concurrency
		}
		if queue == 0 {
			queue = p.Queue
		}
	}
	if concurrency == 0 {
		concurrency = DefaultConcurrency
	}
	if queue == 0 {
		queue = DefaultQueue
	}
	return concurrency, queue
}

// Charge is what plan p charges for the call r, in US dollars: a metered
// plan's prices for its tokens, with the tokens reserved standing for a
// call whose usage nobody reported (r.Est), and nothing on a period plan,
// whose price is for the period.
func (p Plan) Charge(r usage.Record) float64 {
	if p.Kind != Metered {
		return 0
	}
	in := float64(r.Input)
	if r.UsageUnknown || r.Settle == Interrupted {
		in += float64(r.Est)
	}
	return (in*p.PriceIn + float64(r.Output)*p.PriceOut + float64(r.CacheRead)*p.PriceCacheRead + float64(r.CacheWrite)*p.PriceCacheWrite) / 1e6
}

// PlanOf is the plan with this id as it is now.
func PlanOf(id string) (Plan, bool) {
	if id == "" {
		return Plan{}, false
	}
	st, err := read()
	if err != nil {
		return Plan{}, false
	}
	if p := st.plan(id); p != nil {
		return *p, true
	}
	return Plan{}, false
}

// Standing is where a member stands: who they are, what they may use and
// how much of their limit is left. It is what the member's own page and
// Mag show, and has nothing of any other member or of the hub's accounts.
type Standing struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Display     string         `json:"display,omitempty"`
	Groups      []Group        `json:"groups"`
	Models      []string       `json:"models"`
	Plan        *Plan          `json:"plan,omitempty"`
	Limit       *budget.Status `json:"limit,omitempty"`
	Concurrency int            `json:"concurrency"`
	Queue       int            `json:"queue"`
}

// StandingOf is the standing of the member with this id.
func StandingOf(id string, now time.Time) (Standing, bool) {
	st, err := read()
	if err != nil {
		return Standing{}, false
	}
	m := st.member(id)
	if m == nil || m.Off {
		return Standing{}, false
	}
	out := Standing{ID: m.ID, Name: m.Name, Display: m.Display, Groups: []Group{}, Models: st.modelsOf(*m)}
	if out.Models == nil {
		out.Models = []string{}
	}
	for _, g := range m.Groups {
		if gr := st.group(g); gr != nil {
			out.Groups = append(out.Groups, Group{ID: gr.ID, Name: gr.Name})
		}
	}
	p := st.plan(m.Plan)
	if p != nil {
		v := *p
		out.Plan = &v
	}
	out.Limit = budget.OfMember(m.ID, limitOf(*m, p), now)
	out.Concurrency, out.Queue = lanesOf(*m, p)
	return out, true
}
