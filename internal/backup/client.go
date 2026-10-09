package backup

import (
	"slices"

	"github.com/yetone/magpie/internal/provider"
)

// ForClient leaves in a bundle what is Mag's, AI Relay's client (package
// mode): the connection to its hub, with this computer's device key when
// the bundle has keys, the agents' models, the profiles, the library and
// the settings. Everything that is a server's goes — any other provider
// and its key, routing groups, gateway keys, the web search keys — and so
// do the settings' own credentials, which the restore then keeps as the
// computer has them.
//
// Mag applies it to what it collects and to what it is given to restore:
// a backup made by a magpie or a Pie, put into a Mag, brings in no
// upstream account's key.
func (b *Bundle) ForClient() {
	b.Providers = slices.DeleteFunc(slices.Clone(b.Providers), func(p provider.Provider) bool { return !p.IsRemoteMagpie() })
	kept := map[string]bool{}
	for _, p := range b.Providers {
		kept[p.ID] = true
	}
	b.Order = slices.DeleteFunc(slices.Clone(b.Order), func(id string) bool { return !kept[id] })
	b.Icons, b.Groups, b.GroupOrder, b.GatewayKeys, b.Searches = nil, nil, nil, nil, nil
	if b.Settings != nil {
		s := *b.Settings
		s.LAN, s.LANKey, s.LANKeyID, s.GitHubToken = false, "", "", ""
		s.CORSOrigins = nil
		s.OTel.Headers = nil
		b.Settings = &s
	}
	// no settings credential is in it, whatever the bundle's own policy says
	b.SettingsKeys = Flag(false)
}
