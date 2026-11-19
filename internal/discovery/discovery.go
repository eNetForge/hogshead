// Package discovery lets hogshead servers on the same LAN notice each other.
// Each server multicasts a small JSON beacon every few seconds and keeps a
// table of beacons heard recently. The table only informs humans and the UI
// (pre-filling a replication target); nothing is trusted or acted on.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Group and Port are the multicast address beacons go to.
	Group = "239.255.77.77"
	Port  = 9470
	// Interval between beacons; a peer unseen for 4 intervals is dropped.
	Interval = 10 * time.Second
	maxAge   = 4 * Interval
	magic    = "hogshead/1"
)

// Beacon is what a server announces.
type Beacon struct {
	Magic   string `json:"magic"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Port    int    `json:"port"` // S3/UI listen port
	TLS     bool   `json:"tls"`
	Version string `json:"version"`
	// Cellar and CellarName identify the cellar the server belongs to, if any.
	Cellar     string `json:"cellar,omitempty"`
	CellarName string `json:"cellar_name,omitempty"`
}

// Peer is a server heard on the LAN.
type Peer struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Addr       string    `json:"addr"`     // ip:port
	Endpoint   string    `json:"endpoint"` // http(s)://ip:port
	Version    string    `json:"version"`
	LastSeen   time.Time `json:"last_seen"`
	Cellar     string    `json:"cellar,omitempty"`
	CellarName string    `json:"cellar_name,omitempty"`
}

// Self describes this server for the beacon; read on every tick so live
// config changes (name, TLS) show up without a restart.
type Self func() Beacon

// Service announces and listens.
type Service struct {
	self Self
	log  *slog.Logger
	now  func() time.Time

	mu    sync.Mutex
	peers map[string]Peer
}

// New builds a service.
func New(self Self, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{self: self, log: log, now: time.Now, peers: map[string]Peer{}}
}

// Run announces and listens until ctx ends. enabled is read every tick so
// discovery can be switched off and on by a config reload.
func (s *Service) Run(ctx context.Context, enabled func() bool) {
	group := &net.UDPAddr{IP: net.ParseIP(Group), Port: Port}
	var conn *net.UDPConn
	var sender *net.UDPConn
	stopListen := func() {
		if conn != nil {
			_ = conn.Close()
			conn = nil
		}
		if sender != nil {
			_ = sender.Close()
			sender = nil
		}
	}
	defer stopListen()
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		if enabled() {
			if conn == nil {
				c, err := net.ListenMulticastUDP("udp4", nil, group)
				if err != nil {
					s.log.Warn("discovery: listen failed", "err", err)
				} else {
					conn = c
					_ = conn.SetReadBuffer(1 << 16)
					go s.listen(ctx, conn)
				}
			}
			if sender == nil {
				c, err := net.DialUDP("udp4", nil, group)
				if err != nil {
					s.log.Warn("discovery: dial failed", "err", err)
				} else {
					sender = c
				}
			}
			if sender != nil {
				s.announce(sender)
			}
		} else if conn != nil || sender != nil {
			stopListen()
			s.mu.Lock()
			s.peers = map[string]Peer{}
			s.mu.Unlock()
		}
		s.expire()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) announce(sender *net.UDPConn) {
	b := s.self()
	b.Magic = magic
	if b.ID == "" || b.Port == 0 {
		return
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return
	}
	if _, err := sender.Write(raw); err != nil {
		s.log.Debug("discovery: announce failed", "err", err)
	}
}

func (s *Service) listen(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		var b Beacon
		if json.Unmarshal(buf[:n], &b) != nil || b.Magic != magic || b.ID == "" || b.Port <= 0 || b.Port > 65535 {
			continue
		}
		if self := s.self(); self.ID == b.ID {
			continue
		}
		scheme := "http"
		if b.TLS {
			scheme = "https"
		}
		addr := net.JoinHostPort(from.IP.String(), strconv.Itoa(b.Port))
		s.mu.Lock()
		s.peers[b.ID] = Peer{ID: b.ID, Name: strings.TrimSpace(b.Name), Addr: addr, Endpoint: fmt.Sprintf("%s://%s", scheme, addr), Version: b.Version, LastSeen: s.now(), Cellar: b.Cellar, CellarName: b.CellarName}
		s.mu.Unlock()
	}
}

func (s *Service) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.peers {
		if s.now().Sub(p.LastSeen) > maxAge {
			delete(s.peers, id)
		}
	}
}

// Peers returns the servers heard recently, sorted by name then address.
func (s *Service) Peers() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		if s.now().Sub(p.LastSeen) <= maxAge {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

// Observe records a beacon as if heard from addr; used by tests and by the
// listener.
func (s *Service) Observe(b Beacon, ip string) {
	scheme := "http"
	if b.TLS {
		scheme = "https"
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(b.Port))
	s.mu.Lock()
	s.peers[b.ID] = Peer{ID: b.ID, Name: b.Name, Addr: addr, Endpoint: scheme + "://" + addr, Version: b.Version, LastSeen: s.now(), Cellar: b.Cellar, CellarName: b.CellarName}
	s.mu.Unlock()
}
