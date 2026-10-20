//go:build windows

package win

import (
	"os"
	"testing"
)

// TestPickerCompiles builds the picker's COM shim without showing a dialog.
func TestPickerCompiles(t *testing.T) {
	os.Setenv("BATON_PICK_DRYRUN", "1")
	defer os.Unsetenv("BATON_PICK_DRYRUN")
	out, err := Pick(true, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if out != "compiled" {
		t.Fatalf("got %q", out)
	}
}
