// Package mode says which part of AI Relay this process is. One source
// tree builds three programs: magpie as it always was (no mode), Mag, the
// member's client (Client), and Pie, the resource server and its hub
// gateway (Server). Mag wires the agents on a member's computer to its
// local gateway, which sends every request on to a hub with the member's
// device key, and keeps no upstream account. Pie keeps the providers, the
// accounts and the routing, and answers members by their device keys.
//
// The mode is the program's own: Default is set when it is built
// (-ldflags "-X github.com/yetone/magpie/internal/mode.Default=client"),
// and AIRELAY_MODE says it for a magpie built without one. It is never
// read from a settings file, so a restored backup or a synced setting
// can't turn a client into a server.
package mode

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

const (
	Client = "client"
	Server = "server"
)

// Default is the mode this program was built as: "client", "server", or ""
// for magpie itself.
var Default = ""

var (
	mu      sync.RWMutex
	forced  string
	isSet   bool
	fromEnv = sync.OnceValue(func() string { return valid(os.Getenv("AIRELAY_MODE")) })
)

func valid(m string) string {
	switch m = strings.ToLower(strings.TrimSpace(m)); m {
	case Client, "mag":
		return Client
	case Server, "pie":
		return Server
	}
	return ""
}

// Current is the mode: what Set said, else the one built in, else
// AIRELAY_MODE's.
func Current() string {
	mu.RLock()
	f, ok := forced, isSet
	mu.RUnlock()
	if ok {
		return f
	}
	if d := valid(Default); d != "" {
		return d
	}
	return fromEnv()
}

// Set makes the mode m for this process, and returns what puts it back: a
// test's own.
func Set(m string) (restore func()) {
	mu.Lock()
	was, had := forced, isSet
	forced, isSet = valid(m), true
	mu.Unlock()
	return func() {
		mu.Lock()
		forced, isSet = was, had
		mu.Unlock()
	}
}

// IsClient: this is Mag.
func IsClient() bool { return Current() == Client }

// IsServer: this is Pie.
func IsServer() bool { return Current() == Server }

// App is the program's name as its messages say it.
func App() string {
	switch Current() {
	case Client:
		return "mag"
	case Server:
		return "pie"
	}
	return "magpie"
}

// Title is the program's name as its window, tray and page show it.
func Title() string {
	switch Current() {
	case Client:
		return "Mag"
	case Server:
		return "Pie"
	}
	return "magpie"
}

// Folder is the name of the folder the program keeps its files under: Mag
// and Pie each have their own, so neither reads the other's providers and
// keys, nor a magpie's that was on the computer before them.
func Folder() string {
	switch Current() {
	case Client:
		return "airelay-mag"
	case Server:
		return "airelay-pie"
	}
	return "magpie"
}

// LocalOpen: AIRELAY_LOCAL_OPEN=1 lets requests from the hub's own
// computer in without a key, as magpie's gateway does, for an
// administrator trying a provider there. Off unless said: a hub signs
// every request.
func LocalOpen() bool {
	v, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("AIRELAY_LOCAL_OPEN")))
	return v
}
