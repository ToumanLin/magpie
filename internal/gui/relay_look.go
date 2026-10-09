//go:build !nogui

package gui

import (
	_ "embed"

	"github.com/yetone/magpie/internal/mode"
)

// Mag's and Pie's own icons: magpie's bird on a tinted tile with the
// program's letter, so the two tell apart in the taskbar and the tray
// (drawn by AI Relay's scripts/make-icons.py).
//
//go:embed icon-mag.png
var magIcon []byte

//go:embed icon-mag-1024.png
var magIconLarge []byte

//go:embed icon-pie.png
var pieIcon []byte

//go:embed icon-pie-1024.png
var pieIconLarge []byte

// lookSmall is the running program's tray icon.
func lookSmall() []byte {
	switch mode.Current() {
	case mode.Client:
		return magIcon
	case mode.Server:
		return pieIcon
	}
	return appIcon
}

// lookLarge is the running program's window and taskbar icon.
func lookLarge() []byte {
	switch mode.Current() {
	case mode.Client:
		return magIconLarge
	case mode.Server:
		return pieIconLarge
	}
	return appIconLarge
}
