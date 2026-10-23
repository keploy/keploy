//go:build windows

package tls

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// keepKeystoreAttributes gives copyPath, keploy's edited copy of the keystore
// whose real file is real (orig, as it was read), the keystore's permissions
// before the copy is renamed over it: its DACL (protected from inheritance or
// not, as the keystore's is), which decides who may read it, and its
// read-only attribute; a copy that cannot be given them is not put in place.
// Its owner and group are given too where Windows lets keploy set them:
// setting an owner other than one's own takes a privilege an Administrator
// holds but does not have enabled, and keytool, writing in place, never
// needed it. A file keploy creates otherwise carries what its directory gives
// it, and the creator as its owner.
func keepKeystoreAttributes(copyPath, real string, orig os.FileInfo) error {
	const ownerGroup = windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION
	sd, err := windows.GetNamedSecurityInfo(real, windows.SE_FILE_OBJECT, ownerGroup|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("could not read the permissions of the keystore %s: %w", real, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("could not read the permissions of the keystore %s: %w", real, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("could not read the DACL of the keystore %s: %w", real, err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(copyPath, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("could not give the edited copy of %s the keystore's DACL: %w", real, err)
	}

	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("could not read the owner of the keystore %s: %w", real, err)
	}
	group, _, err := sd.Group()
	if err != nil {
		return fmt.Errorf("could not read the group of the keystore %s: %w", real, err)
	}
	csd, err := windows.GetNamedSecurityInfo(copyPath, windows.SE_FILE_OBJECT, ownerGroup)
	if err != nil {
		return fmt.Errorf("could not read the owner of the edited copy of %s: %w", real, err)
	}
	cowner, _, _ := csd.Owner()
	cgroup, _, _ := csd.Group()
	// Set only where they differ, and only where Windows lets keploy (see
	// above).
	if owner != nil && (cowner == nil || !owner.Equals(cowner)) {
		_ = windows.SetNamedSecurityInfo(copyPath, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, owner, nil, nil, nil)
	}
	if group != nil && (cgroup == nil || !group.Equals(cgroup)) {
		_ = windows.SetNamedSecurityInfo(copyPath, windows.SE_FILE_OBJECT, windows.GROUP_SECURITY_INFORMATION, nil, group, nil, nil)
	}
	// The read-only attribute, which is what a Windows file's mode is.
	if err := os.Chmod(copyPath, orig.Mode().Perm()); err != nil {
		return fmt.Errorf("could not give the edited copy of %s the keystore's attributes: %w", real, err)
	}
	return nil
}

// syncDir has nothing to do on Windows: a rename is written with the file
// system's own metadata.
func syncDir(string) error { return nil }

// hardLinks is 1 on Windows, where FileInfo does not carry a link count; a
// JDK's cacerts is not hard-linked there.
func hardLinks(os.FileInfo) uint64 { return 1 }
