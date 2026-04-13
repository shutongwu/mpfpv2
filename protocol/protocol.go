// Package protocol implements the mpfpv wire protocol: 8-byte UDP header,
// heartbeat/ack encoding, team key hashing, and sliding-window deduplication.
package protocol

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"
	"sync"
)

// Wire format constants.
const (
	HeaderSize          = 8
	HeartbeatPayloadMin = 16 // fixed part of heartbeat
	HeartbeatAckSize    = 8
	Version1            = 0x10

	TypeData         = 0x00
	TypeHeartbeat    = 0x01
	TypeHeartbeatAck = 0x02

	AckStatusOK           = 0x00
	AckStatusBadTeamKey   = 0x01
	AckStatusIDConflict   = 0x02

	DefaultDedupWindow = 65536
	MTU                = 1400 // fixed TUN MTU for all nodes
	StaleThresholdMs   = 200  // server drops packets older than this (ms)
)

var (
	ErrTooShort       = errors.New("protocol: buffer too short")
	ErrBadVersion     = errors.New("protocol: unsupported version")
)

// Header is the 8-byte per-packet encapsulation.
//
//	Byte 0:   Version(4 high) | Type(4 low)
//	Byte 1:   NIC-ID (sender's network interface index)
//	Byte 2-3: ClientID  (big-endian uint16)
//	Byte 4-7: Seq       (big-endian uint32, millisecond timestamp)
type Header struct {
	Type     uint8
	NICID    uint8
	ClientID uint16
	Seq      uint32
}

func EncodeHeader(buf []byte, h *Header) {
	buf[0] = Version1 | (h.Type & 0x0F)
	buf[1] = h.NICID
	binary.BigEndian.PutUint16(buf[2:4], h.ClientID)
	binary.BigEndian.PutUint32(buf[4:8], h.Seq)
}

func DecodeHeader(buf []byte) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, ErrTooShort
	}
	if buf[0]&0xF0 != Version1 {
		return Header{}, ErrBadVersion
	}
	return Header{
		Type:     buf[0] & 0x0F,
		NICID:    buf[1],
		ClientID: binary.BigEndian.Uint16(buf[2:4]),
		Seq:      binary.BigEndian.Uint32(buf[4:8]),
	}, nil
}

// PathRTT holds per-NIC stats reported by the client inside a heartbeat.
type PathRTT struct {
	NICID   uint8
	Name    string
	RTTms   uint16
	TxBytes uint64
	RxBytes uint64
}

// Heartbeat is the heartbeat payload (16 bytes fixed + variable extensions).
type Heartbeat struct {
	VirtualIP   net.IP
	PrefixLen   uint8
	ReplyPort   uint16
	TeamKeyHash [8]byte
	DeviceName  string    // from extension
	PathRTTs    []PathRTT // from extension
}

func EncodeHeartbeat(buf []byte, hb *Heartbeat) {
	ip := hb.VirtualIP.To4()
	if ip == nil {
		ip = net.IPv4zero.To4()
	}
	copy(buf[0:4], ip)
	buf[4] = hb.PrefixLen
	buf[5] = 0 // sendMode byte, kept for wire compat, always 0 (redundant)
	buf[6] = byte(hb.ReplyPort >> 8)
	buf[7] = byte(hb.ReplyPort)
	copy(buf[8:16], hb.TeamKeyHash[:])
}

// EncodeHeartbeatFull writes fixed payload + device name. Returns bytes written.
func EncodeHeartbeatFull(buf []byte, hb *Heartbeat, deviceName string) int {
	EncodeHeartbeat(buf, hb)
	n := HeartbeatPayloadMin
	if deviceName != "" {
		n += copy(buf[n:], deviceName)
	}
	return n
}

func DecodeHeartbeat(buf []byte) (Heartbeat, error) {
	if len(buf) < HeartbeatPayloadMin {
		return Heartbeat{}, ErrTooShort
	}
	var hash [8]byte
	copy(hash[:], buf[8:16])
	hb := Heartbeat{
		VirtualIP:   net.IP(append([]byte(nil), buf[0:4]...)),
		PrefixLen:   buf[4],
		ReplyPort:   uint16(buf[6])<<8 | uint16(buf[7]),
		TeamKeyHash: hash,
	}
	if len(buf) <= HeartbeatPayloadMin {
		return hb, nil
	}
	ext := buf[HeartbeatPayloadMin:]
	sep := -1
	for i, b := range ext {
		if b == 0x00 {
			sep = i
			break
		}
	}
	if sep < 0 {
		hb.DeviceName = string(ext)
		return hb, nil
	}
	hb.DeviceName = string(ext[:sep])
	// v2.2 multi-path format: [pathCount 1B] [nicID 1B nameLen 1B name rtt 2B tx 4B rx 4B] × N
	d := ext[sep+1:]
	if len(d) < 1 {
		return hb, nil
	}
	pathCount := int(d[0])
	pos := 1
	for i := 0; i < pathCount; i++ {
		if pos+2 > len(d) {
			break
		}
		nicID := d[pos]
		nameLen := int(d[pos+1])
		pos += 2
		if pos+nameLen+10 > len(d) {
			break
		}
		name := string(d[pos : pos+nameLen])
		pos += nameLen
		rtt := uint16(d[pos])<<8 | uint16(d[pos+1])
		pos += 2
		tx := uint64(binary.BigEndian.Uint32(d[pos:]))
		pos += 4
		rx := uint64(binary.BigEndian.Uint32(d[pos:]))
		pos += 4
		hb.PathRTTs = append(hb.PathRTTs, PathRTT{NICID: nicID, Name: name, RTTms: rtt, TxBytes: tx, RxBytes: rx})
	}
	return hb, nil
}

// HeartbeatAck is the 8-byte server reply.
type HeartbeatAck struct {
	AssignedIP net.IP
	PrefixLen  uint8
	Status     uint8
	MTU        uint16
}

func EncodeHeartbeatAck(buf []byte, ack *HeartbeatAck) {
	ip := ack.AssignedIP.To4()
	if ip == nil {
		ip = net.IPv4zero.To4()
	}
	copy(buf[0:4], ip)
	buf[4] = ack.PrefixLen
	buf[5] = ack.Status
	buf[6] = byte(ack.MTU >> 8)
	buf[7] = byte(ack.MTU)
}

func DecodeHeartbeatAck(buf []byte) (HeartbeatAck, error) {
	if len(buf) < HeartbeatAckSize {
		return HeartbeatAck{}, ErrTooShort
	}
	return HeartbeatAck{
		AssignedIP: net.IP(append([]byte(nil), buf[0:4]...)),
		PrefixLen:  buf[4],
		Status:     buf[5],
		MTU:        uint16(buf[6])<<8 | uint16(buf[7]),
	}, nil
}

// EncodePathExtension encodes multi-path NIC stats into buf.
// Format: [pathCount 1B] [nicID 1B nameLen 1B name rtt 2B tx 4B rx 4B] × N
// Returns bytes written.
func EncodePathExtension(buf []byte, paths []PathRTT) int {
	if len(paths) == 0 {
		return 0
	}
	buf[0] = byte(len(paths))
	pos := 1
	for _, p := range paths {
		nameBytes := []byte(p.Name)
		buf[pos] = p.NICID
		buf[pos+1] = byte(len(nameBytes))
		pos += 2
		pos += copy(buf[pos:], nameBytes)
		buf[pos] = byte(p.RTTms >> 8)
		buf[pos+1] = byte(p.RTTms)
		pos += 2
		binary.BigEndian.PutUint32(buf[pos:], uint32(p.TxBytes))
		pos += 4
		binary.BigEndian.PutUint32(buf[pos:], uint32(p.RxBytes))
		pos += 4
	}
	return pos
}

// SeqIsNewer returns true if seq is ahead of ref (handles uint32 wraparound).
func SeqIsNewer(seq, ref uint32) bool {
	return int32(seq-ref) > 0
}

// TeamKeyHash returns the first 8 bytes of SHA-256(key).
func TeamKeyHash(key string) [8]byte {
	sum := sha256.Sum256([]byte(key))
	var out [8]byte
	copy(out[:], sum[:8])
	return out
}

// ---------- buffer pool ----------

const BufSize = 2048 // MTU(1400) + header(8) + margin

var bufPool = sync.Pool{New: func() any { b := make([]byte, BufSize); return b }}

func GetBuf(size int) []byte {
	buf := bufPool.Get().([]byte)
	if cap(buf) >= size {
		return buf[:size]
	}
	bufPool.Put(buf)
	return make([]byte, size)
}

func PutBuf(buf []byte) { bufPool.Put(buf[:cap(buf)]) }
