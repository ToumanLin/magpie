package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/hubclient"
	"github.com/yetone/magpie/internal/mode"
	"github.com/yetone/magpie/internal/relay"
)

// AI Relay's two programs are this one built in a mode (package mode): Mag,
// the member's client, and Pie, the resource server and its hub. Each
// keeps the commands that are its own and says of the others whose they
// are.

const magUsage = `mag — AI Relay's client: this computer's agents, served by a hub

  mag                             open the app (mag web in a browser, mag tui in the terminal)
  mag hub                         the hub this computer is connected to, who you are there and what you may use
  mag hub connect <address>       connect as a member: asks your name and password, makes this computer a device
  mag hub connect <address> --invite     use an invitation code and choose your password
  mag hub connect <address> --key        connect with a device key made on the hub's page (<address>/relay/)
  mag hub disconnect              forget the hub and this computer's device key
  mag hub usage [today|7d|30d|month|all]   your use, from the hub's ledger
  mag ls | mag <agent> [<model>]  the agents here and the model each uses (a hub's models are hub/…)
  mag models                      every model the hub lets you use
  mag sessions · mag library      this computer's sessions and what is written into its agents
  mag backup [file] · mag restore <file>   the hub connection, agent models, settings and the library, sealed with a passphrase
  mag serve                       the local gateway alone (the app runs it too)

Mag keeps no provider, account or upstream key: those are the hub's.
`

const pieUsage = `pie — AI Relay's server: the providers, accounts and routing members share, and the hub they call

  pie web [--addr host:port] [--lan]     the administrator's page in a browser, with the hub
  pie serve [--listen host:port]         the hub alone (members call it at this address; /relay/ is their page)
  pie relay                              members, groups and plans at a glance
  pie relay group add <name> models=<m1>,<m2>      models as provider/model, provider/* or group/<routing group>
  pie relay group set <id> models=… | rm <id>
  pie relay plan add <name> kind=metered in=<$/M> out=<$/M> [cache-read=…] [budget=<$>] [window=day|week|month]
  pie relay plan add <name> kind=period price=<$> tokens=<n> [window=…] [concurrency=<n>] [queue=<n>]
  pie relay plan rm <id>
  pie relay member add <name> [display=…] [groups=a,b] [plan=<id>]   then: pie relay invite <name>
  pie relay member set <name> [groups=…] [plan=…] [tokens=<n>] [cost=<$>] [period=day|week|month] [concurrency=<n>] [queue=<n>]
  pie relay member off|on|rm <name>
  pie relay invite <name>                an invitation code for the member, shown once, good for 72 hours
  pie relay devices [<name>]             members' devices
  pie relay revoke <device id>           take one device away: its next request is refused
  pie relay usage [today|7d|30d|month|all] [<name>]   every member's use, or one's, from the ledger

  pie providers · pie provider … · pie accounts … · pie groups · pie group … · pie plugin …
                                         the upstream side, as magpie has it (pie help-magpie lists it all)
  pie gateway-key …                      keys of the hub's own, for the administrator: no member's limits hold them
`

// clientOnly and serverOnly are the commands that are one program's.
var (
	serverOnly = []string{"providers", "presets", "provider", "import", "groups", "group", "gateway-key", "accounts", "account", "plugin", "plugins", "search", "quota", "quotas", "relay", "webdav", "dav", "s3"}
	clientOnly = []string{"hub", "ls", "list", "agents", "save", "use", "rm", "profiles", "library", "lib", "sessions", "tui", "visible"}
)

// wrongProgram says, of a command this program doesn't have, which one
// does; nil for a command that is its own.
func wrongProgram(args []string) error {
	if len(args) == 0 {
		return nil
	}
	switch mode.Current() {
	case mode.Client:
		if slices.Contains(serverOnly, args[0]) {
			return fmt.Errorf("mag has no %q: providers, accounts, routing and members are the hub's, kept by its administrator (pie %s)\n\n%s", args[0], args[0], magUsage)
		}
	case mode.Server:
		if slices.Contains(clientOnly, args[0]) {
			return fmt.Errorf("pie has no %q: agents, sessions and the library are a member's own computer's (mag %s)\n\n%s", args[0], args[0], pieUsage)
		}
	default:
		if args[0] == "hub" || args[0] == "relay" {
			return fmt.Errorf("%q is AI Relay's: run mag (the client) or pie (the server), or set AIRELAY_MODE=client|server", args[0])
		}
	}
	return nil
}

// modeUsage is the help of the program this is.
func modeUsage() string {
	switch mode.Current() {
	case mode.Client:
		return magUsage
	case mode.Server:
		return pieUsage
	}
	return usage
}

func pairsOf(args []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("expected key=value, got %q", a)
		}
		out[strings.ToLower(k)] = v
	}
	return out, nil
}

func listOf(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func dollars(v float64) string {
	if v > 0 && v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// relayCmd is `pie relay …`: the administrator's side of the hub.
func relayCmd(args []string) error {
	if len(args) == 0 {
		return relayOverview()
	}
	switch args[0] {
	case "group", "groups":
		return relayGroup(args[1:])
	case "plan", "plans":
		return relayPlan(args[1:])
	case "member", "members":
		return relayMember(args[1:])
	case "invite":
		if len(args) != 2 {
			return errors.New("pie relay invite <name>")
		}
		m, err := memberNamed(args[1])
		if err != nil {
			return err
		}
		code, exp, err := relay.Invite(m.ID)
		if err != nil {
			return err
		}
		fmt.Println(green.Render("✓"), "invitation for", bold.Render(m.Name), muted.Render("· good until "+exp.Format("2006-01-02 15:04")+", once"))
		fmt.Println("  code ", code)
		fmt.Println(muted.Render("  they open " + advertisedURL() + "/relay/#invite=<code> (or paste it there), or run: mag hub connect " + advertisedURL() + " --invite"))
		fmt.Println(muted.Render("  send it privately: whoever has it can take the account until it is used"))
		return nil
	case "devices", "device":
		name := ""
		if len(args) > 1 {
			m, err := memberNamed(args[1])
			if err != nil {
				return err
			}
			name = m.ID
		}
		v, err := relay.Snapshot()
		if err != nil {
			return err
		}
		n := 0
		for _, m := range v.Members {
			if name != "" && m.ID != name {
				continue
			}
			for _, d := range m.Devices {
				n++
				fmt.Printf("  %s  %s  %s  %s\n", pad(bold.Render(m.Name), 16), pad(d.Name, 24), muted.Render(d.Masked), faint.Render(d.ID))
			}
		}
		if n == 0 {
			fmt.Println(muted.Render("no devices yet: a member makes one after signing in"))
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return errors.New("pie relay revoke <device id>   (pie relay devices lists them)")
		}
		if err := access.RemoveDevice("", args[1]); err != nil {
			return err
		}
		fmt.Println(green.Render("✓"), "device revoked", muted.Render("· its next request is refused"))
		return nil
	case "usage":
		return relayUsage(args[1:])
	}
	return fmt.Errorf("unknown: pie relay %s\n\n%s", args[0], pieUsage)
}

func memberNamed(name string) (relay.MemberView, error) {
	v, err := relay.Snapshot()
	if err != nil {
		return relay.MemberView{}, err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	for _, m := range v.Members {
		if m.Name == name || m.ID == name {
			return m, nil
		}
	}
	return relay.MemberView{}, fmt.Errorf("no member %q — pie relay lists them", name)
}

func relayOverview() error {
	v, err := relay.Snapshot()
	if err != nil {
		return err
	}
	fmt.Println(bold.Render("groups"))
	if len(v.Groups) == 0 {
		fmt.Println(muted.Render("  none · pie relay group add lab models=group/coding"))
	}
	for _, g := range v.Groups {
		fmt.Printf("  %s  %s\n", pad(bold.Render(g.ID), 14), strings.Join(g.Models, ", "))
	}
	fmt.Println(bold.Render("plans"))
	if len(v.Plans) == 0 {
		fmt.Println(muted.Render("  none · pie relay plan add …"))
	}
	for _, p := range v.Plans {
		fmt.Printf("  %s  %s\n", pad(bold.Render(p.ID), 14), planLine(p))
	}
	fmt.Println(bold.Render("members"))
	if len(v.Members) == 0 {
		fmt.Println(muted.Render("  none · pie relay member add alice groups=lab"))
	}
	now := time.Now()
	for _, m := range v.Members {
		state := "invited"
		switch {
		case m.Off:
			state = "off"
		case m.HasPassword:
			state = "active"
		case m.Invited == 0:
			state = "not invited"
		}
		line := fmt.Sprintf("  %s  %s  groups %s  plan %s  %d device(s)", pad(bold.Render(m.Name), 16), pad(state, 11), orDash(strings.Join(m.Groups, ",")), orDash(m.Plan), len(m.Devices))
		if st, ok := relay.StandingOf(m.ID, now); ok && st.Limit != nil {
			l := st.Limit
			switch {
			case l.TokenLimit > 0:
				line += muted.Render(fmt.Sprintf("  %d/%d tokens this %s", l.Tokens, l.TokenLimit, l.Period))
			case l.ChargeLimit > 0:
				line += muted.Render(fmt.Sprintf("  %s/%s this %s", dollars(l.Charge), dollars(l.ChargeLimit), l.Period))
			case l.CostLimit > 0:
				line += muted.Render(fmt.Sprintf("  %s/%s est. this %s", dollars(l.Cost), dollars(l.CostLimit), l.Period))
			}
		}
		fmt.Println(line)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func planLine(p relay.Plan) string {
	var parts []string
	if p.Kind == relay.Metered {
		parts = append(parts, fmt.Sprintf("metered · per million tokens: in %s, out %s", dollars(p.PriceIn), dollars(p.PriceOut)))
		if p.Budget > 0 {
			parts = append(parts, fmt.Sprintf("at most %s a %s", dollars(p.Budget), p.Window))
		}
	} else {
		parts = append(parts, fmt.Sprintf("%s a %s", dollars(p.Price), p.Window))
		if p.Tokens > 0 {
			parts = append(parts, fmt.Sprintf("%d tokens included", p.Tokens))
		} else {
			parts = append(parts, "no token limit")
		}
	}
	if p.Concurrency > 0 {
		parts = append(parts, fmt.Sprintf("%d at once", p.Concurrency))
	}
	return strings.Join(parts, " · ")
}

func relayGroup(args []string) error {
	if len(args) < 2 {
		return errors.New("pie relay group add <name> models=<m1>,<m2>  |  set <id> models=…  |  rm <id>")
	}
	switch args[0] {
	case "add", "set":
		kv, err := pairsOf(args[2:])
		if err != nil {
			return err
		}
		g := relay.Group{Name: args[1], Models: listOf(kv["models"])}
		if args[0] == "set" {
			v, err := relay.Snapshot()
			if err != nil {
				return err
			}
			i := slices.IndexFunc(v.Groups, func(o relay.Group) bool { return o.ID == args[1] })
			if i < 0 {
				return fmt.Errorf("no group %q", args[1])
			}
			g = v.Groups[i]
			if m, ok := kv["models"]; ok {
				g.Models = listOf(m)
			}
			if n, ok := kv["name"]; ok {
				g.Name = n
			}
		}
		id, err := relay.SaveGroup(g)
		if err != nil {
			return err
		}
		fmt.Println(green.Render("✓"), "group", bold.Render(id), muted.Render(strings.Join(g.Models, ", ")))
		return nil
	case "rm":
		if err := relay.RemoveGroup(args[1]); err != nil {
			return err
		}
		fmt.Println(green.Render("✓"), "group removed", muted.Render("· its members lose the models it alone gave them"))
		return nil
	}
	return fmt.Errorf("unknown: pie relay group %s", args[0])
}

func num(kv map[string]string, key string, into *float64) error {
	v, ok := kv[key]
	if !ok {
		return nil
	}
	f, err := strconv.ParseFloat(strings.TrimPrefix(v, "$"), 64)
	if err != nil {
		return fmt.Errorf("%s=%q is not a number", key, v)
	}
	*into = f
	return nil
}

func whole(kv map[string]string, key string, into *int64) error {
	v, ok := kv[key]
	if !ok {
		return nil
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(v, "_", ""), 10, 64)
	if err != nil {
		return fmt.Errorf("%s=%q is not a whole number", key, v)
	}
	*into = n
	return nil
}

func relayPlan(args []string) error {
	if len(args) < 2 {
		return errors.New("pie relay plan add <name> kind=metered in=<$/M> out=<$/M>  |  kind=period price=<$> tokens=<n>  |  rm <id>")
	}
	switch args[0] {
	case "add":
		kv, err := pairsOf(args[2:])
		if err != nil {
			return err
		}
		p := relay.Plan{Name: args[1], Kind: kv["kind"], Window: kv["window"], CacheReads: kv["cache-reads"] == "yes" || kv["cache-reads"] == "true"}
		var conc, queue int64
		for _, e := range []error{num(kv, "in", &p.PriceIn), num(kv, "out", &p.PriceOut), num(kv, "cache-read", &p.PriceCacheRead), num(kv, "cache-write", &p.PriceCacheWrite),
			num(kv, "budget", &p.Budget), num(kv, "price", &p.Price), whole(kv, "tokens", &p.Tokens), whole(kv, "concurrency", &conc), whole(kv, "queue", &queue)} {
			if e != nil {
				return e
			}
		}
		p.Concurrency, p.Queue = int(conc), int(queue)
		id, err := relay.SavePlan(p)
		if err != nil {
			return err
		}
		p.ID = id
		if p.Window == "" {
			p.Window = "month"
		}
		fmt.Println(green.Render("✓"), "plan", bold.Render(id), muted.Render(planLine(p)))
		return nil
	case "rm":
		if err := relay.RemovePlan(args[1]); err != nil {
			return err
		}
		fmt.Println(green.Render("✓"), "plan removed")
		return nil
	}
	return fmt.Errorf("unknown: pie relay plan %s", args[0])
}

func relayMember(args []string) error {
	if len(args) < 2 {
		return errors.New("pie relay member add <name> [groups=a,b] [plan=<id>]  |  set <name> k=v…  |  off|on|rm <name>")
	}
	if args[0] == "add" {
		kv, err := pairsOf(args[2:])
		if err != nil {
			return err
		}
		in := relay.MemberIn{Name: args[1], Display: kv["display"], Groups: listOf(kv["groups"]), Plan: kv["plan"]}
		if err := memberPairs(&in, kv); err != nil {
			return err
		}
		if _, err := relay.SaveMember(in); err != nil {
			return err
		}
		fmt.Println(green.Render("✓"), "member", bold.Render(strings.ToLower(args[1])), muted.Render("· pie relay invite "+strings.ToLower(args[1])+" makes their invitation"))
		return nil
	}
	m, err := memberNamed(args[1])
	if err != nil {
		return err
	}
	in := relay.MemberIn{ID: m.ID, Name: m.Name, Display: m.Display, Off: m.Off, Groups: m.Groups, Plan: m.Plan, Limit: m.Limit, Concurrency: m.Concurrency, Queue: m.Queue}
	switch args[0] {
	case "set":
		kv, err := pairsOf(args[2:])
		if err != nil {
			return err
		}
		if v, ok := kv["groups"]; ok {
			in.Groups = listOf(v)
		}
		if v, ok := kv["plan"]; ok {
			in.Plan = v
		}
		if v, ok := kv["display"]; ok {
			in.Display = v
		}
		if err := memberPairs(&in, kv); err != nil {
			return err
		}
	case "off", "on":
		in.Off = args[0] == "off"
	case "rm":
		if err := relay.RemoveMember(m.ID); err != nil {
			return err
		}
		fmt.Println(green.Render("✓"), "member removed, with its devices", muted.Render("· its use stays in the ledger"))
		return nil
	default:
		return fmt.Errorf("unknown: pie relay member %s", args[0])
	}
	if _, err := relay.SaveMember(in); err != nil {
		return err
	}
	fmt.Println(green.Render("✓"), "member", bold.Render(m.Name), muted.Render("· holds from its next request"))
	return nil
}

// memberPairs reads a member's own limit and concurrency from k=v pairs:
// tokens= and cost= (0 for none) with period=, concurrency=, queue=.
func memberPairs(in *relay.MemberIn, kv map[string]string) error {
	var conc, queue int64 = int64(in.Concurrency), int64(in.Queue)
	if err := whole(kv, "concurrency", &conc); err != nil {
		return err
	}
	if err := whole(kv, "queue", &queue); err != nil {
		return err
	}
	in.Concurrency, in.Queue = int(conc), int(queue)
	_, hasT := kv["tokens"]
	_, hasC := kv["cost"]
	_, hasP := kv["period"]
	if !hasT && !hasC && !hasP {
		return nil
	}
	l := access.Limit{}
	if in.Limit != nil {
		l = *in.Limit
	}
	if err := whole(kv, "tokens", &l.Tokens); err != nil {
		return err
	}
	if err := num(kv, "cost", &l.Cost); err != nil {
		return err
	}
	if hasP {
		l.Period = kv["period"]
	}
	in.Limit = &l
	return nil
}

func totalsLine(t relay.Totals) string {
	s := fmt.Sprintf("%d calls · in %d · out %d", t.Calls, t.Input, t.Output)
	if t.CacheRead > 0 {
		s += fmt.Sprintf(" · cache read %d", t.CacheRead)
	}
	if t.Unknown > 0 {
		s += fmt.Sprintf(" · %d with no usage reported, counted at %d reserved", t.Unknown, t.Est)
	}
	if t.Interrupted > 0 {
		s += fmt.Sprintf(" · %d interrupted", t.Interrupted)
	}
	if t.Refused > 0 {
		s += fmt.Sprintf(" · %d refused", t.Refused)
	}
	if t.Charge > 0 {
		s += " · charged " + dollars(t.Charge)
	}
	return s
}

func relayUsage(args []string) error {
	period, name := "30d", ""
	for _, a := range args {
		if slices.Contains(relay.Periods, a) {
			period = a
		} else {
			name = a
		}
	}
	now := time.Now()
	if name != "" {
		m, err := memberNamed(name)
		if err != nil {
			return err
		}
		st, err := relay.StatementOf(m.ID, period, now, true)
		if err != nil {
			return err
		}
		printStatement(st, true)
		return nil
	}
	lines, all := relay.Overview(period, now)
	if len(lines) == 0 {
		fmt.Println(muted.Render("no member has used the hub in this time (" + period + ")"))
		return nil
	}
	for _, l := range lines {
		fmt.Printf("  %s  %s  %s\n", pad(bold.Render(l.Name), 16), totalsLine(l.Totals), muted.Render("est. upstream "+dollars(l.Cost)))
	}
	fmt.Printf("  %s  %s  %s\n", pad(bold.Render("all"), 16), totalsLine(all), muted.Render("est. upstream "+dollars(all.Cost)))
	return nil
}

func printStatement(st relay.Statement, admin bool) {
	fmt.Println(bold.Render(st.Name), muted.Render(st.Period), totalsLine(st.Totals))
	if st.Fee > 0 {
		fmt.Println("  plan fee", dollars(st.Fee), muted.Render("· total "+dollars(st.Due)))
	}
	if admin && st.Cost > 0 {
		fmt.Println(muted.Render("  est. upstream cost " + dollars(st.Cost)))
	}
	for _, l := range st.ByModel {
		fmt.Printf("  %s  %s\n", pad(l.Key, 36), muted.Render(totalsLine(l.Totals)))
	}
}

// hubCmd is `mag hub …`: this computer's connection to a hub.
func hubCmd(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if len(args) == 0 || args[0] == "status" {
		return hubStatus(ctx)
	}
	switch args[0] {
	case "connect":
		return hubConnect(ctx, args[1:])
	case "disconnect":
		if err := hubclient.Disconnect(); err != nil {
			return err
		}
		fmt.Println(green.Render("✓"), "disconnected", muted.Render("· the device stays at the hub until it is revoked there (its page, Devices)"))
		return nil
	case "usage":
		period := "30d"
		if len(args) > 1 {
			period = args[1]
		}
		st, err := hubclient.Usage(ctx, period)
		if err != nil {
			return err
		}
		printStatement(st, false)
		return nil
	}
	return fmt.Errorf("unknown: mag hub %s\n\n%s", args[0], magUsage)
}

func hubConnect(ctx context.Context, args []string) error {
	addr, how, device, name := "", "login", "", ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--key":
			how = "key"
		case a == "--invite":
			how = "invite"
		case a == "--device" && i+1 < len(args):
			i++
			device = args[i]
		case a == "--name" && i+1 < len(args):
			i++
			name = args[i]
		case strings.HasPrefix(a, "--"):
			return fmt.Errorf("mag hub connect: unknown %q", a)
		default:
			addr = a
		}
	}
	if addr == "" {
		return errors.New("mag hub connect <address> [--invite | --key] [--name <you>] [--device <name>]")
	}
	root, err := hubclient.Root(addr)
	if err != nil {
		return err
	}
	if hubclient.Plain(root) {
		fmt.Fprintln(os.Stderr, amber.Render("!"), "this address is plain http: your password and device key cross the network unencrypted. Use it only over a VPN or a network you trust; an https address is the hub's to offer")
	}
	// what is secret is read without echo, or from stdin when piped: never an argument
	switch how {
	case "key":
		key, err := secret("device key", "Device key: ", false)
		if err != nil {
			return err
		}
		err = hubclient.ConnectKey(ctx, root, key)
		if err != nil {
			return err
		}
	case "invite":
		code, err := secret("invitation code", "Invitation code: ", false)
		if err != nil {
			return err
		}
		pass, err := secret("password", "Choose a password (10 characters or more): ", true)
		if err != nil {
			return err
		}
		if err := hubclient.ConnectInvite(ctx, root, code, pass, device); err != nil {
			return err
		}
	default:
		if name == "" {
			fmt.Fprint(os.Stderr, "Name: ")
			line, err := stdin.ReadString('\n')
			if name = strings.TrimSpace(line); name == "" {
				return fmt.Errorf("no name given: %v", err)
			}
		}
		pass, err := secret("password", "Password: ", false)
		if err != nil {
			return err
		}
		if err := hubclient.ConnectLogin(ctx, root, name, pass, device); err != nil {
			return err
		}
	}
	fmt.Println(green.Render("✓"), "connected to", bold.Render(root))
	return hubStatus(ctx)
}

func hubStatus(ctx context.Context) error {
	st := hubclient.StatusNow(ctx)
	if !st.Connected {
		fmt.Println(muted.Render("not connected to a hub · mag hub connect <address>"))
		return nil
	}
	fmt.Println(bold.Render("hub"), st.Address)
	if st.Plain {
		fmt.Println(amber.Render("!"), "plain http: the device key crosses the network unencrypted")
	}
	if st.Error != "" {
		if st.Revoked {
			return fmt.Errorf("the hub no longer takes this computer's device key (%s): it was revoked, or the member turned off. Connect again: mag hub connect %s", st.Error, st.Address)
		}
		return errors.New(st.Error)
	}
	m := st.Member
	fmt.Println("  member ", bold.Render(m.Name), muted.Render("· device "+st.Device))
	var groups []string
	for _, g := range m.Groups {
		groups = append(groups, g.Name)
	}
	fmt.Println("  groups ", orDash(strings.Join(groups, ", ")))
	if m.Plan != nil {
		fmt.Println("  plan   ", m.Plan.Name, muted.Render("· "+planLine(*m.Plan)))
	}
	if l := m.Limit; l != nil {
		switch {
		case l.TokenLimit > 0:
			fmt.Printf("  limit   %d of %d tokens this %s %s\n", l.Tokens, l.TokenLimit, l.Period, muted.Render("· resets "+l.Reset.Format("2006-01-02 15:04")))
		case l.ChargeLimit > 0:
			fmt.Printf("  limit   %s of %s this %s %s\n", dollars(l.Charge), dollars(l.ChargeLimit), l.Period, muted.Render("· resets "+l.Reset.Format("2006-01-02 15:04")))
		}
	}
	fmt.Printf("  at once %d %s\n", m.Concurrency, muted.Render(fmt.Sprintf("· %d may wait", m.Queue)))
	fmt.Println("  models ", len(st.Models))
	for _, mo := range st.Models {
		fmt.Println("   ", hubclient.ProviderID+"/"+mo.ID)
	}
	fmt.Println(muted.Render("  agents here reach them through the local gateway at " + gateway.URL() + " · mag <agent> " + hubclient.ProviderID + "/<model>"))
	return nil
}
