package broker

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strconv"
)

// ErrUnauthenticatedEndpoint prevents new clients from contacting legacy
// brokers. Never include discovery contents in errors: they contain a secret.
var ErrUnauthenticatedEndpoint = errors.New("invalid or legacy broker discovery file; stop the old broker, remove its stale broker.addr discovery file, then retry with this version")

// endpointRecord is published atomically in the owner-only discovery file.
// The credential is never placed in a URL or sent to a non-loopback address.
type endpointRecord struct {
	Version int    `json:"version"`
	Address string `json:"address"`
	Token   string `json:"token"`
}

func parseEndpoint(raw string) (endpointRecord, error) {
	var e endpointRecord
	if json.Unmarshal([]byte(raw), &e) != nil || e.Version != 2 {
		return e, ErrUnauthenticatedEndpoint
	}
	host, port, err := net.SplitHostPort(e.Address)
	ip := net.ParseIP(host)
	p, perr := strconv.Atoi(port)
	token, terr := hex.DecodeString(e.Token)
	if err != nil || ip == nil || !ip.IsLoopback() || perr != nil || p < 1 || p > 65535 || terr != nil || len(token) != 32 {
		return endpointRecord{}, ErrUnauthenticatedEndpoint
	}
	return e, nil
}
