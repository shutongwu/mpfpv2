//go:build linux && !android

package client

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// createBoundUDPConn creates a UDP socket bound to a specific NIC via
// SO_BINDTODEVICE. Go's runtime netpoller manages the fd as non-blocking
// internally — short buffer-full conditions are retried automatically,
// while real errors (ENETUNREACH) are returned immediately.
func createBoundUDPConn(localAddr net.IP, ifaceName string) (*net.UDPConn, error) {
	isIPv6 := localAddr.To4() == nil
	af := syscall.AF_INET
	if isIPv6 {
		af = syscall.AF_INET6
	}

	s, err := syscall.Socket(af, syscall.SOCK_DGRAM|syscall.SOCK_NONBLOCK, syscall.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("socket (iface=%s): %w", ifaceName, err)
	}

	if err := syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("SO_REUSEADDR (iface=%s): %w", ifaceName, err)
	}

	if ifaceName != "" {
		if err := syscall.SetsockoptString(s, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifaceName); err != nil {
			syscall.Close(s)
			return nil, fmt.Errorf("SO_BINDTODEVICE (iface=%s): %w", ifaceName, err)
		}
	}

	// FPV low-latency: smaller than default (212KB) to limit stale-packet accumulation,
	// but large enough to absorb WiFi/4G jitter (50-100ms bursts).
	// 128KB ≈ 90 packets ≈ 250ms at 4Mbps.
	syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 131072)
	syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 131072)

	var sa syscall.Sockaddr
	if isIPv6 {
		lsa := syscall.SockaddrInet6{Port: 0}
		copy(lsa.Addr[:], localAddr.To16())
		sa = &lsa
	} else {
		lsa := syscall.SockaddrInet4{Port: 0}
		copy(lsa.Addr[:], localAddr.To4())
		sa = &lsa
	}

	if err := syscall.Bind(s, sa); err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("bind (iface=%s, addr=%v): %w", ifaceName, localAddr, err)
	}

	f := os.NewFile(uintptr(s), "")
	c, err := net.FilePacketConn(f)
	f.Close()
	if err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("FilePacketConn (iface=%s): %w", ifaceName, err)
	}

	return c.(*net.UDPConn), nil
}
