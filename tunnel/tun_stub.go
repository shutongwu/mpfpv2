//go:build (!linux && !windows) || android

package tunnel

import "fmt"

func createPlatformTUN(cfg Config) (Device, error) {
	return nil, fmt.Errorf("TUN not supported on this platform")
}
