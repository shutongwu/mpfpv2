//go:build windows

package tunnel

import (
	_ "embed"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

//go:embed wintun.dll
var wintunDLL []byte

func ensureWintunDLL() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	dllPath := filepath.Join(filepath.Dir(exePath), "wintun.dll")
	if info, err := os.Stat(dllPath); err == nil && info.Size() == int64(len(wintunDLL)) {
		return nil
	}
	return os.WriteFile(dllPath, wintunDLL, 0644)
}

type windowsTUN struct {
	dev       wgtun.Device
	name      string
	readBufs  [][]byte
	readSizes []int
}

func createPlatformTUN(cfg Config) (Device, error) {
	if err := ensureWintunDLL(); err != nil {
		return nil, fmt.Errorf("extract wintun.dll: %w", err)
	}
	dev, err := wgtun.CreateTUN(cfg.Name, cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("create wintun: %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		dev.Close()
		return nil, err
	}
	if cfg.VirtualIP != nil {
		mask := net.CIDRMask(cfg.PrefixLen, 32)
		maskStr := fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
		cmd := exec.Command("netsh", "interface", "ip", "set", "address",
			fmt.Sprintf("name=%s", name), "static", cfg.VirtualIP.String(), maskStr)
		if out, err := cmd.CombinedOutput(); err != nil {
			dev.Close()
			return nil, fmt.Errorf("netsh: %s: %w", out, err)
		}
	}
	readBufs := make([][]byte, 1)
	readBufs[0] = make([]byte, cfg.MTU+200)
	return &windowsTUN{dev: dev, name: name, readBufs: readBufs, readSizes: make([]int, 1)}, nil
}

func (t *windowsTUN) Read(buf []byte) (int, error) {
	for {
		n, err := t.dev.Read(t.readBufs, t.readSizes, 0)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		size := t.readSizes[0]
		if size > len(buf) {
			size = len(buf)
		}
		copy(buf[:size], t.readBufs[0][:size])
		return size, nil
	}
}

func (t *windowsTUN) Write(buf []byte) (int, error) {
	_, err := t.dev.Write([][]byte{buf}, 0)
	if err != nil {
		return 0, err
	}
	return len(buf), nil
}

func (t *windowsTUN) Close() error { return t.dev.Close() }
func (t *windowsTUN) Name() string { return t.name }
