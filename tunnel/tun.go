// Package tunnel provides TUN device creation across platforms.
package tunnel

import "net"

const DefaultName = "mpfpv0"

type Device interface {
	Read(buf []byte) (int, error)
	Write(buf []byte) (int, error)
	Close() error
	Name() string
}

type Config struct {
	Name      string
	MTU       int
	VirtualIP net.IP
	PrefixLen int
}

func CreateTUN(cfg Config) (Device, error) {
	if cfg.Name == "" {
		cfg.Name = DefaultName
	}
	if cfg.MTU <= 0 {
		cfg.MTU = 1400
	}
	return createPlatformTUN(cfg)
}
