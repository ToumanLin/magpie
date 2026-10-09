package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/hubclient"
	"github.com/yetone/magpie/internal/mode"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/relay"
)

// AI Relay's pages (package mode). The page of Mag, the member's client,
// has Hub, Agents, Usage, Sessions and Library; Pie's, the server's, has
// Providers, Gateway, Routing, Members, Usage and Plugins. A tab left out
// is not what keeps a program to its own: modeGuard refuses the other
// program's routes here, whatever page asks, and the hub itself signs and
// checks every request for a model (gateway/hub.go).

// clientRefuses and serverRefuses are the routes each program has none of,
// by the path's start. A read of what a page of its own draws from
// (/api/providers for the model pickers) stays; what changes the other
// program's state, or tells its secrets, goes.
var (
	// Mag keeps no provider, account, key, routing or plugin of its own
	clientWrites = []string{"/api/provider/", "/api/providers/", "/api/keys/", "/api/groups/", "/api/plugins/", "/api/icons",
		"/api/usage/workbuddy-checkin", "/api/usage/trae-checkin", "/api/usage/minimax-checkin", "/api/usage/qoder-checkin", "/api/usage/plugin-checkin", "/api/usage/codex-reset",
		"/api/settings/lan", "/api/settings/cors", "/api/settings/search-api", "/api/settings/search-key", "/api/settings/github-token", "/api/davsync/"}
	clientAll = []string{"/api/caller-keys", "/api/signin", "/api/login/", "/api/plugin-signin", "/api/stepfun/", "/api/codex/daemon/",
		"/api/importapps", "/api/import/", "/api/relay", "/api/settings/reveal", "/api/archive"}
	// Pie wires no agent and reads no session on the computer it runs on
	serverAll = []string{"/api/set", "/api/agents/", "/api/agent-models/", "/api/agent-fast", "/api/profile/", "/api/drift",
		"/api/sessions", "/api/library", "/api/hub", "/api/davsync"}
)

func startsAny(path string, starts []string) bool {
	return slices.ContainsFunc(starts, func(s string) bool { return path == strings.TrimSuffix(s, "/") || strings.HasPrefix(path, s) })
}

// refusedByMode says why this program has no such route, "" when it has.
func refusedByMode(r *http.Request) string {
	p := r.URL.Path
	switch mode.Current() {
	case mode.Client:
		if startsAny(p, clientAll) || r.Method != http.MethodGet && startsAny(p, clientWrites) {
			return "Mag keeps no provider, account, routing or member: those are the hub's, kept by its administrator"
		}
	case mode.Server:
		if startsAny(p, serverAll) {
			return "Pie wires no agent and reads no session or library on the computer it runs on: those are a member's own, in Mag"
		}
	default:
		if startsAny(p, []string{"/api/relay", "/api/hub"}) {
			return "This is AI Relay's: run Mag (the client) or Pie (the server)"
		}
	}
	return ""
}

// modeGuard refuses the routes this program has none of (403), before any
// handler reads or writes for them.
func modeGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if why := refusedByMode(r); why != "" {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusForbidden)
			json.NewEncoder(rw).Encode(map[string]string{"error": why, "code": "not_this_program"})
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// relayRoutes are the administrator's on Pie and the member's on Mag;
// modeGuard lets each program reach only its own.
func relayRoutes(mux *http.ServeMux) {
	// ---- Pie: members, groups, plans, devices and the ledger's totals ----
	mux.HandleFunc("GET /api/relay", func(rw http.ResponseWriter, r *http.Request) {
		st, err := relayState(r.URL.Query().Get("period"))
		if err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, st)
	})
	mux.HandleFunc("GET /api/relay/usage", func(rw http.ResponseWriter, r *http.Request) {
		s, err := relay.StatementOf(r.URL.Query().Get("member"), r.URL.Query().Get("period"), time.Now(), true)
		if err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, s)
	})
	mux.HandleFunc("POST /api/relay/{action}", func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			Member relay.MemberIn `json:"member"`
			Group  relay.Group    `json:"group"`
			Plan   relay.Plan     `json:"plan"`
			ID     string         `json:"id"`
			Period string         `json:"period"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(rw, err)
			return
		}
		out := map[string]any{}
		var err error
		switch r.PathValue("action") {
		case "member-save":
			out["id"], err = relay.SaveMember(in.Member)
		case "member-remove":
			err = relay.RemoveMember(in.ID)
		case "group-save":
			out["id"], err = relay.SaveGroup(in.Group)
		case "group-remove":
			err = relay.RemoveGroup(in.ID)
		case "plan-save":
			out["id"], err = relay.SavePlan(in.Plan)
		case "plan-remove":
			err = relay.RemovePlan(in.ID)
		case "invite":
			var code string
			var exp time.Time
			if code, exp, err = relay.Invite(in.ID); err == nil {
				// shown this once: the page keeps it only while its dialog is open
				out["code"], out["expires"] = code, exp
			}
		case "device-revoke":
			err = access.RemoveDevice("", in.ID)
		default:
			err = fmt.Errorf("unknown action %q", r.PathValue("action"))
		}
		if err != nil {
			fail(rw, err)
			return
		}
		st, err := relayState(in.Period)
		if err != nil {
			fail(rw, err)
			return
		}
		out["state"] = st
		writeJSON(rw, out)
	})

	// ---- Mag: the hub this computer is connected to ----
	mux.HandleFunc("GET /api/hub", func(rw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		writeJSON(rw, hubState(ctx))
	})
	mux.HandleFunc("GET /api/hub/usage", func(rw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		s, err := hubclient.Usage(ctx, r.URL.Query().Get("period"))
		if err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, s)
	})
	mux.HandleFunc("POST /api/hub/connect", func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			Address  string `json:"address"`
			How      string `json:"how"` // "login", "invite" or "key"
			Name     string `json:"name"`
			Password string `json:"password"`
			Code     string `json:"code"`
			Key      string `json:"key"`
			Device   string `json:"device"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(rw, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		var err error
		switch in.How {
		case "key":
			err = hubclient.ConnectKey(ctx, in.Address, in.Key)
		case "invite":
			err = hubclient.ConnectInvite(ctx, in.Address, in.Code, in.Password, in.Device)
		case "login", "":
			err = hubclient.ConnectLogin(ctx, in.Address, in.Name, in.Password, in.Device)
		default:
			err = errors.New("connect by login, invite or key")
		}
		if err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, hubState(ctx))
	})
	mux.HandleFunc("POST /api/hub/disconnect", func(rw http.ResponseWriter, r *http.Request) {
		if err := hubclient.Disconnect(); err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, hubState(r.Context()))
	})
}

// relayJSON is the Members page's state.
type relayJSON struct {
	relay.View
	// Standing is each member's limit and what is used of it, by id, and
	// Lanes what each has running and waiting now
	Standing map[string]relay.Standing `json:"standing"`
	Lanes    map[string]gateway.Lane   `json:"lanes"`
	// Usage is each member's totals over Period, and All their sum: the
	// ledger the members' own pages read
	Usage  []relay.Line `json:"usage"`
	All    relay.Totals `json:"all"`
	Period string       `json:"period"`
	// Hub is where members reach the hub, and Page their sign-in page
	Hub  string `json:"hub"`
	Page string `json:"page"`
	// Local: the hub listens on this computer only, so no other reaches it
	Local bool `json:"local"`
	// Models are what a group can be given: every model and routing group
	Models []relay.Model `json:"models"`
}

func relayState(period string) (relayJSON, error) {
	v, err := relay.Snapshot()
	if err != nil {
		return relayJSON{}, err
	}
	now := time.Now()
	_, period = relay.Since(period, now)
	out := relayJSON{View: v, Standing: map[string]relay.Standing{}, Lanes: gateway.MemberLanes(), Period: period, Models: []relay.Model{}}
	for _, m := range v.Members {
		if st, ok := relay.StandingOf(m.ID, now); ok {
			out.Standing[m.ID] = st
		}
	}
	out.Usage, out.All = relay.Overview(period, now)
	if out.Usage == nil {
		out.Usage = []relay.Line{}
	}
	out.Hub = gateway.PublicURL()
	if out.Hub == "" {
		if us := gateway.LANURLs(); len(us) > 0 && gateway.OpenHost() {
			out.Hub = us[0]
		} else {
			out.Hub = gateway.URL()
		}
	}
	out.Page = out.Hub + "/relay/"
	out.Local = gateway.PublicURL() == "" && !gateway.OpenHost()
	for _, e := range provider.Catalog() {
		out.Models = append(out.Models, relay.Model{ID: e.ID, Name: e.Name, Group: e.Group != ""})
	}
	return out, nil
}

// hubJSON is the Hub page's state.
type hubJSON struct {
	hubclient.Status
	// Gateway is the local gateway the agents here are wired to, and
	// Device what this computer would be called at a hub
	Gateway string `json:"gateway"`
	Name    string `json:"name"`
}

func hubState(ctx context.Context) hubJSON {
	return hubJSON{Status: hubclient.StatusNow(ctx), Gateway: gateway.URL(), Name: hubclient.DeviceName()}
}
