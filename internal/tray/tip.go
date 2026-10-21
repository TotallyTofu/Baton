//go:build windows

package tray

import (
	"os"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

// fyne.io/systray declares NOTIFYICONDATAW with uTimeout and uVersion as two
// fields instead of a union, so its cbSize matches no version Explorer knows
// and the tooltip falls back to the pre-Windows 2000 limit of 63 characters
// (fyne-io/systray#133). setTooltip sends the tip again on systray's icon with
// the real layout, so all 127 fit.

type notifyIconData struct {
	Size                       uint32
	Wnd                        windows.HWND
	ID, Flags, CallbackMessage uint32
	Icon                       windows.Handle
	Tip                        [128]uint16
	State, StateMask           uint32
	Info                       [256]uint16
	TimeoutOrVersion           uint32 // a union in C
	InfoTitle                  [64]uint16
	InfoFlags                  uint32
	GuidItem                   windows.GUID
	BalloonIcon                windows.Handle
}

const (
	nimModify     = 0x1
	nifTip        = 0x4
	systrayIconID = 100 // the uID systray gives its icon
)

var (
	shell32         = windows.NewLazySystemDLL("shell32.dll")
	shellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")

	trayWnd  windows.HWND
	findTray = windows.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		var pid uint32
		windows.GetWindowThreadProcessId(h, &pid)
		if pid != uint32(os.Getpid()) {
			return 1
		}
		var buf [32]uint16
		n, _ := windows.GetClassName(h, &buf[0], int32(len(buf)))
		if windows.UTF16ToString(buf[:n]) != "SystrayClass" {
			return 1
		}
		trayWnd = h
		return 0
	})
)

func setTooltip(s string) {
	systray.SetTooltip(s) // systray keeps its copy and resends it with every icon change
	if trayWnd == 0 {
		windows.EnumWindows(findTray, nil)
		if trayWnd == 0 {
			return
		}
	}
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	nid := notifyIconData{Wnd: trayWnd, ID: systrayIconID, Flags: nifTip}
	nid.Size = uint32(unsafe.Sizeof(nid))
	copy(nid.Tip[:len(nid.Tip)-1], u)
	shellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}
