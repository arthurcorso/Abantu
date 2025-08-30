package cluster

import (
	"encoding/json"
	"net"
	"sync"
	"time"

	"github.com/arthurcorso/abantu/internal/config"
	"log/slog"
	"crypto/hmac"
	"crypto/sha256"
	"github.com/arthurcorso/abantu/internal/metrics"
)

type State struct {
	Version int               `json:"version"`          // version globale (incrément merge)
	Clock   uint64            `json:"clock"`            // horloge logique lamport
	Hosts   map[string]HostInfo `json:"hosts"`
}

type HostInfo struct {
	Domain     string `json:"domain"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	Version    int    `json:"version"`       // version monotone (timestamp seconde)
	Lamport    uint64 `json:"lamport"`       // horloge logique par host
	Deleted    bool   `json:"deleted,omitempty"`
	BackendsJSON string `json:"backends_json,omitempty"`
	Strategy string `json:"strategy,omitempty"`
}

type Cluster struct {
	mu     sync.RWMutex
	state  State
	peers  []string
	addr   string
	cfg    *config.Config
	clock  uint64
	lastSnapshotVersion int
}

func New(addr string, peers []string) *Cluster { return &Cluster{addr: addr, peers: peers, state: State{Hosts: map[string]HostInfo{}}} }

// AttachConfig permet au cluster d'écrire les hosts appris dans la configuration locale (persistance automatique)
func (c *Cluster) AttachConfig(cfg *config.Config) { c.cfg = cfg }

func (c *Cluster) bumpClock(ext uint64) uint64 { c.clock++; if ext > c.clock { c.clock = ext + 1 }; return c.clock }

func (c *Cluster) UpdateHost(h HostInfo) {
	c.mu.Lock(); defer c.mu.Unlock()
	cur, ok := c.state.Hosts[h.Domain]
	if ok {
		if h.Version < cur.Version { return }
		if h.Version == cur.Version && h.Lamport <= cur.Lamport { return }
	}
	h.Lamport = c.bumpClock(h.Lamport)
	c.state.Hosts[h.Domain] = h
	c.state.Version++
	c.state.Clock = c.clock
}

func (c *Cluster) DeleteHost(domain string, version int) { c.UpdateHost(HostInfo{Domain: domain, Version: version, Deleted: true}) }

func (c *Cluster) GetHost(domain string) (HostInfo, bool) {
	c.mu.RLock(); defer c.mu.RUnlock()
	h, ok := c.state.Hosts[domain]
	return h, ok
}

func (c *Cluster) Snapshot() State {
	c.mu.RLock(); defer c.mu.RUnlock()
	cp := make(map[string]HostInfo, len(c.state.Hosts))
	for k, v := range c.state.Hosts { cp[k] = v }
	return State{Version: c.state.Version, Clock: c.state.Clock, Hosts: cp}
}

func (c *Cluster) gossipOnce() {
	st := c.Snapshot()
	// Diff: si aucune évolution de Version depuis dernier snapshot complet, on n'envoie que les hosts modifiés (Lamport > baseline).
	var payload State
	c.mu.RLock()
	baseVersion := c.lastSnapshotVersion
	if st.Version > baseVersion {
		// collect changed
		changed := make(map[string]HostInfo)
		for d, h := range st.Hosts { if h.Version >= baseVersion { changed[d] = h } }
		payload = State{Version: st.Version, Clock: st.Clock, Hosts: changed}
	} else {
		payload = State{Version: st.Version, Clock: st.Clock, Hosts: map[string]HostInfo{}}
	}
	c.mu.RUnlock()
	b, _ := json.Marshal(payload)
	// signature HMAC si secret présent
	var packet []byte
	if c.cfg != nil && c.cfg.Cluster.Secret != "" {
		h := hmac.New(sha256.New, []byte(c.cfg.Cluster.Secret))
		h.Write(b)
		sig := h.Sum(nil)
		wrapper := struct {
			Sig []byte `json:"sig"`
			Data json.RawMessage `json:"data"`
		}{Sig: sig, Data: b}
		packet, _ = json.Marshal(wrapper)
	} else { packet = b }
	for _, peer := range c.peers {
		go func(pr string) {
			conn, err := net.DialTimeout("udp", pr, 500*time.Millisecond)
			if err != nil { return }
			defer conn.Close(); _, _ = conn.Write(packet)
		}(peer)
	}
	c.mu.Lock(); c.lastSnapshotVersion = st.Version; c.mu.Unlock()
}

func (c *Cluster) receiveLoop() {
	addr, err := net.ResolveUDPAddr("udp", c.addr)
	if err != nil { slog.Warn("cluster.resolve_error", "err", err); return }
	conn, err := net.ListenUDP("udp", addr)
	if err != nil { slog.Warn("cluster.listen_error", "err", err); return }
	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil { continue }
		var st State
		payload := buf[:n]
		metrics.GossipPackets.Inc()
		if c.cfg != nil && c.cfg.Cluster.Secret != "" {
			// wrapper attendu
			var wrap struct{ Sig []byte `json:"sig"`; Data json.RawMessage `json:"data"` }
			if err := json.Unmarshal(payload, &wrap); err != nil { continue }
			h := hmac.New(sha256.New, []byte(c.cfg.Cluster.Secret))
			h.Write(wrap.Data)
			if !hmac.Equal(wrap.Sig, h.Sum(nil)) { metrics.GossipSigInvalid.Inc(); slog.Warn("cluster.sig_invalid"); continue }
			if err := json.Unmarshal(wrap.Data, &st); err != nil { continue }
		} else {
			if err := json.Unmarshal(payload, &st); err != nil { continue }
		}
		c.merge(st)
	}
}

func (c *Cluster) merge(remote State) {
	c.mu.Lock()
	changed := false
	if remote.Clock > c.clock { c.clock = remote.Clock }
	for d, h := range remote.Hosts {
		cur, ok := c.state.Hosts[d]
		if ok {
			if h.Version < cur.Version || (h.Version == cur.Version && h.Lamport <= cur.Lamport) { continue }
		}
		h.Lamport = c.bumpClock(h.Lamport)
		c.state.Hosts[d] = h
		changed = true
		if c.cfg != nil {
			if h.Deleted { c.cfg.DeleteHost(d) } else {
				var backs []config.Backend
				if h.BackendsJSON != "" { _ = json.Unmarshal([]byte(h.BackendsJSON), &backs) }
				c.cfg.UpsertHost(config.HostConfig{Domain: h.Domain, TargetHost: h.TargetHost, TargetPort: h.TargetPort, Backends: backs, Strategy: h.Strategy})
			}
		}
	}
	if remote.Version > c.state.Version { c.state.Version = remote.Version }
	c.state.Clock = c.clock
	c.mu.Unlock()
	if changed && c.cfg != nil { if err := c.cfg.Save(); err != nil { slog.Error("cluster.persist_save_error", "err", err) } }
}

func (c *Cluster) Start(interval time.Duration) {
	go c.receiveLoop()
	go func() {
		for {
			c.gossipOnce()
			time.Sleep(interval)
		}
	}()
}
