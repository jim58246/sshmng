//go:build windows

package update

import "golang.org/x/sys/windows"

// hideOldBinary marks a leftover old-binary file as hidden so it doesn't
// clutter the install directory. On Windows, goupdate.Apply hides the .old it
// creates; we hide our rotated leftovers (.old.<ts>) for parity. A locked
// leftover (still-running old process) is hidden in place and cleaned up by a
// future sweep once unlocked.
func hideOldBinary(path string) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	_ = windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_HIDDEN)
}
