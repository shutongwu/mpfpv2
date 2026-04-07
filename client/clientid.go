package client

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

const clientIDPath = "/etc/mpfpv/client_id"

// loadOrGenerateID loads a persistent clientID from file, or generates one
// deterministically from hostname + machineID using FNV-1a hash.
// The ID is in range [100, 65000) and persisted to /etc/mpfpv/client_id.
func loadOrGenerateID() uint16 {
	// Try to load from file.
	if data, err := os.ReadFile(clientIDPath); err == nil {
		s := strings.TrimSpace(string(data))
		if v, err := strconv.ParseUint(s, 10, 16); err == nil && v >= 100 && v < 65000 {
			log.Infof("loaded clientID=%d from %s", v, clientIDPath)
			return uint16(v)
		}
	}

	// Generate from hostname + machineID.
	hostname, _ := os.Hostname()
	mid := machineID()

	h := fnv.New32a()
	h.Write([]byte(hostname))
	h.Write([]byte(mid))
	id := uint16(h.Sum32()%64900) + 100

	// Try to persist.
	if err := os.MkdirAll(filepath.Dir(clientIDPath), 0755); err == nil {
		if err := os.WriteFile(clientIDPath, []byte(fmt.Sprintf("%d\n", id)), 0644); err == nil {
			log.Infof("generated and saved clientID=%d to %s", id, clientIDPath)
		} else {
			log.WithError(err).Debugf("could not save clientID to %s", clientIDPath)
		}
	}

	log.Infof("generated clientID=%d from hostname=%q", id, hostname)
	return id
}
