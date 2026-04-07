//go:build linux && !android

package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	tunDevice = "/dev/net/tun"
	ifnamsiz  = 16
	iffTUN    = 0x0001
	iffNOPI   = 0x1000
	tunSetIff = 0x400454ca
)

type ifReq struct {
	Name  [ifnamsiz]byte
	Flags uint16
	_     [22]byte
}

type linuxTUN struct {
	file *os.File
	name string
}

func createPlatformTUN(cfg Config) (Device, error) {
	fd, err := syscall.Open(tunDevice, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", tunDevice, err)
	}

	var req ifReq
	copy(req.Name[:], cfg.Name)
	req.Flags = iffTUN | iffNOPI
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(tunSetIff), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("ioctl TUNSETIFF: %w", errno)
	}

	devName := cfg.Name
	for i, b := range req.Name {
		if b == 0 {
			devName = string(req.Name[:i])
			break
		}
	}

	file := os.NewFile(uintptr(fd), tunDevice)
	dev := &linuxTUN{file: file, name: devName}

	if cfg.VirtualIP != nil {
		cidr := fmt.Sprintf("%s/%d", cfg.VirtualIP, cfg.PrefixLen)
		if out, err := exec.Command("ip", "addr", "add", cidr, "dev", devName).CombinedOutput(); err != nil {
			dev.Close()
			return nil, fmt.Errorf("ip addr add: %s: %w", out, err)
		}
	}
	if out, err := exec.Command("ip", "link", "set", devName, "mtu", fmt.Sprint(cfg.MTU)).CombinedOutput(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("ip link set mtu: %s: %w", out, err)
	}
	if out, err := exec.Command("ip", "link", "set", devName, "up").CombinedOutput(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("ip link set up: %s: %w", out, err)
	}
	return dev, nil
}

func (d *linuxTUN) Read(buf []byte) (int, error)  { return d.file.Read(buf) }
func (d *linuxTUN) Write(buf []byte) (int, error) { return d.file.Write(buf) }
func (d *linuxTUN) Close() error                  { return d.file.Close() }
func (d *linuxTUN) Name() string                  { return d.name }
