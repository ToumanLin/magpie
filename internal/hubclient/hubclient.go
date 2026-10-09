// Package hubclient is Mag's side of an AI Relay hub: it connects this
// computer to one, as a member's device, and asks the hub what the member
// may use and has used.
//
// The connection is a Remote magpie provider (provider.RemoteMagpiePreset)
// with the id "hub": the local gateway sends every request the agents make
// on to the hub with the device key, as it does to any remote magpie, so
// nothing here translates or routes. The device key is the only credential
// Mag keeps, in providers.json as every provider's key is; the member's
// password is used once, to make the device, and is never written.
package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/relay"
)

// ProviderID is the id of the provider that is the hub.
const ProviderID = "hub"

// client asks the hub; a test's own.
var client = &http.Client{Timeout: 20 * time.Second, Transport: netproxy.Dispatch(&http.Transport{Proxy: netproxy.Func, ResponseHeaderTimeout: 15 * time.Second})}

// Root is a hub's address as it is kept: with a scheme, and without the
// path of any API a user may have pasted with it.
func Root(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	// a scheme with nothing after it stays one, and is refused below
	hasScheme := strings.Contains(addr, "://")
	addr = strings.TrimRight(addr, "/")
	if addr == "" {
		return "", errors.New("Enter the hub's address, such as https://relay.example.org or http://192.168.1.20:3425")
	}
	if !hasScheme {
		addr = "http://" + addr
	}
	for _, suf := range []string{"/relay", "/v1/messages", "/v1/chat/completions", "/v1/responses", "/v1"} {
		if b, ok := strings.CutSuffix(addr, suf); ok {
			addr = b
			break
		}
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("The hub's address is an http or https address with no user, query or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// Plain says the address carries the device key unencrypted over a
// network: plain http to somewhere that is not this computer.
func Plain(root string) bool {
	u, err := url.Parse(root)
	if err != nil || u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}

func ask(ctx context.Context, method, root, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, root+path, body)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Relay", "1")
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("The hub at %s could not be reached: %w", root, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error any `json:"error"`
		}
		msg := ""
		if json.Unmarshal(b, &e) == nil {
			switch v := e.Error.(type) {
			case string:
				msg = v
			case map[string]any:
				msg, _ = v["message"].(string)
			}
		}
		if msg == "" {
			msg = fmt.Sprintf("the hub answered HTTP %d", res.StatusCode)
		}
		return &Refused{Status: res.StatusCode, Msg: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("The hub at %s answered something that is not JSON: is it an AI Relay hub?", root)
	}
	return nil
}

// Refused is the hub's own refusal, in its words.
type Refused struct {
	Status int
	Msg    string
}

func (r *Refused) Error() string { return r.Msg }

// Probe checks that an AI Relay hub answers at the address.
func Probe(ctx context.Context, root string) error {
	var info struct {
		Name  string `json:"name"`
		Relay string `json:"relay"`
	}
	if err := ask(ctx, "GET", root, "/", "", nil, &info); err != nil {
		return err
	}
	if info.Relay != "hub" {
		return fmt.Errorf("%s answers, but not as an AI Relay hub: check the address", root)
	}
	return nil
}

// DeviceName is what this computer's device is called at the hub unless
// the member names it: the computer's name.
func DeviceName() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	if h = strings.TrimSpace(h); h == "" {
		h = "mag"
	}
	return h
}

// ConnectKey connects with a device key the member made on the hub's page.
func ConnectKey(ctx context.Context, addr, key string) error {
	root, err := Root(addr)
	if err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("Enter the device key")
	}
	if err := Probe(ctx, root); err != nil {
		return err
	}
	var me struct {
		Member relay.Standing `json:"member"`
	}
	if err := ask(ctx, "GET", root, "/relay/api/me", key, nil, &me); err != nil {
		return err
	}
	return keep(ctx, root, key)
}

// ConnectLogin signs the member in with their password, makes a device
// for this computer and signs out again: the device key is kept, the
// password and the sign-in are not.
func ConnectLogin(ctx context.Context, addr, name, pass, device string) error {
	return signInAnd(ctx, addr, device, "/relay/api/login", map[string]string{"name": name, "password": pass})
}

// ConnectInvite uses an invitation, with the password the member chooses,
// then does as ConnectLogin.
func ConnectInvite(ctx context.Context, addr, code, pass, device string) error {
	return signInAnd(ctx, addr, device, "/relay/api/redeem", map[string]string{"code": code, "password": pass})
}

func signInAnd(ctx context.Context, addr, device, path string, in map[string]string) error {
	root, err := Root(addr)
	if err != nil {
		return err
	}
	if device = strings.TrimSpace(device); device == "" {
		device = DeviceName()
	}
	if err := Probe(ctx, root); err != nil {
		return err
	}
	var signed struct {
		Token string `json:"token"`
	}
	if err := ask(ctx, "POST", root, path, "", in, &signed); err != nil {
		return err
	}
	// signed out whatever comes next: the session was only for this
	defer ask(context.WithoutCancel(ctx), "POST", root, "/relay/api/logout", signed.Token, map[string]string{}, nil)
	var dev struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := ask(ctx, "POST", root, "/relay/api/devices", signed.Token, map[string]string{"name": device}, &dev); err != nil {
		return err
	}
	if err := keep(ctx, root, dev.Key); err != nil {
		// a device nobody has the key of is taken away again
		_ = ask(ctx, "POST", root, "/relay/api/devices/revoke", signed.Token, map[string]string{"id": dev.ID}, nil)
		return err
	}
	return nil
}

// keep saves the hub as the provider the gateway sends on to, and asks it
// for its models.
func keep(ctx context.Context, root, key string) error {
	p, err := provider.FromPreset(provider.RemoteMagpiePreset)
	if err != nil {
		return err
	}
	if cur, ok := Current(); ok {
		p = cur // its picks and order stay through a reconnect
	}
	p.ID, p.Name, p.Preset = ProviderID, "AI Relay Hub", provider.RemoteMagpiePreset
	p.Chat, p.Responses, p.Anthropic = root, "", ""
	p.Key, p.Keys = key, nil
	if err := provider.Save(p); err != nil {
		return err
	}
	saved, err := provider.Find(ProviderID)
	if err != nil {
		return err
	}
	if _, err := saved.Fetch(ctx); err != nil {
		return fmt.Errorf("Connected, but the hub's model list could not be read: %w", err)
	}
	return nil
}

// Current is the hub this computer is connected to.
func Current() (provider.Provider, bool) {
	for _, p := range provider.All() {
		if p.ID == ProviderID && p.IsRemoteMagpie() {
			return p, true
		}
	}
	return provider.Provider{}, false
}

// Disconnect forgets the hub and the device key. The device stays at the
// hub until the member, or the administrator, takes it away there.
func Disconnect() error {
	if _, ok := Current(); !ok {
		return nil
	}
	return provider.Delete(ProviderID)
}

// Status is the connection as Mag shows it.
type Status struct {
	Connected bool   `json:"connected"`
	Address   string `json:"address,omitempty"`
	// Plain: the address carries the key unencrypted over a network
	Plain  bool            `json:"plain,omitempty"`
	Member *relay.Standing `json:"member,omitempty"`
	Device string          `json:"device,omitempty"`
	Models []relay.Model   `json:"models,omitempty"`
	// Error is why the hub didn't answer as the member's, when it didn't:
	// the key taken away, the member turned off, the hub not reached.
	Error string `json:"error,omitempty"`
	// Revoked: the hub says the key is no longer one of its own
	Revoked bool `json:"revoked,omitempty"`
}

// StatusNow asks the hub who this device is and what it may use.
func StatusNow(ctx context.Context) Status {
	p, ok := Current()
	if !ok {
		return Status{}
	}
	root := strings.TrimRight(p.Anthropic, "/")
	st := Status{Connected: true, Address: root, Plain: Plain(root)}
	var me struct {
		Member relay.Standing `json:"member"`
		Device string         `json:"device"`
	}
	if err := ask(ctx, "GET", root, "/relay/api/me", p.Key, nil, &me); err != nil {
		st.Error = err.Error()
		var r *Refused
		st.Revoked = errors.As(err, &r) && r.Status == http.StatusUnauthorized
		return st
	}
	st.Member, st.Device = &me.Member, me.Device
	var ms struct {
		Models []relay.Model `json:"models"`
	}
	if err := ask(ctx, "GET", root, "/relay/api/models", p.Key, nil, &ms); err != nil {
		st.Error = err.Error()
		return st
	}
	st.Models = ms.Models
	return st
}

// Usage is the member's statement at the hub over a period: the hub's own
// ledger, the one its administrator's totals are of.
func Usage(ctx context.Context, period string) (relay.Statement, error) {
	p, ok := Current()
	if !ok {
		return relay.Statement{}, errors.New("This computer is not connected to a hub")
	}
	var s relay.Statement
	err := ask(ctx, "GET", strings.TrimRight(p.Anthropic, "/"), "/relay/api/usage?period="+url.QueryEscape(period), p.Key, nil, &s)
	return s, err
}
