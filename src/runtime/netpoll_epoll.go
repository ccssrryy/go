// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux
// +build linux

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

func epollcreate(size int32) int32
func epollcreate1(flags int32) int32

//go:noescape
func epollctl(epfd, op, fd int32, ev *epollevent) int32

//go:noescape
func epollwait(epfd int32, ev *epollevent, nev, timeout int32) int32
func closeonexec(fd int32)

var (
	epfd int32 = -1 // epoll descriptor

	netpollBreakRd, netpollBreakWr uintptr // for netpollBreak

	netpollWakeSig uint32 // used to avoid duplicate calls of netpollBreak
)

func netpollinit() {
	epfd = epollcreate1(_EPOLL_CLOEXEC)
	if epfd < 0 {
		epfd = epollcreate(1024)
		if epfd < 0 {
			println("runtime: epollcreate failed with", -epfd)
			throw("runtime: netpollinit failed")
		}
		closeonexec(epfd)
	}
	r, w, errno := nonblockingPipe()
	if errno != 0 {
		println("runtime: pipe failed with", -errno)
		throw("runtime: pipe failed")
	}
	ev := epollevent{
		events: _EPOLLIN,
	}
	*(**uintptr)(unsafe.Pointer(&ev.data)) = &netpollBreakRd
	errno = epollctl(epfd, _EPOLL_CTL_ADD, r, &ev)
	if errno != 0 {
		println("runtime: epollctl failed with", -errno)
		throw("runtime: epollctl failed")
	}
	netpollBreakRd = uintptr(r)
	netpollBreakWr = uintptr(w)
}

func netpollIsPollDescriptor(fd uintptr) bool {
	return fd == uintptr(epfd) || fd == netpollBreakRd || fd == netpollBreakWr
}

func netpollopen(fd uintptr, pd *pollDesc) int32 {
	var ev epollevent
	ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET
	*(**pollDesc)(unsafe.Pointer(&ev.data)) = pd
	return -epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)
}

func netpollclose(fd uintptr) int32 {
	var ev epollevent
	return -epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// netpollBreak interrupts an epollwait.
func netpollBreak() {
	if atomic.Cas(&netpollWakeSig, 0, 1) {
		for {
			var b byte
			n := write(netpollBreakWr, unsafe.Pointer(&b), 1)
			if n == 1 {
				break
			}
			if n == -_EINTR {
				continue
			}
			if n == -_EAGAIN {
				return
			}
			println("runtime: netpollBreak write failed with", -n)
			throw("runtime: netpollBreak write failed")
		}
	}
}

// netpoll checks for ready network connections.
// Returns list of goroutines that become runnable.
// delay < 0: blocks indefinitely
// delay == 0: does not block, just polls
// delay > 0: block for up to that many nanoseconds
// netpoller 最终调用的核心方法

/* 
总共有五处调用
其中四处非阻塞调用，参数为 0，轮询后将 runnable 的 goroutine 放入队列
startTheWorldWithSema, findrunnable, pollWork，sysmon
一处 findrunnable 中的阻塞调用， [list := netpoll(delay)](https://github.com/ccssrryy/go/blob/cd6e0d7cad147b686f9f8066b651b0079e6f51c6/src/runtime/proc.go#L2947)
证明了 netpoller 的五个检查点：
GC 恢复时
GC 扫描时(gcDrain -> pollWork -> netpoll)
系统监视器
调度器在本地处理器全局队列中查找可用 goroutine 之后，窃取其它处理器队列之前
未找到可用 goroutine 时 
所以，netpoller 不是一个单独的线程处理，而是寄生在 *调度器，GC、系统监视器* 中
*/
func netpoll(delay int64) gList {
	if epfd == -1 {
		return gList{}
	}
	var waitms int32
	if delay < 0 {
		waitms = -1
	} else if delay == 0 {
		waitms = 0
	} else if delay < 1e6 {
		waitms = 1
	} else if delay < 1e15 {
		waitms = int32(delay / 1e6)
	} else {
		// An arbitrary cap on how long to wait for a timer.
		// 1e9 ms == ~11.5 days.
		waitms = 1e9
	}
	var events [128]epollevent
retry:
	n := epollwait(epfd, &events[0], int32(len(events)), waitms)
	if n < 0 {
		if n != -_EINTR {
			println("runtime: epollwait on fd", epfd, "failed with", -n)
			throw("runtime: netpoll failed")
		}
		// If a timed sleep was interrupted, just return to
		// recalculate how long we should sleep now.
		if waitms > 0 {
			return gList{}
		}
		goto retry
	}
	var toRun gList
	for i := int32(0); i < n; i++ {
		ev := &events[i]
		if ev.events == 0 {
			continue
		}

		if *(**uintptr)(unsafe.Pointer(&ev.data)) == &netpollBreakRd {
			if ev.events != _EPOLLIN {
				println("runtime: netpoll: break fd ready for", ev.events)
				throw("runtime: netpoll: break fd ready for something unexpected")
			}
			if delay != 0 {
				// netpollBreak could be picked up by a
				// nonblocking poll. Only read the byte
				// if blocking.
				var tmp [16]byte
				read(int32(netpollBreakRd), noescape(unsafe.Pointer(&tmp[0])), int32(len(tmp)))
				atomic.Store(&netpollWakeSig, 0)
			}
			continue
		}

		var mode int32
		if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'r'
		}
		if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'w'
		}
		if mode != 0 {
			pd := *(**pollDesc)(unsafe.Pointer(&ev.data))
			pd.everr = false
			if ev.events == _EPOLLERR {
				pd.everr = true
			}
			netpollready(&toRun, pd, mode)
		}
	}
	return toRun
}
