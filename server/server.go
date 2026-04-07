// Package server implements the mpfpv2 relay server: UDP receive/forward,
// TUN device for server-originated traffic, heartbeat/session management,
// IP auto-allocation with JSON persistence, and periodic cleanup.
package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/cloud/mpfpv2/config"
	"github.com/cloud/mpfpv2/protocol"
	"github.com/cloud/mpfpv2/tunnel"
)

// AddrInfo tracks a single source address (UDP endpoint) of a client.
type AddrInfo struct {
	Addr       *net.UDPAddr
	LastSeen   time.Time
	NICName    string
	NICRTTms   uint16
	NICTxBytes uint64
	NICRxBytes uint64
	RxBytes    uint64
	TxBytes    uint64
}

// Session tracks a connected client.
type Session struct {
	ClientID   uint16
	VirtualIP  net.IP
	PrefixLen  uint8
	DeviceName string
	Addrs      map[string]*AddrInfo // key = addr.String()
	LastSeen   time.Time
	RxBytes    uint64
	TxBytes    uint64
}

// ipPoolEntry is the JSON-serializable form of an IP pool entry.
// Format is compatible with mpfpv v1.
type ipPoolEntry struct {
	ClientID uint16 `json:"clientID"`
	IP       string `json:"ip"`
	Name     string `json:"name,omitempty"`
}

// Server is the mpfpv2 relay server.
type Server struct {
	cfg  *config.Config
	conn *net.UDPConn

	sessions     map[uint16]*Session
	sessionsLock sync.RWMutex

	routeTable map[[4]byte]uint16
	routeLock  sync.RWMutex

	dedup       *protocol.Deduplicator
	teamKeyHash [8]byte

	tunDev          tunnel.Device
	serverIP        net.IP
	serverVirtualIP [4]byte
	prefixLen       uint8

	// IP auto-allocation.
	ipPool     map[uint16]net.IP
	ipPoolNames map[uint16]string
	subnet     *net.IPNet
	ipPoolFile string
	ipPoolLock sync.Mutex

	// Config file path for saving changes via Web UI.
	cfgPath string

	// Atomic counters.
	seq     uint32 // server-originated seq (clientID=0)
	totalRx uint64
	totalTx uint64

	// Timeouts.
	clientTimeout time.Duration
	addrTimeout   time.Duration
}

// New creates and initialises a Server from the given configuration.
func New(cfg *config.Config) (*Server, error) {
	if cfg.Server == nil {
		return nil, fmt.Errorf("server: missing server configuration")
	}

	s := &Server{
		cfg:           cfg,
		sessions:      make(map[uint16]*Session),
		routeTable:    make(map[[4]byte]uint16),
		dedup:         protocol.NewDeduplicator(cfg.Server.DedupWindow),
		teamKeyHash:   protocol.TeamKeyHash(cfg.TeamKey),
		clientTimeout: time.Duration(cfg.Server.ClientTimeout) * time.Second,
		addrTimeout:   time.Duration(cfg.Server.AddrTimeout) * time.Second,
		ipPool:        make(map[uint16]net.IP),
		ipPoolNames:   make(map[uint16]string),
		ipPoolFile:    cfg.Server.IPPoolFile,
	}

	// Parse server virtual IP.
	ip, ipNet, err := net.ParseCIDR(cfg.Server.VirtualIP)
	if err != nil {
		return nil, fmt.Errorf("server: invalid virtualIP: %w", err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("server: virtualIP must be IPv4")
	}
	copy(s.serverVirtualIP[:], ip4)
	s.serverIP = ip4
	ones, _ := ipNet.Mask.Size()
	s.prefixLen = uint8(ones)

	// Parse subnet for auto-allocation.
	if cfg.Server.Subnet != "" {
		_, subnet, err := net.ParseCIDR(cfg.Server.Subnet)
		if err != nil {
			return nil, fmt.Errorf("server: invalid subnet: %w", err)
		}
		s.subnet = subnet
	}

	s.loadIPPool()
	return s, nil
}

// SetConfigPath sets the path used when saving config changes via the Web UI.
func (s *Server) SetConfigPath(path string) { s.cfgPath = path }

// Run starts the server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.initTUN()

	addr, err := net.ResolveUDPAddr("udp", s.cfg.Server.ListenAddr)
	if err != nil {
		return fmt.Errorf("server: resolve listen addr: %w", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("server: listen: %w", err)
	}
	s.conn = conn
	defer conn.Close()

	log.Infof("server: listening on %s", addr)

	go s.cleanupLoop(ctx)

	if s.tunDev != nil {
		go s.tunReadLoop(ctx)
	}

	// Main UDP receive loop.
	buf := make([]byte, protocol.MTU+protocol.HeaderSize+100)
	for {
		select {
		case <-ctx.Done():
			if s.tunDev != nil {
				s.tunDev.Close()
			}
			return ctx.Err()
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			select {
			case <-ctx.Done():
				if s.tunDev != nil {
					s.tunDev.Close()
				}
				return ctx.Err()
			default:
			}
			log.Warnf("server: read error: %v", err)
			continue
		}

		s.handlePacket(buf[:n], from)
	}
}

// initTUN creates the TUN device. If it fails (e.g. not root), the server
// continues in relay-only mode.
func (s *Server) initTUN() {
	dev, err := tunnel.CreateTUN(tunnel.Config{
		Name:      "mpfpv0",
		MTU:       protocol.MTU,
		VirtualIP: s.serverIP,
		PrefixLen: int(s.prefixLen),
	})
	if err != nil {
		log.Warnf("server: TUN creation failed, relay-only mode: %v", err)
		return
	}
	s.tunDev = dev
	log.Infof("server: TUN %s created with IP %s/%d", dev.Name(), s.serverIP, s.prefixLen)
}

// tunReadLoop reads packets from the TUN device and forwards them to clients.
func (s *Server) tunReadLoop(ctx context.Context) {
	buf := make([]byte, protocol.MTU+protocol.HeaderSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := s.tunDev.Read(buf[protocol.HeaderSize:])
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Warnf("server: TUN read error: %v", err)
			continue
		}
		if n < 20 {
			continue
		}
		// Only IPv4.
		if buf[protocol.HeaderSize]>>4 != 4 {
			continue
		}

		payload := buf[protocol.HeaderSize : protocol.HeaderSize+n]
		var dstIP [4]byte
		copy(dstIP[:], payload[16:20])

		s.routeLock.RLock()
		clientID, ok := s.routeTable[dstIP]
		s.routeLock.RUnlock()
		if !ok {
			continue
		}

		seq := atomic.AddUint32(&s.seq, 1) - 1
		protocol.EncodeHeader(buf, &protocol.Header{
			Type:     protocol.TypeData,
			ClientID: 0,
			Seq:      seq,
		})

		s.sendToClient(clientID, buf[:protocol.HeaderSize+n])
	}
}

// handlePacket dispatches a received UDP packet by type.
func (s *Server) handlePacket(data []byte, from *net.UDPAddr) {
	if len(data) < protocol.HeaderSize {
		return
	}
	hdr, err := protocol.DecodeHeader(data)
	if err != nil {
		if log.IsLevelEnabled(log.DebugLevel) {
			log.Debugf("server: decode header from %s: %v", from, err)
		}
		return
	}

	pktLen := uint64(len(data))
	atomic.AddUint64(&s.totalRx, pktLen)

	// Update per-session/addr rx counters.
	s.sessionsLock.RLock()
	if sess, ok := s.sessions[hdr.ClientID]; ok {
		atomic.AddUint64(&sess.RxBytes, pktLen)
		if ai, ok := sess.Addrs[from.String()]; ok {
			atomic.AddUint64(&ai.RxBytes, pktLen)
		}
	}
	s.sessionsLock.RUnlock()

	payload := data[protocol.HeaderSize:]

	switch hdr.Type {
	case protocol.TypeHeartbeat:
		s.handleHeartbeat(hdr, payload, from)
	case protocol.TypeData:
		s.handleData(hdr, payload, from, data)
	default:
		if log.IsLevelEnabled(log.DebugLevel) {
			log.Debugf("server: unknown type 0x%02x from %s", hdr.Type, from)
		}
	}
}

// handleHeartbeat processes a heartbeat, manages sessions, allocates IPs.
func (s *Server) handleHeartbeat(hdr protocol.Header, payload []byte, from *net.UDPAddr) {
	hb, err := protocol.DecodeHeartbeat(payload)
	if err != nil {
		log.Warnf("server: decode heartbeat from %s: %v", from, err)
		return
	}

	if hb.TeamKeyHash != s.teamKeyHash {
		log.Warnf("server: teamKey mismatch from clientID=%d addr=%s", hdr.ClientID, from)
		s.sendHeartbeatAck(from, hdr.ClientID, net.IPv4zero, 0, protocol.AckStatusBadTeamKey, hdr.Seq)
		return
	}

	clientID := hdr.ClientID
	if clientID == 0 {
		log.Warnf("server: heartbeat with reserved clientID=0 from %s", from)
		return
	}

	vip := hb.VirtualIP.To4()
	if vip == nil {
		vip = net.IPv4zero.To4()
	}

	now := time.Now()

	s.sessionsLock.Lock()

	session, exists := s.sessions[clientID]
	if !exists {
		// New client — allocate IP if needed.
		assignedIP := vip
		assignedPrefix := hb.PrefixLen
		if assignedIP.Equal(net.IPv4zero) {
			allocated, allocPrefix := s.allocateIP(clientID, hb.DeviceName)
			if allocated != nil {
				assignedIP = allocated
				assignedPrefix = allocPrefix
				log.Infof("server: auto-assigned IP %s/%d to clientID=%d", assignedIP, assignedPrefix, clientID)
			} else {
				log.Warnf("server: IP allocation failed for clientID=%d", clientID)
			}
		}

		session = &Session{
			ClientID:   clientID,
			VirtualIP:  assignedIP,
			PrefixLen:  assignedPrefix,
			DeviceName: hb.DeviceName,
			Addrs:      make(map[string]*AddrInfo),
			LastSeen:   now,
		}
		s.sessions[clientID] = session

		// Update route table.
		if !assignedIP.Equal(net.IPv4zero) {
			var key [4]byte
			copy(key[:], assignedIP.To4())
			s.routeLock.Lock()
			s.routeTable[key] = clientID
			s.routeLock.Unlock()

			// Persist if not already in pool.
			s.ipPoolLock.Lock()
			if _, pooled := s.ipPool[clientID]; !pooled {
				s.ipPool[clientID] = assignedIP
				if hb.DeviceName != "" {
					s.ipPoolNames[clientID] = hb.DeviceName
				}
				s.saveIPPool()
			}
			s.ipPoolLock.Unlock()
		}

		log.Infof("server: new client: clientID=%d vip=%s name=%q from %s",
			clientID, assignedIP, hb.DeviceName, from)
	} else {
		// Existing client.
		session.LastSeen = now
		if hb.DeviceName != "" {
			session.DeviceName = hb.DeviceName
		}
	}

	// Update/add source address.
	addrKey := from.String()
	ai, addrExists := session.Addrs[addrKey]
	if !addrExists {
		ai = &AddrInfo{Addr: from}
		session.Addrs[addrKey] = ai
		log.Infof("server: clientID=%d added addr %s (total: %d)", clientID, addrKey, len(session.Addrs))
	}
	ai.LastSeen = now

	// Store per-NIC data from heartbeat extension.
	if len(hb.PathRTTs) > 0 {
		p := hb.PathRTTs[0]
		// Remove stale addrs with the same NIC name (port changed after reconnect).
		if p.Name != "" {
			for oldKey, oldAI := range session.Addrs {
				if oldKey != addrKey && oldAI.NICName == p.Name {
					delete(session.Addrs, oldKey)
					if log.IsLevelEnabled(log.DebugLevel) {
						log.Debugf("server: clientID=%d removed stale addr %s for NIC %s", clientID, oldKey, p.Name)
					}
				}
			}
		}
		ai.NICName = p.Name
		ai.NICRTTms = p.RTTms
		ai.NICTxBytes = p.TxBytes
		ai.NICRxBytes = p.RxBytes
	}

	ackVIP := session.VirtualIP
	ackPrefix := session.PrefixLen
	s.sessionsLock.Unlock()

	s.sendHeartbeatAck(from, clientID, ackVIP, ackPrefix, protocol.AckStatusOK, hdr.Seq)
}

// handleData processes a data packet: dedup, validate, route, forward.
func (s *Server) handleData(hdr protocol.Header, payload []byte, from *net.UDPAddr, rawPacket []byte) {
	clientID := hdr.ClientID

	if s.dedup.IsDuplicate(clientID, hdr.Seq) {
		if log.IsLevelEnabled(log.DebugLevel) {
			log.Debugf("server: dedup drop clientID=%d seq=%d", clientID, hdr.Seq)
		}
		return
	}

	s.sessionsLock.RLock()
	session, exists := s.sessions[clientID]
	if !exists {
		s.sessionsLock.RUnlock()
		if log.IsLevelEnabled(log.DebugLevel) {
			log.Debugf("server: data from unknown clientID=%d", clientID)
		}
		return
	}

	// Validate inner packet: must be IPv4, >= 20 bytes.
	if len(payload) < 20 || payload[0]>>4 != 4 {
		s.sessionsLock.RUnlock()
		return
	}

	// Verify source IP matches registered virtual IP.
	var srcIP [4]byte
	copy(srcIP[:], payload[12:16])
	var regIP [4]byte
	if ip4 := session.VirtualIP.To4(); ip4 != nil {
		copy(regIP[:], ip4)
	}
	if srcIP != regIP {
		s.sessionsLock.RUnlock()
		if log.IsLevelEnabled(log.DebugLevel) {
			log.Debugf("server: inner srcIP mismatch for clientID=%d, dropping", clientID)
		}
		return
	}

	// Update addr LastSeen.
	addrKey := from.String()
	if ai, ok := session.Addrs[addrKey]; ok {
		ai.LastSeen = time.Now()
	}
	session.LastSeen = time.Now()
	s.sessionsLock.RUnlock()

	// Route by destination IP.
	var dstIP [4]byte
	copy(dstIP[:], payload[16:20])

	// Destination is server itself — write to TUN.
	if dstIP == s.serverVirtualIP {
		if s.tunDev != nil {
			_, err := s.tunDev.Write(payload)
			if err != nil {
				log.Warnf("server: TUN write error: %v", err)
			}
		}
		return
	}

	s.routeLock.RLock()
	dstClientID, found := s.routeTable[dstIP]
	s.routeLock.RUnlock()
	if !found {
		if log.IsLevelEnabled(log.DebugLevel) {
			log.Debugf("server: no route for dst %d.%d.%d.%d", dstIP[0], dstIP[1], dstIP[2], dstIP[3])
		}
		return
	}

	s.sendToClient(dstClientID, rawPacket)
}

// sendToClient sends rawPacket to all known addresses of the client (redundant mode only).
func (s *Server) sendToClient(clientID uint16, rawPacket []byte) {
	s.sessionsLock.RLock()
	session, exists := s.sessions[clientID]
	if !exists || len(session.Addrs) == 0 {
		s.sessionsLock.RUnlock()
		return
	}

	// Snapshot addresses under read lock — avoid allocation on the common path
	// by using a small stack array.
	var stack [8]*net.UDPAddr
	addrs := stack[:0]
	for _, ai := range session.Addrs {
		addrs = append(addrs, ai.Addr)
	}
	s.sessionsLock.RUnlock()

	pktLen := uint64(len(rawPacket))

	for _, addr := range addrs {
		if _, err := s.conn.WriteToUDP(rawPacket, addr); err != nil {
			if log.IsLevelEnabled(log.DebugLevel) {
				log.Debugf("server: write to %s for clientID=%d: %v", addr, clientID, err)
			}
			continue
		}
		atomic.AddUint64(&s.totalTx, pktLen)
		// Update per-session and per-addr tx counters.
		s.sessionsLock.RLock()
		if sess, ok := s.sessions[clientID]; ok {
			atomic.AddUint64(&sess.TxBytes, pktLen)
			if ai, ok := sess.Addrs[addr.String()]; ok {
				atomic.AddUint64(&ai.TxBytes, pktLen)
			}
		}
		s.sessionsLock.RUnlock()
	}
}

// sendHeartbeatAck sends a HeartbeatAck to the given address.
func (s *Server) sendHeartbeatAck(to *net.UDPAddr, clientID uint16, assignedIP net.IP, prefixLen uint8, status uint8, seq uint32) {
	buf := protocol.GetBuf(protocol.HeaderSize + protocol.HeartbeatAckSize)
	defer protocol.PutBuf(buf)

	ackSeq := atomic.AddUint32(&s.seq, 1) - 1
	protocol.EncodeHeader(buf, &protocol.Header{
		Type:     protocol.TypeHeartbeatAck,
		ClientID: 0,
		Seq:      ackSeq,
	})
	protocol.EncodeHeartbeatAck(buf[protocol.HeaderSize:], &protocol.HeartbeatAck{
		AssignedIP: assignedIP,
		PrefixLen:  prefixLen,
		Status:     status,
		MTU:        protocol.MTU,
	})

	if s.conn != nil {
		if _, err := s.conn.WriteToUDP(buf, to); err != nil {
			log.Warnf("server: send ack to %s: %v", to, err)
		}
	}
}

// ---------- IP auto-allocation ----------

// allocateIP assigns a virtual IP to the given clientID from the subnet pool.
// Returns the existing allocation if one exists.
// Must NOT be called with ipPoolLock held.
func (s *Server) allocateIP(clientID uint16, deviceName string) (net.IP, uint8) {
	s.ipPoolLock.Lock()
	defer s.ipPoolLock.Unlock()

	if deviceName != "" {
		s.ipPoolNames[clientID] = deviceName
	}

	// Return existing allocation.
	if ip, ok := s.ipPool[clientID]; ok {
		if deviceName != "" {
			s.saveIPPool()
		}
		return ip, s.prefixLen
	}

	if s.subnet == nil {
		return nil, 0
	}

	// Build set of used IPs.
	used := make(map[[4]byte]bool, len(s.ipPool)+1)
	for _, ip := range s.ipPool {
		var key [4]byte
		copy(key[:], ip.To4())
		used[key] = true
	}
	used[s.serverVirtualIP] = true

	// Iterate subnet for next available.
	networkIP := s.subnet.IP.To4()
	mask := s.subnet.Mask
	ones, bits := mask.Size()
	hostBits := uint(bits - ones)
	maxHosts := (uint32(1) << hostBits) - 1
	baseIP := binary.BigEndian.Uint32(networkIP)

	for offset := uint32(1); offset < maxHosts; offset++ {
		var candidate [4]byte
		binary.BigEndian.PutUint32(candidate[:], baseIP+offset)
		if used[candidate] {
			continue
		}
		allocatedIP := make(net.IP, 4)
		copy(allocatedIP, candidate[:])
		s.ipPool[clientID] = allocatedIP
		s.saveIPPool()
		return allocatedIP, s.prefixLen
	}

	return nil, 0
}

func (s *Server) loadIPPool() {
	if s.ipPoolFile == "" {
		return
	}
	data, err := os.ReadFile(s.ipPoolFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("server: load IP pool: %v", err)
		}
		return
	}
	var entries []ipPoolEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Warnf("server: parse IP pool: %v", err)
		return
	}
	s.ipPoolLock.Lock()
	defer s.ipPoolLock.Unlock()
	for _, e := range entries {
		ip := net.ParseIP(e.IP).To4()
		if ip != nil {
			s.ipPool[e.ClientID] = ip
			if e.Name != "" {
				s.ipPoolNames[e.ClientID] = e.Name
			}
		}
	}
	log.Infof("server: loaded %d IP pool entries from %s", len(entries), s.ipPoolFile)
}

// saveIPPool persists the IP pool to disk. Must be called with ipPoolLock held.
func (s *Server) saveIPPool() {
	if s.ipPoolFile == "" {
		return
	}
	entries := make([]ipPoolEntry, 0, len(s.ipPool))
	for cid, ip := range s.ipPool {
		entries = append(entries, ipPoolEntry{
			ClientID: cid,
			IP:       ip.String(),
			Name:     s.ipPoolNames[cid],
		})
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Warnf("server: marshal IP pool: %v", err)
		return
	}
	if err := os.WriteFile(s.ipPoolFile, data, 0644); err != nil {
		log.Warnf("server: save IP pool: %v", err)
	}
}

// ---------- Cleanup ----------

func (s *Server) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanup()
		}
	}
}

func (s *Server) cleanup() {
	now := time.Now()

	type addrRm struct {
		clientID uint16
		key      string
	}
	type sessRm struct {
		clientID uint16
		ip       net.IP
	}

	var expAddrs []addrRm
	var expSess []sessRm

	s.sessionsLock.Lock()
	for cid, sess := range s.sessions {
		for key, ai := range sess.Addrs {
			if now.Sub(ai.LastSeen) > s.addrTimeout {
				delete(sess.Addrs, key)
				expAddrs = append(expAddrs, addrRm{cid, key})
			}
		}
		if len(sess.Addrs) == 0 && now.Sub(sess.LastSeen) > s.clientTimeout {
			ip4 := sess.VirtualIP.To4()
			delete(s.sessions, cid)
			expSess = append(expSess, sessRm{cid, ip4})
		}
	}
	s.sessionsLock.Unlock()

	for _, a := range expAddrs {
		log.Infof("server: clientID=%d addr %s timed out", a.clientID, a.key)
	}
	for _, sr := range expSess {
		if sr.ip != nil {
			var key [4]byte
			copy(key[:], sr.ip)
			s.routeLock.Lock()
			delete(s.routeTable, key)
			s.routeLock.Unlock()
		}
		s.dedup.Reset(sr.clientID)
		// Clean ipPool to prevent unbounded growth.
		s.ipPoolLock.Lock()
		delete(s.ipPool, sr.clientID)
		delete(s.ipPoolNames, sr.clientID)
		s.saveIPPool()
		s.ipPoolLock.Unlock()
		log.Infof("server: clientID=%d session timed out, removed from IP pool", sr.clientID)
	}
}
