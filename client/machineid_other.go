//go:build (!linux && !windows) || android

package client

import "os"

func machineID() string {
	name, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return "other-" + name
}
