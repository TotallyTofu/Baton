//go:build windows

package win

import (
	"encoding/base64"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
)

// The dashboard runs in a browser, which can never hand back a full local
// path, so the daemon shows Windows' own picker (the modern Explorer dialog)
// on the browser's behalf. PowerShell compiles the small COM shim on the fly.
const pickerScript = `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class BatonPicker {
  [ComImport, Guid("DC1C5A9C-E88A-4dde-A5A1-60F82A20AEF7")] class FileOpenDialogCo {}
  [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
  struct Spec { [MarshalAs(UnmanagedType.LPWStr)] public string Name; [MarshalAs(UnmanagedType.LPWStr)] public string Pattern; }
  [ComImport, Guid("43826D1E-E718-42EE-BC55-A1E261C37BFE"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
  interface IShellItem {
    void BindToHandler(IntPtr pbc, ref Guid bhid, ref Guid riid, out IntPtr ppv);
    void GetParent(out IShellItem ppsi);
    void GetDisplayName(uint sigdn, [MarshalAs(UnmanagedType.LPWStr)] out string name);
  }
  [ComImport, Guid("42f85136-db7e-439c-85f1-e4075d135fc8"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
  interface IFileOpenDialog {
    [PreserveSig] int Show(IntPtr parent);
    void SetFileTypes(uint n, [MarshalAs(UnmanagedType.LPArray)] Spec[] specs);
    void SetFileTypeIndex(uint i);
    void GetFileTypeIndex(out uint i);
    void Advise(IntPtr sink, out uint cookie);
    void Unadvise(uint cookie);
    void SetOptions(uint fos);
    void GetOptions(out uint fos);
    void SetDefaultFolder(IShellItem si);
    void SetFolder(IShellItem si);
    void GetFolder(out IShellItem si);
    void GetCurrentSelection(out IShellItem si);
    void SetFileName([MarshalAs(UnmanagedType.LPWStr)] string name);
    void GetFileName([MarshalAs(UnmanagedType.LPWStr)] out string name);
    void SetTitle([MarshalAs(UnmanagedType.LPWStr)] string title);
    void SetOkButtonLabel([MarshalAs(UnmanagedType.LPWStr)] string label);
    void SetFileNameLabel([MarshalAs(UnmanagedType.LPWStr)] string label);
    void GetResult(out IShellItem si);
  }
  public static string Pick(IntPtr owner, bool folder, string title, string ok) {
    var d = (IFileOpenDialog)new FileOpenDialogCo();
    uint o; d.GetOptions(out o);
    o |= 0x40 | 0x800;                       // FORCEFILESYSTEM | PATHMUSTEXIST
    if (folder) { o |= 0x20; }               // PICKFOLDERS
    else {
      o |= 0x1000;                           // FILEMUSTEXIST
      d.SetFileTypes(2, new[] { new Spec { Name = "Programs", Pattern = "*.exe" }, new Spec { Name = "All files", Pattern = "*.*" } });
    }
    d.SetOptions(o);
    d.SetTitle(title);
    if (ok != "") d.SetOkButtonLabel(ok);
    if (d.Show(owner) != 0) return "";
    IShellItem item; d.GetResult(out item);
    string path; item.GetDisplayName(0x80058000, out path); // SIGDN_FILESYSPATH
    return path;
  }
}
"@
if ($env:BATON_PICK_DRYRUN -eq '1') { [Console]::Out.Write('compiled'); exit }
$owner = New-Object System.Windows.Forms.Form
$owner.TopMost = $true; $owner.ShowInTaskbar = $false; $owner.Opacity = 0
$owner.FormBorderStyle = 'None'; $owner.StartPosition = 'CenterScreen'; $owner.Size = New-Object System.Drawing.Size(1,1)
$owner.Show(); $owner.Activate()
try { [Console]::Out.Write([BatonPicker]::Pick($owner.Handle, $env:BATON_PICK_FOLDER -eq '1', $env:BATON_PICK_TITLE, $env:BATON_PICK_OK)) }
finally { $owner.Close() }
`

var pickMu sync.Mutex

// ErrPickerBusy means a picker is already open.
var ErrPickerBusy = errors.New("a picker is already open")

// Pick shows Windows' file or folder picker and returns the chosen path, or
// "" if it was cancelled.
func Pick(folder bool, title, okLabel string) (string, error) {
	if !pickMu.TryLock() {
		return "", ErrPickerBusy
	}
	defer pickMu.Unlock()
	u := utf16.Encode([]rune(pickerScript))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-STA", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	f := "0"
	if folder {
		f = "1"
	}
	cmd.Env = append(cmd.Environ(), "BATON_PICK_FOLDER="+f, "BATON_PICK_TITLE="+title, "BATON_PICK_OK="+okLabel)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // no console window
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
