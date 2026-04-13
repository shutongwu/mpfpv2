package protocol

import "sync"

// clientState holds per-clientID deduplication state.
type clientState struct {
	mu     sync.Mutex
	maxSeq uint32
	bitmap []uint64
	inited bool
}

// Deduplicator performs per-clientID sliding-window sequence deduplication.
// Each clientID has an independent lock, eliminating cross-client contention.
type Deduplicator struct {
	mu         sync.RWMutex
	windowSize uint32
	clients    map[uint16]*clientState
}

func NewDeduplicator(windowSize int) *Deduplicator {
	if windowSize <= 0 {
		windowSize = DefaultDedupWindow
	}
	return &Deduplicator{
		windowSize: uint32(windowSize),
		clients:    make(map[uint16]*clientState),
	}
}

// IsDuplicate returns true if the packet should be dropped.
func (d *Deduplicator) IsDuplicate(clientID uint16, seq uint32) bool {
	d.mu.RLock()
	cs, ok := d.clients[clientID]
	d.mu.RUnlock()

	if !ok {
		d.mu.Lock()
		cs, ok = d.clients[clientID]
		if !ok {
			cs = &clientState{bitmap: make([]uint64, (d.windowSize+63)/64)}
			d.clients[clientID] = cs
		}
		d.mu.Unlock()
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	if !cs.inited {
		cs.inited = true
		cs.maxSeq = seq
		cs.setBit(seq, d.windowSize)
		return false
	}

	diff := int64(seq) - int64(cs.maxSeq)
	if diff > int64(1<<31) {
		diff -= int64(1 << 32)
	} else if diff < -int64(1<<31) {
		diff += int64(1 << 32)
	}

	if diff > 0 {
		shift := diff
		if shift >= int64(d.windowSize) {
			for i := range cs.bitmap {
				cs.bitmap[i] = 0
			}
		} else {
			for i := int64(1); i <= shift; i++ {
				cs.clearBit(cs.maxSeq+uint32(i), d.windowSize)
			}
		}
		cs.maxSeq = seq
		cs.setBit(seq, d.windowSize)
		return false
	}

	if diff == 0 {
		return true
	}

	behind := -diff
	if behind >= int64(d.windowSize) {
		// With timestamp-based seq, far-behind packets are stale, not sender restarts.
		// Sender restarts produce a forward jump (new current time), handled by diff > 0 above.
		return true
	}

	if cs.getBit(seq, d.windowSize) {
		return true
	}
	cs.setBit(seq, d.windowSize)
	return false
}

func (d *Deduplicator) Reset(clientID uint16) {
	d.mu.Lock()
	delete(d.clients, clientID)
	d.mu.Unlock()
}

func (cs *clientState) setBit(seq, w uint32)          { i := seq % w; cs.bitmap[i/64] |= 1 << (i % 64) }
func (cs *clientState) clearBit(seq, w uint32)        { i := seq % w; cs.bitmap[i/64] &^= 1 << (i % 64) }
func (cs *clientState) getBit(seq, w uint32) bool      { i := seq % w; return cs.bitmap[i/64]&(1<<(i%64)) != 0 }
