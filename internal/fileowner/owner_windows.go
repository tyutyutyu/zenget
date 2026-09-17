//go:build windows

// Package fileowner provides the small platform-specific ownership check used
// by local policy and trust files.
package fileowner

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CurrentUserOwns fails closed on Windows. Windows ACLs do not expose a
// portable POSIX uid through os.FileInfo, so a caller must add an explicit ACL
// check before allowing a sensitive local file.
func CurrentUserOwns(os.FileInfo) bool {
	return false
}

const (
	fileDeleteChild   = windows.ACCESS_MASK(0x00000040)
	unsafeWriteRights = windows.ACCESS_MASK(windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA|windows.FILE_WRITE_ATTRIBUTES|windows.FILE_WRITE_EA) | fileDeleteChild |
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
)

// CurrentUserOwnsPath checks the Windows owner SID and rejects write access
// granted to any principal other than the owner, LocalSystem, or built-in
// Administrators. A missing/NULL DACL, reparse point, or metadata error fails
// closed.
func CurrentUserOwnsPath(path string, info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false
	}

	token := windows.GetCurrentProcessToken()
	tokenUser, err := token.GetTokenUser()
	if err != nil || tokenUser == nil || tokenUser.User.Sid == nil {
		return false
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(tokenUser.User.Sid) {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return false
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil || ace == nil {
			return false
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		// The x/sys/windows API exposes the common ACE layout. Reject every
		// other ACE type rather than guessing at object/callback ACE layouts;
		// an unrecognised allow ACE could grant a write right to an untrusted
		// SID.
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		if windows.ACCESS_MASK(ace.Mask)&unsafeWriteRights == 0 {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !trustedWritePrincipal(aceSID, owner) {
			return false
		}
	}
	return true
}

func trustedWritePrincipal(sid, owner *windows.SID) bool {
	if sid == nil || owner == nil || !sid.IsValid() || !owner.IsValid() {
		return false
	}
	if sid.Equals(owner) {
		return true
	}
	identifier := sid.String()
	return identifier == "S-1-5-18" || identifier == "S-1-5-32-544"
}

// PrivateFileModeSafe and PrivateDirectoryModeSafe intentionally do not use
// os.FileMode on Windows. ACL evaluation in CurrentUserOwnsPath is the
// authoritative access check on this platform.
func PrivateFileModeSafe(os.FileInfo) bool      { return true }
func PrivateDirectoryModeSafe(os.FileInfo) bool { return true }

// SecurePath replaces inherited permissions with an owner/SYSTEM/
// Administrators ACL. It is used immediately after creating sensitive files
// and directories so the inherited ACL cannot become the trust boundary.
func SecurePath(path string, directory bool) error {
	token := windows.GetCurrentProcessToken()
	tokenUser, err := token.GetTokenUser()
	if err != nil || tokenUser == nil || tokenUser.User.Sid == nil {
		return err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := []windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: inheritance,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(tokenUser.User.Sid)}},
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: inheritance,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(systemSID)}},
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: inheritance,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_ALIAS, TrusteeValue: windows.TrusteeValueFromSID(adminSID)}},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		tokenUser.User.Sid, nil, acl, nil)
}
