//go:build linux

package tls

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// keystoreXattrsNotCopied are the extended attributes a copy does not take
// from the original: IMA's and EVM's, which the kernel derives from the
// file's own content and metadata, so the original's would not match the
// copy.
var keystoreXattrsNotCopied = map[string]bool{"security.ima": true, "security.evm": true}

// copyExtendedAttributes gives dst every extended attribute src has (but
// keystoreXattrsNotCopied): on a host with SELinux the keystore's label
// (security.selinux, cert_t on Fedora and RHEL), which a file keploy creates
// would otherwise take from its directory, so that a confined service could
// no longer read it; a POSIX ACL (system.posix_acl_access); and any other. And
// it takes from dst an attribute src does not have outside the security
// namespace (a default ACL of the directory, which dst took on its creation):
// the kernel's own security.* attributes, a label src lacks among them, are
// the kernel's to keep. dst is a file keploy created in src's directory.
func copyExtendedAttributes(src, dst string) error {
	want, err := xattrNames(src)
	if errors.Is(err, unix.ENOTSUP) {
		return nil // a filesystem without extended attributes: dst has none either
	}
	if err != nil {
		return fmt.Errorf("could not list the extended attributes of %s: %w", src, err)
	}
	keep := map[string]bool{}
	for _, name := range want {
		if keystoreXattrsNotCopied[name] {
			continue
		}
		keep[name] = true
		v, err := xattrValue(src, name)
		if err != nil {
			return fmt.Errorf("could not read %s of %s: %w", name, src, err)
		}
		if err := unix.Lsetxattr(dst, name, v, 0); err != nil {
			return fmt.Errorf("could not set %s on %s: %w", name, dst, err)
		}
	}
	have, err := xattrNames(dst)
	if err != nil {
		return fmt.Errorf("could not list the extended attributes of %s: %w", dst, err)
	}
	for _, name := range have {
		if keep[name] || strings.HasPrefix(name, "security.") {
			continue
		}
		// One the original does not have: a default ACL of the directory,
		// say, which the copy took on its creation.
		if err := unix.Lremovexattr(dst, name); err != nil {
			return fmt.Errorf("could not remove %s from %s: %w", name, dst, err)
		}
	}
	return nil
}

// xattrNames lists path's extended attributes (not following a link).
func xattrNames(path string) ([]string, error) {
	for {
		n, err := unix.Llistxattr(path, nil)
		if err != nil || n == 0 {
			return nil, err
		}
		buf := make([]byte, n)
		n, err = unix.Llistxattr(path, buf)
		if errors.Is(err, unix.ERANGE) {
			continue // grew since it was sized
		}
		if err != nil {
			return nil, err
		}
		var names []string
		for _, name := range bytes.Split(buf[:n], []byte{0}) {
			if len(name) > 0 {
				names = append(names, string(name))
			}
		}
		return names, nil
	}
}

// xattrValue reads one of path's extended attributes (not following a link).
func xattrValue(path, name string) ([]byte, error) {
	for {
		n, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		n, err = unix.Lgetxattr(path, name, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}
