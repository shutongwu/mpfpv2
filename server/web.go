package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/cloud/mpfpv2/config"
	"github.com/cloud/mpfpv2/protocol"
)

//go:embed static
var staticFS embed.FS

// ---------- API response types ----------

type clientInfoResp struct {
	ClientID   uint16        `json:"clientID"`
	VirtualIP  string        `json:"virtualIP"`
	DeviceName string        `json:"deviceName,omitempty"`
	Online     bool          `json:"online"`
	AddrCount  int           `json:"addrCount"`
	PathRTTs   []pathRTTResp `json:"pathRTTs,omitempty"`
	RxBytes    uint64        `json:"rxBytes"`
	TxBytes    uint64        `json:"txBytes"`
	LastSeen   string        `json:"lastSeen"`
}

type pathRTTResp struct {
	Name    string `json:"name"`
	RTTms   int    `json:"rttMs"`
	TxBytes uint64 `json:"txBytes"`
	RxBytes uint64 `json:"rxBytes"`
}

type addrDetailResp struct {
	Addr       string `json:"addr"`
	LastSeen   string `json:"lastSeen"`
	NICName    string `json:"nicName,omitempty"`
	NICRTTms   int    `json:"nicRttMs,omitempty"`
	NICTxBytes uint64 `json:"nicTxBytes,omitempty"`
	NICRxBytes uint64 `json:"nicRxBytes,omitempty"`
	RxBytes    uint64 `json:"rxBytes"`
	TxBytes    uint64 `json:"txBytes"`
}

type clientDetailResp struct {
	clientInfoResp
	Addrs []addrDetailResp `json:"addrs"`
}

type routeResp struct {
	VirtualIP string `json:"virtualIP"`
	ClientID  uint16 `json:"clientID"`
}

type statsResp struct {
	RxBytes     uint64 `json:"rxBytes"`
	TxBytes     uint64 `json:"txBytes"`
	DeviceCount int    `json:"deviceCount"`
	OnlineCount int    `json:"onlineCount"`
}

type serverConfigResp struct {
	TeamKey    string `json:"teamKey"`
	ListenAddr string `json:"listenAddr"`
}

type connStringResp struct {
	ConnString string `json:"connectionString"`
}

// ---------- Web UI startup ----------

// StartWebUI starts the embedded HTTP server for the Web UI and API.
// It supports graceful shutdown via the provided context.
// teamKey and listenAddr are used to generate the connection string.
func StartWebUI(ctx context.Context, addr string, s *Server, teamKey, listenAddr string) error {
	mux := http.NewServeMux()
	registerRoutes(mux, s, teamKey, listenAddr)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Warnf("web: shutdown error: %v", err)
		}
	}()

	log.Infof("web: starting on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("web: %w", err)
	}
	return nil
}

func registerRoutes(mux *http.ServeMux, s *Server, teamKey, listenAddr string) {
	// Static files.
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("web: embedded static fs: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	// --- Clients ---
	mux.HandleFunc("/api/clients", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, getClients(s))
	})

	mux.HandleFunc("/api/clients/", func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimPrefix(r.URL.Path, "/api/clients/")
		idStr = strings.TrimSuffix(idStr, "/")
		id, err := strconv.ParseUint(idStr, 10, 16)
		if err != nil {
			http.Error(w, "invalid client ID", http.StatusBadRequest)
			return
		}
		clientID := uint16(id)

		switch r.Method {
		case http.MethodGet:
			detail := getClient(s, clientID)
			if detail == nil {
				http.Error(w, "client not found", http.StatusNotFound)
				return
			}
			writeJSON(w, detail)
		case http.MethodDelete:
			if err := deleteClient(s, clientID); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]string{"status": "deleted"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// --- Routes ---
	mux.HandleFunc("/api/routes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, getRoutes(s))
	})

	// --- Stats ---
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, getStats(s))
	})

	// --- Connection string ---
	mux.HandleFunc("/api/connection-string", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Use the request Host to derive the public address, replacing the
		// web UI port with the UDP listen port.
		host := r.Host
		if host == "" {
			host = listenAddr
		}
		// Extract host part (may include web port), replace port with UDP port.
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			h = host // no port in Host header
		}
		_, udpPort, _ := net.SplitHostPort(listenAddr)
		if udpPort == "" {
			udpPort = "9800"
		}
		cs := fmt.Sprintf("mpfpv://%s@%s:%s", teamKey, h, udpPort)
		writeJSON(w, connStringResp{ConnString: cs})
	})

	// --- v1 compat: /api/devices → same as /api/clients ---
	mux.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, getClients(s))
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/devices/", func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimPrefix(r.URL.Path, "/api/devices/")
		idStr = strings.TrimSuffix(idStr, "/")
		id, err := strconv.ParseUint(idStr, 10, 16)
		if err != nil {
			http.Error(w, "invalid ID", http.StatusBadRequest)
			return
		}
		clientID := uint16(id)
		switch r.Method {
		case http.MethodGet:
			detail := getClient(s, clientID)
			if detail == nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(w, detail)
		case http.MethodDelete:
			if err := deleteClient(s, clientID); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]string{"status": "deleted"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// --- Client downloads (served from disk, not embedded) ---
	mux.Handle("/download/", http.StripPrefix("/download/", http.FileServer(http.Dir("/opt/mpfpv/download"))))

	// --- Server config ---
	mux.HandleFunc("/api/server-config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, getServerConfig(s))
		case http.MethodPost:
			var req serverConfigResp
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := updateServerConfig(s, req.TeamKey, req.ListenAddr); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"status": "saved"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// ---------- Data gathering ----------

func getClients(s *Server) []clientInfoResp {
	// Snapshot session data under lock, format outside.
	type snapshot struct {
		id         uint16
		vip        net.IP
		name       string
		lastSeen   time.Time
		addrCount  int
		rx, tx     uint64
		pathRTTs   []pathRTTResp
	}

	s.sessionsLock.RLock()
	snaps := make([]snapshot, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sn := snapshot{
			id:        sess.ClientID,
			vip:       sess.VirtualIP,
			name:      sess.DeviceName,
			lastSeen:  sess.LastSeen,
			addrCount: len(sess.Addrs),
			rx:        atomic.LoadUint64(&sess.RxBytes),
			tx:        atomic.LoadUint64(&sess.TxBytes),
		}
		for _, ai := range sess.Addrs {
			if ai.NICName != "" {
				sn.pathRTTs = append(sn.pathRTTs, pathRTTResp{
					Name:    ai.NICName,
					RTTms:   int(ai.NICRTTms),
					TxBytes: ai.NICTxBytes,
					RxBytes: ai.NICRxBytes,
				})
			}
		}
		snaps = append(snaps, sn)
	}
	s.sessionsLock.RUnlock()

	// Format + sort outside lock.
	now := time.Now()
	result := make([]clientInfoResp, len(snaps))
	for i, sn := range snaps {
		result[i] = clientInfoResp{
			ClientID:   sn.id,
			VirtualIP:  sn.vip.String(),
			DeviceName: sn.name,
			Online:     now.Sub(sn.lastSeen) < s.clientTimeout,
			AddrCount:  sn.addrCount,
			RxBytes:    sn.rx,
			TxBytes:    sn.tx,
			LastSeen:   sn.lastSeen.Format(time.RFC3339),
			PathRTTs:   sn.pathRTTs,
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ClientID < result[j].ClientID
	})
	return result
}

func getClient(s *Server, id uint16) *clientDetailResp {
	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}

	now := time.Now()
	detail := &clientDetailResp{
		clientInfoResp: clientInfoResp{
			ClientID:   sess.ClientID,
			VirtualIP:  sess.VirtualIP.String(),
			DeviceName: sess.DeviceName,
			Online:     now.Sub(sess.LastSeen) < s.clientTimeout,
			AddrCount:  len(sess.Addrs),
			RxBytes:    atomic.LoadUint64(&sess.RxBytes),
			TxBytes:    atomic.LoadUint64(&sess.TxBytes),
			LastSeen:   sess.LastSeen.Format(time.RFC3339),
		},
		Addrs: make([]addrDetailResp, 0, len(sess.Addrs)),
	}

	for _, ai := range sess.Addrs {
		detail.Addrs = append(detail.Addrs, addrDetailResp{
			Addr:       ai.Addr.String(),
			LastSeen:   ai.LastSeen.Format(time.RFC3339),
			NICName:    ai.NICName,
			NICRTTms:   int(ai.NICRTTms),
			NICTxBytes: ai.NICTxBytes,
			NICRxBytes: ai.NICRxBytes,
			RxBytes:    atomic.LoadUint64(&ai.RxBytes),
			TxBytes:    atomic.LoadUint64(&ai.TxBytes),
		})
		if ai.NICName != "" {
			detail.PathRTTs = append(detail.PathRTTs, pathRTTResp{
				Name:    ai.NICName,
				RTTms:   int(ai.NICRTTms),
				TxBytes: ai.NICTxBytes,
				RxBytes: ai.NICRxBytes,
			})
		}
	}
	return detail
}

func deleteClient(s *Server, id uint16) error {
	s.sessionsLock.Lock()
	sess, ok := s.sessions[id]
	if !ok {
		s.sessionsLock.Unlock()
		return fmt.Errorf("client %d not found", id)
	}
	ip4 := sess.VirtualIP.To4()
	delete(s.sessions, id)
	s.sessionsLock.Unlock()

	if ip4 != nil {
		var key [4]byte
		copy(key[:], ip4)
		s.routeLock.Lock()
		delete(s.routeTable, key)
		s.routeLock.Unlock()
	}
	s.dedup.Reset(id)

	// Also remove from IP pool so the device can re-register.
	s.ipPoolLock.Lock()
	delete(s.ipPool, id)
	delete(s.ipPoolNames, id)
	s.saveIPPool()
	s.ipPoolLock.Unlock()

	return nil
}

func getRoutes(s *Server) []routeResp {
	s.routeLock.RLock()
	defer s.routeLock.RUnlock()

	routes := make([]routeResp, 0, len(s.routeTable))
	for ipBytes, cid := range s.routeTable {
		routes = append(routes, routeResp{
			VirtualIP: net.IP(ipBytes[:]).String(),
			ClientID:  cid,
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].ClientID < routes[j].ClientID
	})
	return routes
}

func getStats(s *Server) statsResp {
	s.ipPoolLock.Lock()
	deviceCount := len(s.ipPool)
	s.ipPoolLock.Unlock()

	s.sessionsLock.RLock()
	now := time.Now()
	online := 0
	for _, sess := range s.sessions {
		if now.Sub(sess.LastSeen) < s.clientTimeout {
			online++
		}
	}
	s.sessionsLock.RUnlock()

	return statsResp{
		RxBytes:     atomic.LoadUint64(&s.totalRx),
		TxBytes:     atomic.LoadUint64(&s.totalTx),
		DeviceCount: deviceCount,
		OnlineCount: online,
	}
}

func getServerConfig(s *Server) serverConfigResp {
	return serverConfigResp{
		TeamKey:    s.cfg.TeamKey,
		ListenAddr: s.cfg.Server.ListenAddr,
	}
}

func updateServerConfig(s *Server, teamKey, listenAddr string) error {
	if teamKey != "" && teamKey != s.cfg.TeamKey {
		s.cfg.TeamKey = teamKey
		s.teamKeyHash = protocol.TeamKeyHash(teamKey)
	}
	if listenAddr != "" {
		s.cfg.Server.ListenAddr = listenAddr
	}
	if s.cfgPath != "" {
		return config.SaveFile(s.cfgPath, s.cfg)
	}
	return nil
}

// ---------- Helpers ----------

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Warnf("web: JSON encode: %v", err)
	}
}
