//go:build linux

package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"go.keploy.io/server/v3/pkg/agent/appstart"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	cnIdxProc         = 1
	cnValProc         = 1
	procCnMcastListen = 1
	procEventExec     = 0x2
)

func (p *Proxy) watchStarts(ctx context.Context) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM, unix.NETLINK_CONNECTOR)
	if err != nil {
		p.logger.Debug("app starts are not watched: the process event feed is not available", zap.Error(err))
		return
	}
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, 8<<20)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: cnIdxProc, Pid: uint32(os.Getpid())}); err != nil {
		unix.Close(fd)
		p.logger.Debug("app starts are not watched: the process event feed could not be joined", zap.Error(err))
		return
	}
	msg := make([]byte, 40)
	binary.NativeEndian.PutUint32(msg[0:], 40)
	binary.NativeEndian.PutUint16(msg[4:], unix.NLMSG_DONE)
	binary.NativeEndian.PutUint32(msg[12:], uint32(os.Getpid()))
	binary.NativeEndian.PutUint32(msg[16:], cnIdxProc)
	binary.NativeEndian.PutUint32(msg[20:], cnValProc)
	binary.NativeEndian.PutUint16(msg[32:], 4)
	binary.NativeEndian.PutUint32(msg[36:], procCnMcastListen)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: cnIdxProc}); err != nil {
		unix.Close(fd)
		p.logger.Debug("app starts are not watched: the process event feed refused the subscription", zap.Error(err))
		return
	}
	go func() {
		<-ctx.Done()
		unix.Close(fd)
	}()

	execs := make(chan int, 1024)
	go func() {
		defer close(execs)
		buf := make([]byte, 64*1024)
		for {
			n, _, err := unix.Recvfrom(fd, buf, 0)
			if err == unix.EINTR || err == unix.ENOBUFS {
				if err == unix.ENOBUFS {
					p.logger.Debug("the process event feed overflowed; some app starts may be missed")
				}
				continue
			}
			if err != nil {
				if ctx.Err() == nil {
					p.logger.Debug("the process event feed stopped", zap.Error(err))
				}
				return
			}
			msgs, err := syscall.ParseNetlinkMessage(buf[:n])
			if err != nil {
				continue
			}
			for _, m := range msgs {
				if len(m.Data) < 20+24 || binary.NativeEndian.Uint32(m.Data[20:]) != procEventExec {
					continue
				}
				execs <- int(binary.NativeEndian.Uint32(m.Data[20+20:]))
			}
		}
	}()

	self := os.Getpid()
	pending := map[int]time.Time{}
	t := time.NewTicker(250 * time.Microsecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case pid, ok := <-execs:
			if !ok {
				return
			}
			if pid != self && descends(pid, int(p.appPID)) {
				pending[pid] = time.Now()
			}
		case <-t.C:
			if len(pending) == 0 {
				continue
			}
			table := listenTable()
			for pid, at := range pending {
				port, listening, alive := listensOn(pid, table)
				switch {
				case !alive || time.Since(at) > 2*time.Minute:
					delete(pending, pid)
				case listening:
					delete(pending, pid)
					if !appstart.IsWorker(pid) {
						appstart.NoteAt(at, uint32(pid), uint16(port))
						p.logger.Debug("an app started", zap.Int("pid", pid), zap.Uint32("port", port))
					}
				}
			}
		}
	}
}

func listensOn(pid int, table map[string]uint32) (uint32, bool, bool) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if err != nil {
		return 0, false, false
	}
	for _, fd := range fds {
		link, err := os.Readlink(dir + "/" + fd.Name())
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if port, ok := table[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")]; ok {
			return port, true, true
		}
	}
	return 0, false, true
}
