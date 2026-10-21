//go:build windows

package tray

import (
	"testing"
	"unsafe"
)

// Explorer only gives the full 127-character tooltip to a cbSize it knows:
// sizeof(NOTIFYICONDATAW) is 976 on 64-bit Windows and 956 on 32-bit.
func TestNotifyIconDataSize(t *testing.T) {
	want := uintptr(956)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		want = 976
	}
	if got := unsafe.Sizeof(notifyIconData{}); got != want {
		t.Fatalf("sizeof(notifyIconData) = %d, want %d", got, want)
	}
}
