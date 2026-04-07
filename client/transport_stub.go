//go:build !linux || android

package client

import "net"

// createBoundUDPConn creates a UDP connection bound to the given local address.
// On non-Linux platforms SO_BINDTODEVICE is not available, so we rely on
// address binding to route traffic through the correct interface.
func createBoundUDPConn(localAddr net.IP, ifaceName string) (*net.UDPConn, error) {
	network := "udp4"
	if localAddr.To4() == nil {
		network = "udp6"
	}
	return net.ListenUDP(network, &net.UDPAddr{IP: localAddr, Port: 0})
}
