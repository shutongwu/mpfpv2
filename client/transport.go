package client

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cloud/mpfpv2/protocol"
	log "github.com/sirupsen/logrus"
)

// Path health states.
const (
	PathProbing = iota // newly discovered, awaiting first heartbeat ack
	PathActive         // confirmed via ack, used for data
	PathSuspect        // send error occurred
	PathDown           // consecutive 5 heartbeat misses
)

const (
	rttWindowSize      = 10
	missThresholdDown  = 5
	recvChanSize       = 256 // ~360ms at 700pps, with default drop on full
	recvBufSize        = 65535
	pollInterval       = 200 * time.Millisecond
	pathStaleTimeout   = 8 * time.Second
	recycleMinInterval = 10 * time.Second
	recycleMaxAttempts = 2
	recycleBackoff     = 60 * time.Second
)

// PathStatus is the health state of a network path.
type PathStatus int

func (s PathStatus) String() string {
	switch s {
	case PathProbing:
		return "probing"
	case PathActive:
		return "active"
	case PathSuspect:
		return "suspect"
	case PathDown:
		return "down"
	default:
		return "unknown"
	}
}

// Path represents a single network path through one interface.
type Path struct {
	IfaceName    string
	LocalAddr    net.IP
	Conn         *net.UDPConn
	RTT          time.Duration
	rttSamples   []time.Duration
	LastRecv     time.Time
	Status       PathStatus
	missCount    int
	TxBytes      uint64
	RxBytes      uint64
	lastRecycled time.Time
	recycleCount int
	mu           sync.Mutex
	closed       chan struct{}
}

// RecvPacket is a packet received on any path.
type RecvPacket struct {
	Data     []byte
	FromPath string
	Addr     *net.UDPAddr
}

// PathInfo is a read-only snapshot for diagnostics.
type PathInfo struct {
	IfaceName string
	LocalAddr string
	RTT       time.Duration
	LastRecv  time.Time
	Status    string
	TxBytes   uint64
	RxBytes   uint64
}

// MultiPath manages sending data over multiple network interfaces (redundant only).
type MultiPath struct {
	serverAddr *net.UDPAddr
	paths      map[string]*Path
	excluded   map[string]bool
	recvCh     chan RecvPacket
	recvConn   *net.UDPConn // central unbound recv socket
	recvPort   uint16
	mu         sync.RWMutex
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

// NewMultiPath creates a multipath sender targeting serverAddr.
// excluded is a list of interface names to skip.
func NewMultiPath(serverAddr *net.UDPAddr, excluded []string) (*MultiPath, error) {
	if serverAddr == nil {
		return nil, fmt.Errorf("transport: serverAddr must not be nil")
	}
	ex := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		ex[name] = true
	}
	return &MultiPath{
		serverAddr: serverAddr,
		paths:      make(map[string]*Path),
		excluded:   ex,
		recvCh:     make(chan RecvPacket, recvChanSize),
		stopCh:     make(chan struct{}),
	}, nil
}

// Start creates the central recv socket, discovers interfaces, and starts loops.
func (m *MultiPath) Start() error {
	// Central recv socket: try dual-stack, fall back to IPv4.
	recvConn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6zero, Port: 0})
	if err != nil {
		recvConn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			return fmt.Errorf("transport: create recv socket: %w", err)
		}
	}
	m.recvConn = recvConn
	m.recvPort = uint16(recvConn.LocalAddr().(*net.UDPAddr).Port)
	log.WithField("port", m.recvPort).Info("central recv socket created")

	// Central recv loop.
	m.wg.Add(1)
	go m.centralRecvLoop()

	// Initial interface scan.
	for name, info := range m.scanInterfaces() {
		m.addPath(name, info)
	}

	// Start polling.
	m.wg.Add(1)
	go m.pollInterfaces()

	return nil
}

// Stop shuts down all paths and goroutines.
func (m *MultiPath) Stop() {
	select {
	case <-m.stopCh:
		return
	default:
		close(m.stopCh)
	}

	m.mu.Lock()
	for name, p := range m.paths {
		close(p.closed)
		p.Conn.Close()
		delete(m.paths, name)
	}
	m.mu.Unlock()

	if m.recvConn != nil {
		m.recvConn.Close()
	}
	m.wg.Wait()
}

// Send sends data redundantly through all Active paths.
// Probing and Down paths are skipped.
func (m *MultiPath) Send(data []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.paths) == 0 {
		return fmt.Errorf("transport: no available paths")
	}

	sent := 0
	for _, p := range m.paths {
		p.mu.Lock()
		st := p.Status
		p.mu.Unlock()
		if st != PathActive {
			continue
		}
		if _, err := p.Conn.WriteToUDP(data, m.serverAddr); err != nil {
			p.mu.Lock()
			p.Status = PathSuspect
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		p.TxBytes += uint64(len(data))
		p.mu.Unlock()
		sent++
	}
	if sent == 0 {
		return fmt.Errorf("transport: all paths failed or not active")
	}
	return nil
}

// SendAllHeartbeat sends a heartbeat through all non-Down paths (including Probing),
// appending per-path data to each copy.
func (m *MultiPath) SendAllHeartbeat(baseBuf []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var lastErr error
	for _, p := range m.paths {
		p.mu.Lock()
		st := p.Status
		rttMs := uint16(p.RTT.Milliseconds())
		txBytes := uint32(p.TxBytes)
		rxBytes := uint32(p.RxBytes)
		p.mu.Unlock()

		if st == PathDown {
			continue
		}

		// Append: \x00 [nameLen] [name] [rtt 2B] [tx 4B] [rx 4B]
		nameBytes := []byte(p.IfaceName)
		suffixLen := 1 + 1 + len(nameBytes) + 2 + 4 + 4
		pktLen := len(baseBuf) + suffixLen
		pkt := protocol.GetBuf(pktLen)
		copy(pkt, baseBuf)

		off := len(baseBuf)
		pkt[off] = 0x00 // separator
		off++
		pkt[off] = byte(len(nameBytes))
		off++
		off += copy(pkt[off:], nameBytes)
		pkt[off] = byte(rttMs >> 8)
		pkt[off+1] = byte(rttMs)
		off += 2
		pkt[off] = byte(txBytes >> 24)
		pkt[off+1] = byte(txBytes >> 16)
		pkt[off+2] = byte(txBytes >> 8)
		pkt[off+3] = byte(txBytes)
		off += 4
		pkt[off] = byte(rxBytes >> 24)
		pkt[off+1] = byte(rxBytes >> 16)
		pkt[off+2] = byte(rxBytes >> 8)
		pkt[off+3] = byte(rxBytes)

		if _, err := p.Conn.WriteToUDP(pkt[:pktLen], m.serverAddr); err != nil {
			p.mu.Lock()
			p.Status = PathSuspect
			p.mu.Unlock()
			lastErr = err
		} else {
			p.mu.Lock()
			p.TxBytes += uint64(pktLen)
			p.mu.Unlock()
		}
		protocol.PutBuf(pkt)
	}
	return lastErr
}

// RecvChan returns the channel on which received packets are delivered.
func (m *MultiPath) RecvChan() <-chan RecvPacket {
	return m.recvCh
}

// RecvPort returns the central recv socket port (sent in heartbeat as ReplyPort).
func (m *MultiPath) RecvPort() uint16 {
	return m.recvPort
}

// UpdateRTT records a new RTT sample for the named path. If the path was
// Probing, it transitions to Active.
func (m *MultiPath) UpdateRTT(ifaceName string, rtt time.Duration) {
	m.mu.RLock()
	p, ok := m.paths[ifaceName]
	m.mu.RUnlock()
	if !ok {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.rttSamples = append(p.rttSamples, rtt)
	if len(p.rttSamples) > rttWindowSize {
		p.rttSamples = p.rttSamples[len(p.rttSamples)-rttWindowSize:]
	}
	p.RTT = averageDuration(p.rttSamples)
	p.LastRecv = time.Now()
	p.missCount = 0
	p.recycleCount = 0

	if p.Status == PathProbing {
		log.WithFields(log.Fields{
			"iface": ifaceName,
			"rtt":   rtt,
		}).Info("path confirmed active")
	}
	p.Status = PathActive
}

// GetPaths returns a snapshot of all paths for diagnostics.
func (m *MultiPath) GetPaths() []PathInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]PathInfo, 0, len(m.paths))
	for _, p := range m.paths {
		p.mu.Lock()
		pi := PathInfo{
			IfaceName: p.IfaceName,
			LocalAddr: p.LocalAddr.String(),
			RTT:       p.RTT,
			LastRecv:  p.LastRecv,
			Status:    p.Status.String(),
			TxBytes:   p.TxBytes,
			RxBytes:   p.RxBytes,
		}
		p.mu.Unlock()
		out = append(out, pi)
	}
	return out
}

// CheckAndRecycleStalePaths detects paths with stale NAT and recreates sockets.
func (m *MultiPath) CheckAndRecycleStalePaths() {
	m.mu.RLock()
	var stale []string
	now := time.Now()
	for name, p := range m.paths {
		p.mu.Lock()
		st := p.Status
		lr := p.LastRecv
		lrc := p.lastRecycled
		rc := p.recycleCount
		p.mu.Unlock()

		if st == PathDown {
			if rc >= recycleMaxAttempts && now.Sub(lrc) > recycleBackoff {
				stale = append(stale, name)
			}
			continue
		}
		if now.Sub(lr) <= pathStaleTimeout {
			continue
		}
		if rc >= recycleMaxAttempts {
			p.mu.Lock()
			p.Status = PathDown
			p.mu.Unlock()
			log.WithField("iface", name).Warn("path down after max recycle attempts")
			continue
		}
		if !lrc.IsZero() && now.Sub(lrc) <= recycleMinInterval {
			continue
		}
		stale = append(stale, name)
	}
	m.mu.RUnlock()

	for _, name := range stale {
		m.recyclePath(name)
	}
}

// --- internal methods ---

// centralRecvLoop reads from the central unbound socket.
func (m *MultiPath) centralRecvLoop() {
	defer m.wg.Done()
	buf := make([]byte, recvBufSize)
	for {
		n, addr, err := m.recvConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-m.stopCh:
				return
			default:
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		data := protocol.GetBuf(n)
		copy(data, buf[:n])
		select {
		case m.recvCh <- RecvPacket{Data: data, FromPath: "central", Addr: addr}:
		case <-m.stopCh:
			return
		default:
			protocol.PutBuf(data)
		}
	}
}

// perPathRecvLoop reads from a per-NIC socket for NAT compatibility.
func (m *MultiPath) perPathRecvLoop(p *Path) {
	defer m.wg.Done()
	buf := make([]byte, recvBufSize)
	for {
		n, addr, err := p.Conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-m.stopCh:
				return
			case <-p.closed:
				return
			default:
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		p.mu.Lock()
		p.RxBytes += uint64(n)
		p.mu.Unlock()

		data := protocol.GetBuf(n)
		copy(data, buf[:n])
		select {
		case m.recvCh <- RecvPacket{Data: data, FromPath: p.IfaceName, Addr: addr}:
		case <-m.stopCh:
			return
		default:
			protocol.PutBuf(data)
		}
	}
}

// addPath creates a new Path for the named interface.
func (m *MultiPath) addPath(name string, info *ifaceInfo) {
	var localAddr net.IP
	if m.serverAddr.IP.To4() != nil {
		if len(info.addrs4) == 0 {
			return
		}
		localAddr = info.addrs4[0]
	} else {
		if len(info.addrs6) == 0 {
			return
		}
		localAddr = info.addrs6[0]
	}

	conn, err := createBoundUDPConn(localAddr, name)
	if err != nil {
		log.WithFields(log.Fields{
			"iface": name,
			"addr":  localAddr,
		}).WithError(err).Warn("failed to create bound socket")
		return
	}

	p := &Path{
		IfaceName: name,
		LocalAddr: localAddr,
		Conn:      conn,
		Status:    PathProbing,
		LastRecv:  time.Now(),
		closed:    make(chan struct{}),
	}

	m.mu.Lock()
	m.paths[name] = p
	m.mu.Unlock()

	log.WithFields(log.Fields{
		"iface":  name,
		"addr":   localAddr,
		"status": "probing",
	}).Info("path added")

	m.wg.Add(1)
	go m.perPathRecvLoop(p)
}

// removePath closes and removes the named path.
func (m *MultiPath) removePath(name string) {
	m.mu.RLock()
	p, ok := m.paths[name]
	m.mu.RUnlock()
	if !ok {
		return
	}

	close(p.closed)
	p.Conn.Close()

	m.mu.Lock()
	delete(m.paths, name)
	m.mu.Unlock()

	log.WithField("iface", name).Info("path removed")
}

// recyclePath recreates a path's socket for fresh NAT mapping.
func (m *MultiPath) recyclePath(name string) {
	m.mu.Lock()
	p, ok := m.paths[name]
	if !ok {
		m.mu.Unlock()
		return
	}

	oldClosed := p.closed
	close(oldClosed)
	p.Conn.Close()

	conn, err := createBoundUDPConn(p.LocalAddr, p.IfaceName)
	if err != nil {
		p.mu.Lock()
		p.Status = PathDown
		p.mu.Unlock()
		m.mu.Unlock()
		log.WithField("iface", name).WithError(err).Warn("recycle failed")
		return
	}

	p.mu.Lock()
	p.Conn = conn
	p.closed = make(chan struct{})
	p.Status = PathProbing
	p.lastRecycled = time.Now()
	p.recycleCount++
	p.mu.Unlock()

	m.wg.Add(1)
	go m.perPathRecvLoop(p)

	m.mu.Unlock()

	log.WithFields(log.Fields{
		"iface":   name,
		"recycle": p.recycleCount,
	}).Info("path socket recycled")
}

// --- interface discovery ---

type ifaceInfo struct {
	addrs4 []net.IP
	addrs6 []net.IP
}

// scanInterfaces returns usable physical network interfaces.
func (m *MultiPath) scanInterfaces() map[string]*ifaceInfo {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.WithError(err).Warn("scanInterfaces failed")
		return nil
	}

	result := make(map[string]*ifaceInfo)
	for _, iface := range ifaces {
		name := iface.Name
		if m.excluded[name] {
			continue
		}
		if !isPhysicalInterface(name) {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		var v4s, v6s []net.IP
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}

			if ip4 := ip.To4(); ip4 != nil {
				if ip4[0] == 169 && ip4[1] == 254 { // link-local
					continue
				}
				if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 { // CGNAT
					continue
				}
				if ip4[0] == 10 && ip4[1] == 99 { // our virtual IP
					continue
				}
				v4s = append(v4s, ip4)
				continue
			}

			ip6 := ip.To16()
			if ip6 == nil {
				continue
			}
			if ip6[0] == 0xfe && ip6[1]&0xc0 == 0x80 { // link-local
				continue
			}
			if ip6.Equal(net.IPv6loopback) {
				continue
			}
			if (ip6[0]&0xe0 == 0x20) || (ip6[0]&0xfe == 0xfc) { // global / ULA
				v6s = append(v6s, ip6)
			}
		}

		if len(v4s) == 0 && len(v6s) == 0 {
			continue
		}
		result[name] = &ifaceInfo{addrs4: v4s, addrs6: v6s}
	}
	return result
}

// pollInterfaces polls for interface changes every 200ms.
func (m *MultiPath) pollInterfaces() {
	defer m.wg.Done()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.detectChanges()
		}
	}
}

// detectChanges compares current interfaces with known paths.
func (m *MultiPath) detectChanges() {
	newSet := m.scanInterfaces()

	m.mu.RLock()
	currentNames := make(map[string]bool, len(m.paths))
	for name := range m.paths {
		currentNames[name] = true
	}
	m.mu.RUnlock()

	// Detect removed.
	for name := range currentNames {
		if _, exists := newSet[name]; !exists {
			m.removePath(name)
		}
	}
	// Detect added.
	for name, info := range newSet {
		if !currentNames[name] {
			m.addPath(name, info)
		}
	}
	// Detect address changes.
	m.mu.RLock()
	for name, info := range newSet {
		if p, ok := m.paths[name]; ok {
			p.mu.Lock()
			localAddr := p.LocalAddr
			p.mu.Unlock()

			changed := false
			if m.serverAddr.IP.To4() != nil {
				if len(info.addrs4) > 0 && !info.addrs4[0].Equal(localAddr) {
					changed = true
				}
			} else {
				if len(info.addrs6) > 0 && !info.addrs6[0].Equal(localAddr) {
					changed = true
				}
			}
			if changed {
				m.mu.RUnlock()
				m.removePath(name)
				m.addPath(name, info)
				m.mu.RLock()
			}
		}
	}
	m.mu.RUnlock()
}

// isPhysicalInterface returns true for real physical NIC names.
func isPhysicalInterface(name string) bool {
	// Linux
	for _, prefix := range []string{
		"eth", "enp", "ens", "eno", "enx",
		"wlan", "wlp", "wlx",
		"usb",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	// Windows
	for _, kw := range []string{
		"Wi-Fi", "WiFi", "Wireless", "WLAN",
		"Ethernet", "以太网",
		"USB",
	} {
		if strings.Contains(name, kw) {
			return true
		}
	}
	// macOS
	if len(name) >= 2 && name[:2] == "en" {
		return true
	}
	return false
}

// averageDuration computes the arithmetic mean of a duration slice.
func averageDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}
