//go:build windows

package fileowner

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCurrentUserOwnsPathUsesOwnerAndACL(t *testing.T) {
	path := t.TempDir() + `\private.json`
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SecurePath(path, false); err != nil {
		t.Fatalf("SecurePath() error = %v", err)
	}
	if !CurrentUserOwnsPath(path, info) {
		t.Fatal("CurrentUserOwnsPath() = false for a secured current-user file")
	}

	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_WRITE,
		AccessMode:        windows.SET_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(worldSID),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if CurrentUserOwnsPath(path, info) {
		t.Fatal("CurrentUserOwnsPath() accepted a file writable by Everyone")
	}
}

func TestCurrentUserOwnsPathAllowsDeniedAndReadOnlyUntrustedACEs(t *testing.T) {
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		mode windows.ACCESS_MODE
		mask windows.ACCESS_MASK
	}{
		{name: "read only", mode: windows.GRANT_ACCESS, mask: windows.GENERIC_READ},
		{name: "denied write", mode: windows.DENY_ACCESS, mask: windows.GENERIC_WRITE},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertUntrustedACEIsSafe(t, worldSID, test.mode, test.mask)
		})
	}
}

func assertUntrustedACEIsSafe(t *testing.T, sid *windows.SID, mode windows.ACCESS_MODE, mask windows.ACCESS_MASK) {
	t.Helper()
	path := t.TempDir() + `\private.json`
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SecurePath(path, false); err != nil {
		t.Fatalf("SecurePath() error = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: mask,
		AccessMode:        mode,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if !CurrentUserOwnsPath(path, info) {
		t.Fatal("CurrentUserOwnsPath() rejected a file with a safe untrusted ACE")
	}
}

func TestCurrentUserOwnsPathRejectsReparsePoint(t *testing.T) {
	target := t.TempDir() + `\target`
	link := t.TempDir() + `\link`
	if err := os.WriteFile(target, []byte("target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("creating a Windows symlink is unavailable: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if CurrentUserOwnsPath(link, info) {
		t.Fatal("CurrentUserOwnsPath() accepted a reparse point")
	}
}

func TestCurrentUserOwnsPathFailsClosedOnMissingMetadata(t *testing.T) {
	if CurrentUserOwnsPath(t.TempDir()+`\missing`, nil) {
		t.Fatal("CurrentUserOwnsPath() accepted missing metadata")
	}
}
