//go:build linux && !android

package client

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// createBoundUDPConn creates a UDP socket bound to a specific NIC via
// SO_BINDTODEVICE. Also sets SO_REUSEADDR and SO_SNDTIMEO(50ms).
func createBoundUDPConn(localAddr net.IP, ifaceName string) (*net.UDPConn, error) {
	isIPv6 := localAddr.To4() == nil
	af := syscall.AF_INET
	if isIPv6 {
		af = syscall.AF_INET6
	}

	s, err := syscall.Socket(af, syscall.SOCK_DGRAM, syscall.IPPROTO_UDP)
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

	// 50ms send timeout prevents blocking on dead NICs.
	tv := syscall.Timeval{Sec: 0, Usec: 50000}
	if err := syscall.SetsockoptTimeval(s, syscall.SOL_SOCKET, syscall.SO_SNDTIMEO, &tv); err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("SO_SNDTIMEO (iface=%s): %w", ifaceName, err)
	}

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
