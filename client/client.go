// Package client implements the mpfpv2 client: TUN device management,
// heartbeat exchange, multipath UDP transport, and packet forwarding.
package client

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloud/mpfpv2/config"
	"github.com/cloud/mpfpv2/protocol"
	"github.com/cloud/mpfpv2/tunnel"
	log "github.com/sirupsen/logrus"
)

const (
	heartbeatInterval  = 1 * time.Second
	maxUDPPacketSize   = 65535
	registerTimeout    = 30 * time.Second
)

// Client is the mpfpv2 client.
type Client struct {
	cfg         *config.Config
	clientID    uint16
	conn        *net.UDPConn   // single-socket mode
	serverAddr  *net.UDPAddr
	dedup       *protocol.Deduplicator
	teamKeyHash [8]byte
	virtualIP   atomic.Value // net.IP
	prefixLen   uint8
	deviceName  string
	seq         atomic.Uint32
	registered  atomic.Int32 // 1 = registered
	tunDev      tunnel.Device
	tunReady    chan struct{} // closed when TUN is up
	mu          sync.Mutex

	// Multi-path.
	multipath    *MultiPath
	useMultipath bool

	// RTT for single-socket mode.
	lastHBSent time.Time
	hbMu       sync.Mutex
}

// New creates a new Client from cfg. clientID is loaded from persistent file
// or generated deterministically. DNS resolution is attempted here but
// failures are non-fatal — the client will retry in the heartbeat loop.
func New(cfg *config.Config) (*Client, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("client: client config section is nil")
	}
	cc := cfg.Client

	// Try resolving now; if DNS fails, store the raw address for later retry.
	serverAddr, err := net.ResolveUDPAddr("udp", cc.ServerAddr)
	if err != nil {
		log.Warnf("client: DNS resolve %q failed (%v), will retry", cc.ServerAddr, err)
		serverAddr = nil // resolved later in heartbeat loop
	}

	deviceName, _ := os.Hostname()
	clientID := loadOrGenerateID()

	dedupWindow := cc.DedupWindow
	if dedupWindow <= 0 {
		dedupWindow = protocol.DefaultDedupWindow
	}

	c := &Client{
		cfg:         cfg,
		clientID:    clientID,
		serverAddr:  serverAddr,
		dedup:       protocol.NewDeduplicator(dedupWindow),
		teamKeyHash: protocol.TeamKeyHash(cfg.TeamKey),
		deviceName:  deviceName,
		tunReady:    make(chan struct{}),
	}
	c.virtualIP.Store(net.IPv4zero.To4())

	addrStr := "<pending DNS>"
	if serverAddr != nil {
		addrStr = serverAddr.String()
	}
	log.WithFields(log.Fields{
		"clientID":   clientID,
		"deviceName": deviceName,
		"server":     addrStr,
	}).Info("client created")

	return c, nil
}

// resolveServer retries DNS resolution if serverAddr is still nil.
func (c *Client) resolveServer() bool {
	if c.serverAddr != nil {
		return true
	}
	addr, err := net.ResolveUDPAddr("udp", c.cfg.Client.ServerAddr)
	if err != nil {
		return false
	}
	c.serverAddr = addr
	log.Infof("client: DNS resolved → %s", addr)
	return true
}

// Run starts the client. It blocks until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	cc := c.cfg.Client

	// --- socket setup ---
	if cc.BindInterface == "auto" {
		// Auto mode: unbound socket, OS picks the route. Used on Windows.
		conn, err := net.ListenUDP("udp", nil)
		if err != nil {
			return fmt.Errorf("client: listen UDP: %w", err)
		}
		c.conn = conn
		defer conn.Close()
		log.Info("client: single socket mode (auto)")
	} else if cc.BindInterface != "" {
		// Explicit NIC binding.
		localAddr, err := resolveInterfaceAddr(cc.BindInterface)
		if err != nil {
			return fmt.Errorf("client: bind interface %q: %w", cc.BindInterface, err)
		}
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: localAddr})
		if err != nil {
			return fmt.Errorf("client: listen on %s: %w", localAddr, err)
		}
		c.conn = conn
		defer conn.Close()
		log.WithFields(log.Fields{
			"iface": cc.BindInterface,
			"addr":  localAddr,
		}).Info("client: single NIC mode")
	} else if c.serverAddr != nil && !c.serverAddr.IP.IsLoopback() {
		// Multi-path mode.
		mp, err := NewMultiPath(c.serverAddr, nil, c.dedup)
		if err == nil {
			if startErr := mp.Start(); startErr == nil {
				c.multipath = mp
				c.useMultipath = true
				log.Info("client: multipath mode enabled")
			} else {
				log.WithError(startErr).Warn("client: multipath start failed, falling back")
			}
		} else {
			log.WithError(err).Warn("client: multipath init failed, falling back")
		}
	}

	// Fallback: unbound socket.
	if !c.useMultipath && c.conn == nil {
		conn, err := net.ListenUDP("udp", nil)
		if err != nil {
			return fmt.Errorf("client: listen UDP: %w", err)
		}
		c.conn = conn
		defer conn.Close()
	} else if c.useMultipath {
		defer c.multipath.Stop()
	}

	log.WithFields(log.Fields{
		"clientID":     c.clientID,
		"useMultipath": c.useMultipath,
	}).Info("client starting")

	// --- goroutines ---
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		if c.useMultipath {
			c.recvLoopMultipath(ctx)
		} else {
			c.recvLoop(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		c.heartbeatLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		c.tunReadLoop(ctx)
	}()

	// Registration timeout warning.
	go func() {
		select {
		case <-time.After(registerTimeout):
			if c.registered.Load() == 0 {
				log.Warnf("failed to register with server after %s", registerTimeout)
			}
		case <-ctx.Done():
		}
	}()

	<-ctx.Done()
	log.Info("client shutting down")

	if c.conn != nil {
		c.conn.Close()
	}
	if c.tunDev != nil {
		c.tunDev.Close()
	}
	wg.Wait()
	return nil
}

// tunReadLoop reads IP packets from TUN and sends them to the server.
func (c *Client) tunReadLoop(ctx context.Context) {
	// Wait for TUN to be created (after first HeartbeatAck).
	if c.tunDev == nil {
		select {
		case <-c.tunReady:
		case <-ctx.Done():
			return
		}
	}
	if c.tunDev == nil {
		return
	}

	buf := protocol.GetBuf(protocol.MTU + protocol.HeaderSize)
	defer protocol.PutBuf(buf)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := c.tunDev.Read(buf[protocol.HeaderSize:])
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.WithError(err).Warn("TUN read error")
			continue
		}
		if n < 20 {
			continue
		}

		// Only forward IPv4.
		if buf[protocol.HeaderSize]>>4 != 4 {
			continue
		}

		// Rewrite source IP to virtualIP (fixes wrong src on Windows).
		vip := c.virtualIP.Load().(net.IP)
		if len(vip) == 4 && !vip.Equal(net.IPv4zero) {
			copy(buf[protocol.HeaderSize+12:protocol.HeaderSize+16], vip)
			recalcIPv4Checksum(buf[protocol.HeaderSize : protocol.HeaderSize+n])
		}

		// Encode header.
		seq := c.nextSeq()
		hdr := &protocol.Header{
			Type:     protocol.TypeData,
			ClientID: c.clientID,
			Seq:      seq,
		}
		protocol.EncodeHeader(buf, hdr)

		pkt := buf[:protocol.HeaderSize+n]
		if c.useMultipath {
			if err := c.multipath.Send(pkt); err != nil {
				log.WithError(err).Debug("multipath send failed")
			}
		} else {
			if _, err := c.conn.WriteToUDP(pkt, c.serverAddr); err != nil {
				log.WithError(err).Debug("send failed")
			}
		}
	}
}

// heartbeatLoop sends heartbeats every second.
func (c *Client) heartbeatLoop(ctx context.Context) {
	// Send first heartbeat immediately.
	if err := c.sendHeartbeat(); err != nil {
		log.WithError(err).Warn("initial heartbeat failed")
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.sendHeartbeat(); err != nil {
				log.WithError(err).Warn("heartbeat failed")
			}
			if c.useMultipath {
				c.multipath.CheckAndRecycleStalePaths()
			}
		}
	}
}

// sendHeartbeat encodes and sends a heartbeat. In multipath mode, it sends
// through all paths with per-path data appended.
func (c *Client) sendHeartbeat() error {
	if !c.resolveServer() {
		return fmt.Errorf("DNS not resolved yet")
	}
	vip := c.virtualIP.Load().(net.IP)

	var replyPort uint16
	if c.multipath != nil {
		replyPort = c.multipath.RecvPort()
	}

	hb := &protocol.Heartbeat{
		VirtualIP:   vip,
		PrefixLen:   c.prefixLen,
		ReplyPort:   replyPort,
		TeamKeyHash: c.teamKeyHash,
	}

	// Allocate buffer: header + heartbeat + deviceName + path data margin.
	buf := make([]byte, protocol.HeaderSize+protocol.HeartbeatPayloadMin+len(c.deviceName)+256)

	seq := c.nextSeq()
	protocol.EncodeHeader(buf, &protocol.Header{
		Type:     protocol.TypeHeartbeat,
		ClientID: c.clientID,
		Seq:      seq,
	})

	payloadLen := protocol.EncodeHeartbeatFull(buf[protocol.HeaderSize:], hb, c.deviceName)
	buf = buf[:protocol.HeaderSize+payloadLen]

	// Record send time for RTT.
	c.hbMu.Lock()
	c.lastHBSent = time.Now()
	c.hbMu.Unlock()

	if c.useMultipath {
		return c.multipath.SendAllHeartbeat(buf)
	}

	if _, err := c.conn.WriteToUDP(buf, c.serverAddr); err != nil {
		return fmt.Errorf("send heartbeat: %w", err)
	}
	log.WithField("seq", seq).Debug("heartbeat sent")
	return nil
}

// recvLoop reads from a single UDP socket.
func (c *Client) recvLoop(ctx context.Context) {
	buf := make([]byte, maxUDPPacketSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.WithError(err).Warn("recv error")
			continue
		}
		if n < protocol.HeaderSize {
			continue
		}

		hdr, err := protocol.DecodeHeader(buf[:n])
		if err != nil {
			continue
		}

		payload := buf[protocol.HeaderSize:n]
		switch hdr.Type {
		case protocol.TypeHeartbeatAck:
			c.handleHeartbeatAck(hdr, payload)
		case protocol.TypeData:
			c.handleData(hdr, payload)
		}
	}
}

// recvLoopMultipath reads from the multipath receive channel.
func (c *Client) recvLoopMultipath(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-c.multipath.RecvChan():
			if !ok {
				return
			}
			if len(pkt.Data) < protocol.HeaderSize {
				protocol.PutBuf(pkt.Data)
				continue
			}
			hdr, err := protocol.DecodeHeader(pkt.Data)
			if err != nil {
				protocol.PutBuf(pkt.Data)
				continue
			}

			payload := pkt.Data[protocol.HeaderSize:]
			switch hdr.Type {
			case protocol.TypeHeartbeatAck:
				c.handleHeartbeatAck(hdr, payload)
				// Update RTT for the path this ack came from.
				c.hbMu.Lock()
				sent := c.lastHBSent
				c.hbMu.Unlock()
				if !sent.IsZero() && pkt.FromPath != "central" {
					c.multipath.UpdateRTT(pkt.FromPath, time.Since(sent))
				}
			case protocol.TypeData:
				c.handleData(hdr, payload)
			}
			protocol.PutBuf(pkt.Data)
		}
	}
}

// handleHeartbeatAck processes a server ack.
func (c *Client) handleHeartbeatAck(hdr protocol.Header, payload []byte) {
	ack, err := protocol.DecodeHeartbeatAck(payload)
	if err != nil {
		log.WithError(err).Warn("bad heartbeat ack")
		return
	}

	switch ack.Status {
	case protocol.AckStatusOK:
		wasRegistered := c.registered.Load() == 1

		c.virtualIP.Store(ack.AssignedIP.To4())
		c.mu.Lock()
		c.prefixLen = ack.PrefixLen
		c.mu.Unlock()
		c.registered.Store(1)

		if !wasRegistered {
			log.WithFields(log.Fields{
				"virtualIP": ack.AssignedIP,
				"prefixLen": ack.PrefixLen,
			}).Info("registered with server")
			c.setupTUN()
			close(c.tunReady)
		}

	case protocol.AckStatusBadTeamKey:
		log.Error("heartbeat ack: teamKey mismatch")
	case protocol.AckStatusIDConflict:
		log.Error("heartbeat ack: clientID conflict")
	default:
		log.WithField("status", ack.Status).Warn("heartbeat ack: unknown status")
	}
}

// handleData processes an incoming data packet.
func (c *Client) handleData(hdr protocol.Header, payload []byte) {
	if c.dedup.IsDuplicate(hdr.ClientID, hdr.Seq) {
		return
	}
	if c.tunDev == nil {
		return
	}
	// Copy payload because the caller's buffer may be reused.
	pkt := protocol.GetBuf(len(payload))
	copy(pkt, payload)
	if _, err := c.tunDev.Write(pkt); err != nil {
		log.WithError(err).Warn("TUN write error")
	}
	protocol.PutBuf(pkt)
}

// setupTUN creates the TUN device with the assigned virtual IP.
func (c *Client) setupTUN() {
	vip := c.virtualIP.Load().(net.IP)
	c.mu.Lock()
	pl := c.prefixLen
	c.mu.Unlock()

	dev, err := tunnel.CreateTUN(tunnel.Config{
		Name:      tunnel.DefaultName,
		MTU:       protocol.MTU,
		VirtualIP: vip,
		PrefixLen: int(pl),
	})
	if err != nil {
		log.WithError(err).Warn("TUN creation failed")
		return
	}
	c.tunDev = dev
	log.WithFields(log.Fields{
		"device":    dev.Name(),
		"virtualIP": vip,
		"prefixLen": pl,
	}).Info("TUN device created")
}

// resolveInterfaceAddr finds a usable IP on the named interface.
func resolveInterfaceAddr(ifaceName string) (net.IP, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("interface %q not found: %w", ifaceName, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("get addrs for %q: %w", ifaceName, err)
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip4 := ip.To4(); ip4 != nil {
			return ip4, nil
		}
	}
	return nil, fmt.Errorf("no usable IPv4 address on %q", ifaceName)
}

// recalcIPv4Checksum recalculates the IPv4 header checksum in-place.
func recalcIPv4Checksum(ipHeader []byte) {
	ihl := int(ipHeader[0]&0x0f) * 4
	if ihl < 20 || ihl > len(ipHeader) {
		return
	}
	ipHeader[10] = 0
	ipHeader[11] = 0
	var sum uint32
	for i := 0; i < ihl; i += 2 {
		sum += uint32(ipHeader[i])<<8 | uint32(ipHeader[i+1])
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	cs := ^uint16(sum)
	ipHeader[10] = byte(cs >> 8)
	ipHeader[11] = byte(cs)
}

// WaitRegistered blocks until the client registers with the server.
func (c *Client) WaitRegistered() {
	<-c.tunReady
}

// VirtualIP returns the assigned virtual IP as a string.
func (c *Client) VirtualIP() string {
	vip := c.virtualIP.Load().(net.IP)
	if vip == nil {
		return ""
	}
	return vip.String()
}

// nextSeq returns a monotonically increasing millisecond timestamp for use as Seq.
// During bursts (multiple packets per ms), it increments past the current ms.
// After the burst, it re-syncs to real time.
func (c *Client) nextSeq() uint32 {
	now := uint32(time.Now().UnixMilli())
	for {
		old := c.seq.Load()
		next := now
		if next <= old {
			next = old + 1
		}
		if c.seq.CompareAndSwap(old, next) {
			return next
		}
	}
}
