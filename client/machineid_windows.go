//go:build windows

package client

import (
	"golang.org/x/sys/windows/registry"
)

func machineID() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE)
	if err != nil {
		return "windows-unknown"
	}
	defer k.Close()
	val, _, err := k.GetStringValue("MachineGuid")
	if err != nil || val == "" {
		return "windows-unknown"
	}
	return val
}
