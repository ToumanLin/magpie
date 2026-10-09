package gateway

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/mode"
	"github.com/yetone/magpie/internal/provider"
	hub "github.com/yetone/magpie/internal/relay"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

// The hub gateway is this gateway as Pie serves it (mode.Server): the one
// address every member's tools and Mags call. It is the same handler, held
// closer:
//
//   - Every request is signed. A request for a model needs an enabled key
//     wherever it comes from, this computer too: on a hub "any token on
//     loopback" would let whatever runs beside it, or a proxy that adds no
//     forwarding header, spend every member's accounts unnamed. What needs
//     none is what says nothing: the address's own name (GET /), the
//     member's sign-in page under /relay/, which signs its callers in
//     itself, and the callbacks of programs the hub starts on this computer.
//   - A member's device is held to the member's models, limit and
//     concurrency, and told nothing of the hub's accounts: the quota and
//     concurrency reports, which name accounts and balances, answer only a
//     key of the hub's own (one that is no member's).
//   - AIRELAY_LOCAL_OPEN=1 lets this computer's own requests in without a
//     key again, as magpie does, for an administrator trying a provider.

// shared: the gateway asks callers from elsewhere for a key — shared on
// the local network from Settings, or a hub, which always does.
func shared() bool { return mode.IsServer() || settings.Load().LAN }

// hubOpen are the paths a hub answers unsigned: they say nothing of its
// models, members or accounts.
func hubOpen(r *http.Request) bool {
	switch p := r.URL.Path; {
	case p == "/relay" || strings.HasPrefix(p, "/relay/"):
		return true // the member's page and its API sign their callers in
	case r.Method == http.MethodGet && (p == "/" || p == "/v1" || p == "/v1/" || p == "/api/hello"):
		return true
	case r.Method == http.MethodOptions:
		return true // a preflight carries no key; corsGuard answers it
	}
	return false
}

// hubLocal are the paths programs the hub itself starts call back on,
// from this computer: Claude Code's MCP bridge and the MCP proxy.
func hubLocal(r *http.Request) bool {
	return local(r) && (strings.HasPrefix(r.URL.Path, "/_magpie/") || strings.HasPrefix(r.URL.Path, "/mcp/"))
}

// hubGuard is lanGuard on a hub.
func hubGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hubOpen(r) || hubLocal(r) {
			next.ServeHTTP(w, r)
			return
		}
		if local(r) && mode.LocalOpen() && !managedKey(r) {
			next.ServeHTTP(w, r)
			return
		}
		var who access.Identity
		ok := false
		for _, k := range callerKeys(r) {
			if who, ok = access.Authenticate(k); ok {
				break
			}
		}
		if !ok {
			msg := refusedKey(callerKey(r))
			log.Printf("refused %s %s from %s: %s", r.Method, r.URL.Path, r.RemoteAddr, msg)
			writeError(w, protoOfPath(r.URL.Path), http.StatusUnauthorized, msg)
			return
		}
		r = withGatewayToken(r.WithContext(access.WithIdentity(r.Context(), who)))
		if who.Member != "" {
			// which of the hub's accounts answers is routing's to say: a
			// member's device names none (withGatewayToken cloned the headers)
			r.Header.Del(AccountHeader)
		}
		if who.Member == "" {
			// a key of the hub's own: told what a shared gateway's key is
			r = r.WithContext(context.WithValue(r.Context(), lanKeyed{}, true))
		}
		next.ServeHTTP(w, r)
	})
}

// memberOnly answers a member's device what it gets in place of the hub's
// own reports: nothing of the accounts behind the models.
func memberOnly(w http.ResponseWriter, r *http.Request) bool {
	if access.Caller(r.Context()).Member == "" {
		return false
	}
	writeError(w, provider.Chat, http.StatusForbidden, "this is the hub's own report, of its accounts: a member's device isn't told it. Your own usage and limit are at /relay/api/usage and /relay/api/me")
	return true
}

// openKey is where a member's request in flight is kept in its context.
type openKey struct{}

func openOf(ctx context.Context) *hub.Open {
	o, _ := ctx.Value(openKey{}).(*hub.Open)
	return o
}

// memberLanes are the slots of each member with a concurrency limit, by
// "member:<id>": the same queue the provider lanes are (concurrency.go),
// counted over every device of the member's.
var memberLanes lanes

// memberTurn waits for one of the member's slots. It answers the release,
// or false having told the caller why not: the queue is full or the wait
// too long (429, to come back), or the caller went away (nothing sent).
func memberTurn(w http.ResponseWriter, r *http.Request, who access.Identity, model string) (release func(), ok bool) {
	if who.Member == "" || who.Concurrency <= 0 {
		return func() {}, true
	}
	release, err := memberLanes.take(r.Context(), budgetLane(who), who.Concurrency, who.Queue, hub.QueueWait)
	if err == nil {
		return release, true
	}
	c := callerOf(r)
	rec := usage.Record{Time: limitClock(), Agent: c.agent, Via: c.via, Requested: model, Rejected: true, Endpoint: r.URL.Path,
		Session: sessionOf(r.Header), NativeSession: nativeSessionOf(r.Header)}
	switch {
	case errors.Is(err, errQueueFull), errors.Is(err, errQueueWait):
		msg := "AI Relay member " + strconv.Quote(who.MemberName) + " has " + strconv.Itoa(who.Concurrency) + " requests running and " + strconv.Itoa(who.Queue) + " waiting already: try again when one has finished"
		if errors.Is(err, errQueueWait) {
			msg = "AI Relay member " + strconv.Quote(who.MemberName) + " has " + strconv.Itoa(who.Concurrency) + " requests running, and this one waited " + hub.QueueWait.String() + " for a turn: try again when one has finished"
		}
		w.Header().Set("Retry-After", "5")
		writeError(w, protoOfPath(r.URL.Path), http.StatusTooManyRequests, msg)
		rec.Status, rec.Error, rec.ErrType = http.StatusTooManyRequests, msg, "relay_member_concurrency"
	default:
		// the caller went away while it waited: nothing was sent anywhere
		rec.Status, rec.Error, rec.ErrType = 499, "the caller went away while the request waited for the member's turn", "relay_member_canceled"
	}
	appendUsage(r, rec)
	return nil, false
}

func budgetLane(who access.Identity) string { return "member:" + who.Member }

// MemberLanes is what each member has running and waiting now, by member
// id, for the administrator's page.
func MemberLanes() map[string]Lane {
	memberLanes.mu.Lock()
	defer memberLanes.mu.Unlock()
	out := map[string]Lane{}
	for who, ln := range memberLanes.m {
		if id, ok := strings.CutPrefix(who, "member:"); ok {
			out[id] = Lane{Busy: ln.busy, Waiting: len(ln.queue), Limit: ln.limit}
		}
	}
	return out
}

// memberModels lists the models a member may use for its page
// (hub.ModelsFor): the catalog through the filter the model lists go
// through.
func memberModels(who access.Identity) []hub.Model {
	out := []hub.Model{}
	if who.Member == "" {
		return out
	}
	if len(who.Models) == 0 {
		who.Models = []string{access.NoModels}
	}
	for _, e := range provider.Catalog() {
		if entryAllowed(who, e) {
			out = append(out, hub.Model{ID: e.ID, Name: e.Name, Group: e.Group != ""})
		}
	}
	return out
}

func init() { hub.ModelsFor = memberModels }

// scopeOf is what keeps one caller's conversations apart from another's
// in everything the gateway remembers by a session or a conversation: the
// member, so a member's devices share a conversation and no two members
// do, else the gateway key; "" for a caller with neither, as it was.
func scopeOf(ctx context.Context) string {
	who := access.Caller(ctx)
	switch {
	case who.Member != "":
		return "m:" + who.Member
	case who.KeyID != "" && mode.IsServer():
		return "k:" + who.KeyID
	}
	return ""
}

// scoped is key told apart by the request's caller: unchanged for a
// caller with no scope.
func scoped(ctx context.Context, key string) string {
	if s := scopeOf(ctx); s != "" {
		return s + "\x00" + key
	}
	return key
}

// hubStart is what a hub does once, as it starts serving: the requests a
// hub stopped in the middle of are written to the ledger.
func hubStart() {
	if !mode.IsServer() {
		return
	}
	if n := hub.Recover(); n > 0 {
		log.Printf("relay: %d request(s) were in flight when the hub last stopped; each is in the ledger as interrupted, counted at what was reserved for it", n)
	}
}

// hubRefusesOpen says why a hub won't serve as it is set to listen: beyond
// this computer with no key asked can't happen on a hub (shared), so this
// is only its own check that it holds.
func hubRefusesOpen() error {
	if mode.IsServer() && OpenToAnyone() {
		return errors.New("a hub never serves without keys: refusing to listen open to anyone")
	}
	return nil
}

// OpenHost: the gateway listens beyond this computer, on an address other
// machines can reach.
func OpenHost() bool {
	h, _, err := net.SplitHostPort(listenAddr())
	if err != nil || h == "localhost" {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}
