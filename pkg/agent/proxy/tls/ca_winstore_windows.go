//go:build windows

package tls

import (
	"bytes"
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// userRootHolds says whether the current user's ROOT store holds the
// certificate der. That store's view takes in the machine's ROOT store too, so
// a CA an elevated run put there counts as trusted as well.
func userRootHolds(der []byte) (bool, error) {
	return rootStoreHolds(windows.CERT_SYSTEM_STORE_CURRENT_USER, der)
}

// machineRootHolds says whether the machine's own ROOT store holds der.
func machineRootHolds(der []byte) (bool, error) {
	return rootStoreHolds(windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, der)
}

// rootStoreHolds opens the ROOT system store of location read-only and looks
// for a certificate whose encoding is der. Reading needs no administrator in
// either location.
func rootStoreHolds(location uint32, der []byte) (bool, error) {
	name, err := windows.UTF16PtrFromString("ROOT")
	if err != nil {
		return false, err
	}
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		location|windows.CERT_STORE_READONLY_FLAG|windows.CERT_STORE_OPEN_EXISTING_FLAG, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CertCloseStore(store, 0) }()
	var c *windows.CertContext
	for {
		// Each call frees the context it is passed.
		c, err = windows.CertEnumCertificatesInStore(store, c)
		if c == nil {
			if errors.Is(err, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return false, nil
			}
			return false, err
		}
		if bytes.Equal(unsafe.Slice(c.EncodedCert, c.Length), der) {
			_ = windows.CertFreeCertificateContext(c)
			return true, nil
		}
	}
}

// tokenElevated says whether this process's token can use the Administrators
// group (an administrator past UAC, or LocalSystem): only such a run may
// change the machine's trust. It reads the group, as CheckTokenMembership
// does, not TokenElevation: a token restricted from an elevated one, with
// Administrators deny-only, still reports TokenElevation (seen on Windows 11
// 26200), yet cannot write the machine's store. A check that cannot be made
// answers false, which never asks the machine's store.
func tokenElevated() bool {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	member, err := windows.Token(0).IsMember(admins)
	return err == nil && member
}
