package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
)

// NewHolder builds this process's lease identity, "<name>/<random>": name is what a person reads in
// cp_leases.holder and the logs (empty takes the hostname, an unresolvable hostname takes "unknown"),
// and the random half is drawn per start, because a name alone collides across processes on one machine.
func NewHolder(name string) string {
	return holderID(name, os.Hostname)
}

func holderID(name string, hostname func() (string, error)) string {
	name = strings.TrimSpace(name)
	if name == "" {
		if h, err := hostname(); err == nil {
			name = strings.TrimSpace(h)
		}
	}
	if name == "" {
		name = "unknown"
	}
	var b [8]byte
	_, _ = rand.Read(b[:])

	return name + "/" + hex.EncodeToString(b[:])
}
