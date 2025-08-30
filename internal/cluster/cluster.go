package cluster

import (
	"encoding/json"
	"log"
	"net"
	"sync"
	"time"

	"github.com/arthurcorso/abantu/internal/config"
)

type State struct {
	Version int               `json:"version"`
	Hosts   map[string]HostInfo `json:"hosts"`
}

type HostInfo struct {
	Domain     string `json:"domain"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	Version    int    `json:"version"`
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
}

func New(addr string, peers []string) *Cluster {
	return &Cluster{addr: addr, peers: peers, state: State{Hosts: map[string]HostInfo{}}}
}

// AttachConfig permet au cluster d'écrire les hosts appris dans la configuration locale (persistance automatique)
func (c *Cluster) AttachConfig(cfg *config.Config) { c.cfg = cfg }

func (c *Cluster) UpdateHost(h HostInfo) {
	c.mu.Lock(); defer c.mu.Unlock()
	current, ok := c.state.Hosts[h.Domain]
	if !ok || h.Version > current.Version {
		c.state.Hosts[h.Domain] = h
		c.state.Version++
	}
}

func (c *Cluster) DeleteHost(domain string, version int) {
	c.UpdateHost(HostInfo{Domain: domain, Version: version, Deleted: true})
}

func (c *Cluster) GetHost(domain string) (HostInfo, bool) {
	c.mu.RLock(); defer c.mu.RUnlock()
	h, ok := c.state.Hosts[domain]
	return h, ok
}

func (c *Cluster) Snapshot() State {
	c.mu.RLock(); defer c.mu.RUnlock()
	cp := make(map[string]HostInfo, len(c.state.Hosts))
	for k, v := range c.state.Hosts { cp[k] = v }
	return State{Version: c.state.Version, Hosts: cp}
}

func (c *Cluster) gossipOnce() {
	data := c.Snapshot()
	b, _ := json.Marshal(data)
	for _, p := range c.peers {
		go func(peer string) {
			conn, err := net.DialTimeout("udp", peer, 500*time.Millisecond)
			if err != nil { return }
			defer conn.Close()
			_, _ = conn.Write(b)
		}(p)
	}
}

func (c *Cluster) receiveLoop() {
	addr, err := net.ResolveUDPAddr("udp", c.addr)
	if err != nil { log.Printf("cluster resolve: %v", err); return }
	conn, err := net.ListenUDP("udp", addr)
	if err != nil { log.Printf("cluster listen: %v", err); return }
	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil { continue }
		var st State
		if err := json.Unmarshal(buf[:n], &st); err != nil { continue }
		c.merge(st)
	}
}

func (c *Cluster) merge(remote State) {
	c.mu.Lock()
	changed := false
	for d, h := range remote.Hosts {
		cur, ok := c.state.Hosts[d]
		if !ok || h.Version > cur.Version {
			c.state.Hosts[d] = h
			changed = true
			if c.cfg != nil {
				if h.Deleted {
					c.cfg.DeleteHost(d)
				} else {
					var backs []config.Backend
					if h.BackendsJSON != "" { _ = json.Unmarshal([]byte(h.BackendsJSON), &backs) }
					c.cfg.UpsertHost(config.HostConfig{Domain: h.Domain, TargetHost: h.TargetHost, TargetPort: h.TargetPort, Backends: backs, Strategy: h.Strategy})
				}
			}
		}
	}
	if remote.Version > c.state.Version { c.state.Version = remote.Version }
	c.mu.Unlock()
	if changed && c.cfg != nil {
		if err := c.cfg.Save(); err != nil { log.Printf("cluster persist save error: %v", err) }
	}
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
