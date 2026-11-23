//go:build windows

package tls

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
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
	// name reaches CertOpenStore as a uintptr, which keeps nothing alive.
	runtime.KeepAlive(name)
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

// openRootStore opens s, read-only or for a removal. The user's is its
// .Default physical store, where certutil -user -addstore puts a root: the
// user's view of ROOT takes in the machine's roots as well, and a removal
// through that view could reach the machine's store. The machine's is its
// system store.
func openRootStore(s winRootStore, readOnly bool) (windows.Handle, error) {
	provider, location, name := uintptr(windows.CERT_STORE_PROV_PHYSICAL), uint32(windows.CERT_SYSTEM_STORE_CURRENT_USER), `Root\.Default`
	if s == machineRootStore {
		provider, location, name = windows.CERT_STORE_PROV_SYSTEM, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, "ROOT"
	}
	flags := location | windows.CERT_STORE_OPEN_EXISTING_FLAG
	if readOnly {
		flags |= windows.CERT_STORE_READONLY_FLAG
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	store, err := windows.CertOpenStore(provider, 0, 0, flags, uintptr(unsafe.Pointer(p)))
	// p reaches CertOpenStore as a uintptr, which keeps nothing alive.
	runtime.KeepAlive(p)
	return store, err
}

// rootStoreCerts returns the encoding of every certificate in s. Reading
// needs no administrator in either store.
func rootStoreCerts(s winRootStore) ([][]byte, error) {
	store, err := openRootStore(s, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CertCloseStore(store, 0) }()
	var ders [][]byte
	var c *windows.CertContext
	for {
		// Each call frees the context it is passed.
		c, err = windows.CertEnumCertificatesInStore(store, c)
		if c == nil {
			if errors.Is(err, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return ders, nil
			}
			return nil, err
		}
		ders = append(ders, bytes.Clone(unsafe.Slice(c.EncodedCert, c.Length)))
	}
}

// rootStoreDelete removes from s the certificate whose encoding is der, and
// says whether s held it. The user's store asks the user, in a dialog,
// whether to remove a root, and waits for the answer: a No is
// errRemovalDeclined, a session with no desktop to ask in
// errRemovalNoPrompt. The machine's needs an administrator.
func rootStoreDelete(s winRootStore, der []byte) (bool, error) {
	store, err := openRootStore(s, false)
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CertCloseStore(store, 0) }()
	var c *windows.CertContext
	for {
		c, err = windows.CertEnumCertificatesInStore(store, c)
		if c == nil {
			if errors.Is(err, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return false, nil
			}
			return false, err
		}
		if !bytes.Equal(unsafe.Slice(c.EncodedCert, c.Length), der) {
			continue
		}
		// The removal frees the context it is passed, whatever it answers,
		// and the enumeration's own is freed here.
		dup := windows.CertDuplicateCertificateContext(c)
		_ = windows.CertFreeCertificateContext(c)
		return true, removalError(windows.CertDeleteCertificateFromStore(dup))
	}
}

// removalError names a removal Windows' prompt did not allow: the user's No
// (ERROR_CANCELLED), or a session with no desktop to show the prompt in
// (ERROR_NOT_SUPPORTED), as a Win32 code or as its HRESULT.
func removalError(err error) error {
	var errno windows.Errno
	if err == nil || !errors.As(err, &errno) {
		return err
	}
	switch {
	case errno == windows.ERROR_CANCELLED || uint32(errno) == hrCancelled:
		return fmt.Errorf("%w: %v", errRemovalDeclined, err)
	case errno == windows.ERROR_NOT_SUPPORTED || uint32(errno) == hrNotSupported:
		return fmt.Errorf("%w: %v", errRemovalNoPrompt, err)
	}
	return err
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
