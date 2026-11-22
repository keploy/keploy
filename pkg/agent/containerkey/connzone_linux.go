//go:build linux

package containerkey

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// sock_diag's inet_diag (linux/inet_diag.h): the sizes of struct
// inet_diag_req_v2 and struct inet_diag_msg, the response attribute
// INET_DIAG_CGROUP_ID (kernel 5.9+), and INET_DIAG_NOCOOKIE.
const (
	inetDiagReqV2Len  = 56
	inetDiagMsgLen    = 72
	inetDiagCgroupID  = 21
	inetDiagNoCookie  = ^uint32(0)
	netlinkHeaderSize = unix.SizeofNlMsghdr
)

// socketCgroup is the ID of the cgroup (v2) of the TCP socket of this
// network namespace whose own address is local and whose peer is remote: the
// cgroup of the process that made it, as the kernel's sock_diag gives it
// (INET_DIAG_CGROUP_ID). An error where the kernel finds no such socket (it
// is gone) or gives no cgroup, and where the query's socket cannot be
// closed: its descriptor was closed already (EBADF, a bug), so the number
// may have been another file's while keploy asked, and the answer is not
// taken.
func socketCgroup(local, remote netip.AddrPort) (cg uint64, err error) {
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	if local.Addr().Is4() != remote.Addr().Is4() {
		return 0, fmt.Errorf("its end %s and the proxy's %s are of two address families", local, remote)
	}
	family := uint8(unix.AF_INET6)
	if local.Addr().Is4() {
		family = unix.AF_INET
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return 0, fmt.Errorf("the kernel's socket queries (sock_diag) cannot be opened: %w", err)
	}
	defer func() {
		if cerr := unix.Close(fd); cerr != nil {
			cg, err = 0, errors.Join(err, fmt.Errorf("the kernel's socket query (sock_diag) socket cannot be closed: %w", cerr))
		}
	}()

	req := make([]byte, netlinkHeaderSize+inetDiagReqV2Len)
	ne := binary.NativeEndian
	ne.PutUint32(req[0:], uint32(len(req)))
	ne.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	ne.PutUint16(req[6:], unix.NLM_F_REQUEST)
	ne.PutUint32(req[8:], 1) // sequence
	b := req[netlinkHeaderSize:]
	b[0], b[1] = family, unix.IPPROTO_TCP
	ne.PutUint32(b[4:], ^uint32(0)) // every TCP state
	id := b[8:]
	binary.BigEndian.PutUint16(id[0:], local.Port())
	binary.BigEndian.PutUint16(id[2:], remote.Port())
	putAddr(id[4:20], local.Addr())
	putAddr(id[20:36], remote.Addr())
	ne.PutUint32(id[40:], inetDiagNoCookie)
	ne.PutUint32(id[44:], inetDiagNoCookie)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, fmt.Errorf("the kernel's socket query (sock_diag) could not be sent: %w", err)
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("the kernel's socket query (sock_diag) got no answer: %w", err)
		}
		return cgroupInAnswer(buf[:n], local, remote)
	}
}

// putAddr writes a in network order into the four 32-bit words of
// inet_diag_sockid's idiag_src or idiag_dst.
func putAddr(dst []byte, a netip.Addr) {
	if a.Is4() {
		v := a.As4()
		copy(dst, v[:])
		return
	}
	v := a.As16()
	copy(dst, v[:])
}

// cgroupInAnswer is the cgroup ID sock_diag's answer msgs gives of the
// socket asked for.
func cgroupInAnswer(msgs []byte, local, remote netip.AddrPort) (uint64, error) {
	ne := binary.NativeEndian
	for len(msgs) >= netlinkHeaderSize {
		l := int(ne.Uint32(msgs[0:]))
		if l < netlinkHeaderSize || l > len(msgs) {
			break
		}
		typ, body := ne.Uint16(msgs[4:]), msgs[netlinkHeaderSize:l]
		msgs = msgs[min(nlmAlign(l), len(msgs)):]
		switch typ {
		case unix.NLMSG_ERROR:
			if len(body) < 4 {
				return 0, errors.New("the kernel's socket query (sock_diag) failed and said not why")
			}
			errno := syscall.Errno(-int32(ne.Uint32(body)))
			if errno == unix.ENOENT {
				return 0, fmt.Errorf("its end %s, connected to %s, is gone from the kernel (the app closed it)", local, remote)
			}
			return 0, fmt.Errorf("the kernel's socket query (sock_diag) failed: %w", errno)
		case unix.SOCK_DIAG_BY_FAMILY:
			if len(body) < inetDiagMsgLen {
				continue
			}
			for attrs := body[inetDiagMsgLen:]; len(attrs) >= unix.SizeofNlAttr; {
				al := int(ne.Uint16(attrs[0:]))
				if al < unix.SizeofNlAttr || al > len(attrs) {
					break
				}
				if ne.Uint16(attrs[2:])&^(unix.NLA_F_NESTED|unix.NLA_F_NET_BYTEORDER) == inetDiagCgroupID && al >= unix.SizeofNlAttr+8 {
					return ne.Uint64(attrs[unix.SizeofNlAttr:]), nil
				}
				attrs = attrs[min(nlmAlign(al), len(attrs)):]
			}
			return 0, errors.New("the kernel gives no cgroup of its socket (a kernel before 5.9, or one built without CONFIG_SOCK_CGROUP_DATA)")
		}
	}
	return 0, errors.New("the kernel's socket query (sock_diag) answered nothing of the socket")
}

// nlmAlign is l rounded up to netlink's 4-byte alignment.
func nlmAlign(l int) int { return (l + 3) &^ 3 }

// cgroup2Mount is a cgroup2 mount of this mount namespace: where it is
// mounted, and the path in the hierarchy of the directory it shows there.
type cgroup2Mount struct{ at, root string }

// cgroup2Mounts is every cgroup2 mount mountinfo (/proc/self/mountinfo)
// lists.
func cgroup2Mounts(mountinfo string) (ms []cgroup2Mount, err error) {
	f, err := os.Open(mountinfo)
	if err != nil {
		return nil, fmt.Errorf("keploy cannot read its mounts: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			ms, err = nil, errors.Join(err, fmt.Errorf("keploy cannot close its mount table %s: %w", mountinfo, cerr))
		}
	}()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		for i, fl := range fields {
			if fl == "-" {
				if i >= 6 && i+1 < len(fields) && fields[i+1] == "cgroup2" {
					ms = append(ms, cgroup2Mount{at: unescapeMount(fields[4]), root: unescapeMount(fields[3])})
				}
				break
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("keploy cannot read its mounts: %w", err)
	}
	return ms, nil
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// findCgroupDir is the path, in its hierarchy, of the directory of cgroup
// id under this agent's cgroup2 mounts (a cgroup's ID is its directory's
// inode number); false where none holds it.
func findCgroupDir(id uint64) (string, bool, error) {
	ms, err := cgroup2Mounts("/proc/self/mountinfo")
	if err != nil {
		return "", false, err
	}
	return findCgroupDirIn(ms, id)
}

// findCgroupDirIn is findCgroupDir under mounts ms. A tree it could not
// wholly read where it found none is an error (not kept): the directory may
// be in the part it could not read. A directory removed as it walked is
// not: a socket's cgroup cannot be removed before it is made.
func findCgroupDirIn(ms []cgroup2Mount, id uint64) (string, bool, error) {
	var walkErr error
	for _, m := range ms {
		var top unix.Stat_t
		if err := unix.Stat(m.at, &top); err != nil {
			walkErr = fmt.Errorf("keploy cannot read the cgroup2 mount at %s: %w", m.at, err)
			continue
		}
		found := ""
		err := filepath.WalkDir(m.at, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) && walkErr == nil {
					walkErr = fmt.Errorf("keploy cannot read the cgroup directory %s: %w", p, err)
				}
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			var st unix.Stat_t
			if err := unix.Lstat(p, &st); err != nil {
				if !errors.Is(err, fs.ErrNotExist) && walkErr == nil {
					walkErr = fmt.Errorf("keploy cannot read the cgroup directory %s: %w", p, err)
				}
				return fs.SkipDir
			}
			if st.Dev != top.Dev {
				return fs.SkipDir // another file system mounted under it
			}
			if st.Ino == id {
				found = p
				return fs.SkipAll
			}
			return nil
		})
		if err != nil && walkErr == nil {
			walkErr = err
		}
		if found != "" {
			rel, _ := filepath.Rel(m.at, found)
			return filepath.Join("/", m.root, rel), true, nil
		}
	}
	if walkErr != nil {
		return "", false, walkErr
	}
	return "", false, nil
}
