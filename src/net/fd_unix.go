// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin dragonfly freebsd linux nacl netbsd openbsd solaris

package net

import (
	"context"
	"internal/poll"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
)

// Network file descriptor.
type netFD struct {
	pfd poll.FD

	// immutable until Close
	family      int
	sotype      int
	isConnected bool
	net         string
	laddr       Addr
	raddr       Addr
}

func newFD(sysfd, family, sotype int, net string) (*netFD, error) {
	ret := &netFD{
		pfd: poll.FD{
			Sysfd:         sysfd,
			IsStream:      sotype == syscall.SOCK_STREAM,
			ZeroReadIsEOF: sotype != syscall.SOCK_DGRAM && sotype != syscall.SOCK_RAW,
		},
		family: family,
		sotype: sotype,
		net:    net,
	}
	return ret, nil
}

func (fd *netFD) init() error {
	return fd.pfd.Init(fd.net, true)
}

func (fd *netFD) setAddr(laddr, raddr Addr) {
	fd.laddr = laddr
	fd.raddr = raddr
	runtime.SetFinalizer(fd, (*netFD).Close)
}

func (fd *netFD) name() string {
	var ls, rs string
	if fd.laddr != nil {
		ls = fd.laddr.String()
	}
	if fd.raddr != nil {
		rs = fd.raddr.String()
	}
	return fd.net + ":" + ls + "->" + rs
}

func (fd *netFD) connect(ctx context.Context, la, ra syscall.Sockaddr) (rsa syscall.Sockaddr, ret error) {
	// Do not need to call fd.writeLock here,
	// because fd is not yet accessible to user,
	// so no concurrent operations are possible.
	switch err := connectFunc(fd.pfd.Sysfd, ra); err {
	case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
	case nil, syscall.EISCONN:
		select {
		case <-ctx.Done():
			return nil, mapErr(ctx.Err())
		default:
		}
		if err := fd.pfd.Init(fd.net, true); err != nil {
			return nil, err
		}
		runtime.KeepAlive(fd)
		return nil, nil
	case syscall.EINVAL:
		// On Solaris we can see EINVAL if the socket has
		// already been accepted and closed by the server.
		// Treat this as a successful connection--writes to
		// the socket will see EOF.  For details and a test
		// case in C see https://golang.org/issue/6828.
		if runtime.GOOS == "solaris" {
			return nil, nil
		}
		fallthrough
	default:
		return nil, os.NewSyscallError("connect", err)
	}
	if err := fd.pfd.Init(fd.net, true); err != nil {
		return nil, err
	}
	if deadline, _ := ctx.Deadline(); !deadline.IsZero() {
		fd.pfd.SetWriteDeadline(deadline)
		defer fd.pfd.SetWriteDeadline(noDeadline)
	}

	// Start the "interrupter" goroutine, if this context might be canceled.
	// (The background context cannot)
	//
	// The interrupter goroutine waits for the context to be done and
	// interrupts the dial (by altering the fd's write deadline, which
	// wakes up waitWrite).
	if ctx != context.Background() {
		// Wait for the interrupter goroutine to exit before returning
		// from connect.
		done := make(chan struct{})
		interruptRes := make(chan error)
		defer func() {
			close(done)
			if ctxErr := <-interruptRes; ctxErr != nil && ret == nil {
				// The interrupter goroutine called SetWriteDeadline,
				// but the connect code below had returned from
				// waitWrite already and did a successful connect (ret
				// == nil). Because we've now poisoned the connection
				// by making it unwritable, don't return a successful
				// dial. This was issue 16523.
				ret = ctxErr
				fd.Close() // prevent a leak
			}
		}()
		go func() {
			select {
			case <-ctx.Done():
				// Force the runtime's poller to immediately give up
				// waiting for writability, unblocking waitWrite
				// below.
				fd.pfd.SetWriteDeadline(aLongTimeAgo)
				testHookCanceledDial()
				interruptRes <- ctx.Err()
			case <-done:
				interruptRes <- nil
			}
		}()
	}

	for {
		// Performing multiple connect system calls on a
		// non-blocking socket under Unix variants does not
		// necessarily result in earlier errors being
		// returned. Instead, once runtime-integrated network
		// poller tells us that the socket is ready, get the
		// SO_ERROR socket option to see if the connection
		// succeeded or failed. See issue 7474 for further
		// details.
		if err := fd.pfd.WaitWrite(); err != nil {
			select {
			case <-ctx.Done():
				return nil, mapErr(ctx.Err())
			default:
			}
			return nil, err
		}
		nerr, err := getsockoptIntFunc(fd.pfd.Sysfd, syscall.SOL_SOCKET, syscall.SO_ERROR)
		if err != nil {
			return nil, os.NewSyscallError("getsockopt", err)
		}
		switch err := syscall.Errno(nerr); err {
		case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
		case syscall.EISCONN:
			return nil, nil
		case syscall.Errno(0):
			// The runtime poller can wake us up spuriously;
			// see issues 14548 and 19289. Check that we are
			// really connected; if not, wait again.
			if rsa, err := syscall.Getpeername(fd.pfd.Sysfd); err == nil {
				return rsa, nil
			}
		default:
			return nil, os.NewSyscallError("getsockopt", err)
		}
		runtime.KeepAlive(fd)
	}
}

func (fd *netFD) Close() error {
	runtime.SetFinalizer(fd, nil)
	return fd.pfd.Close()
}

func (fd *netFD) shutdown(how int) error {
	err := fd.pfd.Shutdown(how)
	runtime.KeepAlive(fd)
	return wrapSyscallError("shutdown", err)
}

func (fd *netFD) closeRead() error {
	return fd.shutdown(syscall.SHUT_RD)
}

func (fd *netFD) closeWrite() error {
	return fd.shutdown(syscall.SHUT_WR)
}

func (fd *netFD) Read(p []byte) (n int, err error) {
	n, err = fd.pfd.Read(p)
	runtime.KeepAlive(fd)
	return n, wrapSyscallError("read", err)
}

func (fd *netFD) readFrom(p []byte) (n int, sa syscall.Sockaddr, err error) {
	n, sa, err = fd.pfd.ReadFrom(p)
	runtime.KeepAlive(fd)
	return n, sa, wrapSyscallError("recvfrom", err)
}

func (fd *netFD) readMsg(p []byte, oob []byte) (n, oobn, flags int, sa syscall.Sockaddr, err error) {
	n, oobn, flags, sa, err = fd.pfd.ReadMsg(p, oob)
	runtime.KeepAlive(fd)
	return n, oobn, flags, sa, wrapSyscallError("recvmsg", err)
}

func (fd *netFD) Write(p []byte) (nn int, err error) {
	nn, err = fd.pfd.Write(p)
	runtime.KeepAlive(fd)
	return nn, wrapSyscallError("write", err)
}

func (fd *netFD) writeTo(p []byte, sa syscall.Sockaddr) (n int, err error) {
	n, err = fd.pfd.WriteTo(p, sa)
	runtime.KeepAlive(fd)
	return n, wrapSyscallError("sendto", err)
}

func (fd *netFD) writeMsg(p []byte, oob []byte, sa syscall.Sockaddr) (n int, oobn int, err error) {
	n, oobn, err = fd.pfd.WriteMsg(p, oob, sa)
	runtime.KeepAlive(fd)
	return n, oobn, wrapSyscallError("sendmsg", err)
}

func (fd *netFD) accept() (netfd *netFD, err error) {
	d, rsa, errcall, err := fd.pfd.Accept()
	if err != nil {
		if errcall != "" {
			err = wrapSyscallError(errcall, err)
		}
		return nil, err
	}

	if netfd, err = newFD(d, fd.family, fd.sotype, fd.net); err != nil {
		poll.CloseFunc(d)
		return nil, err
	}
	if err = netfd.init(); err != nil {
		fd.Close()
		return nil, err
	}
	lsa, _ := syscall.Getsockname(netfd.pfd.Sysfd)
	netfd.setAddr(netfd.addrFunc()(lsa), netfd.addrFunc()(rsa))
	return netfd, nil
}

// tryDupCloexec indicates whether F_DUPFD_CLOEXEC should be used.
// If the kernel doesn't support it, this is set to 0.
var tryDupCloexec = int32(1)

func dupCloseOnExec(fd int) (newfd int, err error) {
	if atomic.LoadInt32(&tryDupCloexec) == 1 {
		r0, _, e1 := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD_CLOEXEC, 0)
		if runtime.GOOS == "darwin" && e1 == syscall.EBADF {
			// On OS X 10.6 and below (but we only support
			// >= 10.6), F_DUPFD_CLOEXEC is unsupported
			// and fcntl there falls back (undocumented)
			// to doing an ioctl instead, returning EBADF
			// in this case because fd is not of the
			// expected device fd type. Treat it as
			// EINVAL instead, so we fall back to the
			// normal dup path.
			// TODO: only do this on 10.6 if we can detect 10.6
			// cheaply.
			e1 = syscall.EINVAL
		}
		switch e1 {
		case 0:
			return int(r0), nil
		case syscall.EINVAL:
			// Old kernel. Fall back to the portable way
			// from now on.
			atomic.StoreInt32(&tryDupCloexec, 0)
		default:
			return -1, os.NewSyscallError("fcntl", e1)
		}
	}
	return dupCloseOnExecOld(fd)
}

// dupCloseOnExecUnixOld is the traditional way to dup an fd and
// set its O_CLOEXEC bit, using two system calls.
func dupCloseOnExecOld(fd int) (newfd int, err error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	newfd, err = syscall.Dup(fd)
	if err != nil {
		return -1, os.NewSyscallError("dup", err)
	}
	syscall.CloseOnExec(newfd)
	return
}

func (fd *netFD) dup() (f *os.File, err error) {
	ns, err := dupCloseOnExec(fd.pfd.Sysfd)
	if err != nil {
		return nil, err
	}

	// We want blocking mode for the new fd, hence the double negative.
	// This also puts the old fd into blocking mode, meaning that
	// I/O will block the thread instead of letting us use the epoll server.
	// Everything will still work, just with more threads.
	if err = fd.pfd.SetBlocking(); err != nil {
		return nil, os.NewSyscallError("setnonblock", err)
	}

	return os.NewFile(uintptr(ns), fd.name()), nil
}
