package relay

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
)

// The member's side of the hub, served by the hub gateway beside its APIs
// under /relay/: a web page for a member who installs nothing, and the
// JSON it and Mag ask. A member is who the request's session (a cookie the
// page holds, or a bearer token Mag was given at sign-in) or device key
// says: every answer is of that member alone, and nothing here takes a
// member's id from the request.
//
//	GET  /relay/                     the page
//	POST /relay/api/redeem           {code, password}  an invitation used
//	POST /relay/api/login            {name, password}
//	POST /relay/api/logout
//	POST /relay/api/password         {old, password}
//	GET  /relay/api/me               the member's standing
//	GET  /relay/api/models           the models the member may use
//	GET  /relay/api/usage?period=    the member's statement
//	GET  /relay/api/devices          the member's devices
//	POST /relay/api/devices          {name}  a new device; its key, once
//	POST /relay/api/devices/revoke   {id}
//
// A device key reads (me, models, usage, devices). Making or revoking a
// device and changing the password need a session: a key that leaked can
// neither make more keys nor lock its member out.

//go:embed portal.html
var portal []byte

// Model is a model a member may use, as the page lists it.
type Model struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Group bool   `json:"group,omitempty"`
}

// ModelsFor lists the models who may use now. The gateway sets it: the
// same filter its own model lists go through.
var ModelsFor = func(who access.Identity) []Model { return nil }

const cookieName = "airelay_session"

// Register adds the member's routes to the hub gateway's mux.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /relay", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/relay/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /relay/{$}", func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Write(portal)
	})
	mux.HandleFunc("POST /relay/api/redeem", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Code, Password string }
		if !body(w, r, &in) {
			return
		}
		token, m, err := Redeem(in.Code, in.Password, from(r))
		if err != nil {
			refuse(w, err)
			return
		}
		signedIn(w, r, token, m)
	})
	mux.HandleFunc("POST /relay/api/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Password string }
		if !body(w, r, &in) {
			return
		}
		token, m, err := Login(in.Name, in.Password)
		if err != nil {
			refuse(w, err)
			return
		}
		signedIn(w, r, token, m)
	})
	mux.HandleFunc("POST /relay/api/logout", func(w http.ResponseWriter, r *http.Request) {
		if !body(w, r, nil) {
			return
		}
		if c := caller(r); c.session != "" {
			_ = Logout(c.session)
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/relay", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		reply(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /relay/api/password", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Old, Password string }
		if !body(w, r, &in) {
			return
		}
		c, ok := signed(w, r)
		if !ok {
			return
		}
		if err := ChangePassword(c.member, in.Old, in.Password, c.session); err != nil {
			refuse(w, err)
			return
		}
		reply(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /relay/api/me", func(w http.ResponseWriter, r *http.Request) {
		c, ok := known(w, r)
		if !ok {
			return
		}
		st, ok := StandingOf(c.member, time.Now())
		if !ok {
			fail(w, http.StatusUnauthorized, "Sign in again")
			return
		}
		reply(w, 200, map[string]any{"member": st, "session": c.session != "", "device": c.who.KeyName})
	})
	mux.HandleFunc("GET /relay/api/models", func(w http.ResponseWriter, r *http.Request) {
		c, ok := known(w, r)
		if !ok {
			return
		}
		ms := ModelsFor(c.who)
		if ms == nil {
			ms = []Model{}
		}
		reply(w, 200, map[string]any{"models": ms})
	})
	mux.HandleFunc("GET /relay/api/usage", func(w http.ResponseWriter, r *http.Request) {
		c, ok := known(w, r)
		if !ok {
			return
		}
		s, err := StatementOf(c.member, r.URL.Query().Get("period"), time.Now(), false)
		if err != nil {
			refuse(w, err)
			return
		}
		reply(w, 200, s)
	})
	mux.HandleFunc("GET /relay/api/devices", func(w http.ResponseWriter, r *http.Request) {
		c, ok := known(w, r)
		if !ok {
			return
		}
		ds, err := access.Devices(c.member)
		if err != nil {
			refuse(w, err)
			return
		}
		reply(w, 200, map[string]any{"devices": devicesOut(ds), "current": c.who.KeyID})
	})
	mux.HandleFunc("POST /relay/api/devices", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name string }
		if !body(w, r, &in) {
			return
		}
		c, ok := signed(w, r)
		if !ok {
			return
		}
		ds, err := access.Devices(c.member)
		if err != nil {
			refuse(w, err)
			return
		}
		if len(ds) >= MaxDevices {
			refuse(w, errors.New("You have as many devices as a member can: revoke one you no longer use"))
			return
		}
		id, secret, err := access.AddDevice(c.member, in.Name, time.Now().Unix())
		if err != nil {
			refuse(w, err)
			return
		}
		reply(w, 200, map[string]any{"id": id, "name": strings.TrimSpace(in.Name), "key": secret})
	})
	mux.HandleFunc("POST /relay/api/devices/revoke", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ ID string }
		if !body(w, r, &in) {
			return
		}
		c, ok := signed(w, r)
		if !ok {
			return
		}
		// only a device of the member's own: another's id is not found
		if err := access.RemoveDevice(c.member, in.ID); err != nil {
			refuse(w, err)
			return
		}
		reply(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("/relay/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, "Not found")
	})
}

// MaxDevices is how many devices a member can have at once.
const MaxDevices = 20

type deviceOut struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Masked  string `json:"masked"`
	Created int64  `json:"created,omitempty"`
}

func devicesOut(ds []access.Key) []deviceOut {
	out := make([]deviceOut, 0, len(ds))
	for _, d := range ds {
		out = append(out, deviceOut{ID: d.ID, Name: d.Name, Masked: d.Masked, Created: d.Created})
	}
	return out
}

// who a request is from: the member, by a session or by a device key.
type from_ struct {
	member  string
	session string // the session's token, "" when a device key signed it
	who     access.Identity
}

// caller reads who a request is from: a session in the page's cookie or a
// bearer token, else a device key as the gateway takes one.
func caller(r *http.Request) from_ {
	// a device key the gateway's guard took already (its token is in the
	// key's place by now)
	if who := access.Caller(r.Context()); who.Member != "" {
		return from_{member: who.Member, who: who}
	}
	var tokens []string
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		tokens = append(tokens, c.Value)
	}
	a := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
		tokens = append(tokens, strings.TrimSpace(a[7:]))
	}
	if k := strings.TrimSpace(r.Header.Get("x-api-key")); k != "" {
		tokens = append(tokens, k)
	}
	for _, t := range tokens {
		if strings.HasPrefix(t, SessionPrefix) {
			if m, ok := SessionMember(t); ok {
				who, _ := memberOf(access.Key{Member: m.ID})
				return from_{member: m.ID, session: t, who: who}
			}
			continue
		}
		if who, ok := access.Authenticate(t); ok && who.Member != "" {
			return from_{member: who.Member, who: who}
		}
	}
	return from_{}
}

// known is a request from a member, however it says so.
func known(w http.ResponseWriter, r *http.Request) (from_, bool) {
	c := caller(r)
	if c.member == "" {
		fail(w, http.StatusUnauthorized, "Sign in, or send a device key as Authorization: Bearer <key>")
		return c, false
	}
	return c, true
}

// signed is a request from a member signed in: a device key is not
// enough.
func signed(w http.ResponseWriter, r *http.Request) (from_, bool) {
	c := caller(r)
	if c.member == "" || c.session == "" {
		fail(w, http.StatusUnauthorized, "Sign in with your name and password to do this: a device key can't")
		return c, false
	}
	return c, true
}

// body reads a POST's JSON into v (nil for none). It takes only what the
// hub's own page or Mag sends: JSON, with the header a page of another
// site can't add without asking, so the cookie can't be used from one.
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Header.Get("X-Relay") == "" || !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		fail(w, http.StatusBadRequest, "Send JSON with the X-Relay header")
		return false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		fail(w, http.StatusBadRequest, "The request could not be read")
		return false
	}
	if v == nil {
		return true
	}
	if len(b) == 0 || json.Unmarshal(b, v) != nil {
		fail(w, http.StatusBadRequest, "The request is not JSON")
		return false
	}
	return true
}

func signedIn(w http.ResponseWriter, r *http.Request, token string, m Member) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/relay", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"), MaxAge: int(SessionLife / time.Second)})
	reply(w, 200, map[string]any{"token": token, "member": map[string]any{"id": m.ID, "name": m.Name, "display": m.Display}})
}

// from is where a request came from, for the limit on wrong tries: the
// address that connected, whatever a header says — unless
// MAGPIE_TRUST_PROXY=1 says a proxy on this computer passes its clients
// on, as the gateway takes it: then it is the client the proxy names
// (X-Forwarded-For's first), or every member behind the proxy would share
// its address and its five wrong tries.
func from(r *http.Request) string {
	if trust, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("MAGPIE_TRUST_PROXY"))); trust {
		first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
		if first = strings.TrimSpace(first); first != "" && len(first) <= 64 {
			return first
		}
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func reply(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]any{"error": msg})
}

func refuse(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrThrottled):
		status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", "900")
	case errors.Is(err, errSignIn):
		status = http.StatusUnauthorized
	}
	fail(w, status, err.Error())
}
