package config

import (
	"encoding/json"
	"os"
	"time"
	"strings"
	"sync"
)

type HostConfig struct {
	Domain      string   `json:"domain"`
	TargetHost  string   `json:"target_host"`   // rétrocompatibilité (single backend)
	TargetPort  int      `json:"target_port"`   // rétrocompatibilité
	Backends    []Backend `json:"backends,omitempty"`
	Strategy    string    `json:"strategy,omitempty"` // round_robin, random, least_conn, weighted_random
	BackendDownMOTD string `json:"backend_down_motd,omitempty"`
	PingCacheTTL string `json:"ping_cache_ttl,omitempty"` // ex: "5s"
	Whitelist   []string `json:"whitelist"`
	Blacklist   []string `json:"blacklist"`
	RateLimit   RateLimitConfig `json:"rate_limit"`
}

type Backend struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Weight int    `json:"weight,omitempty"`
}

type RateLimitConfig struct {
	ConnectionsPerMinute int `json:"connections_per_minute"`
	Burst                int `json:"burst"`
}

type ClusterConfig struct {
	NodeID         string   `json:"node_id"`
	BindAddr       string   `json:"bind_addr"`
	Peers          []string `json:"peers"`
	GossipInterval string   `json:"gossip_interval"`
	Seeds          []string `json:"seeds,omitempty"`
	Discovery      bool     `json:"discovery,omitempty"`
	Secret         string   `json:"secret,omitempty"` // secret partagé HMAC gossip
}

// APIAuthConfig définit tokens -> rôle (admin|read)
type APIAuthConfig struct {
	Tokens map[string]string `json:"tokens"`
	MTLS   struct {
		Enabled bool   `json:"enabled"`
		CAFile  string `json:"ca_file"`
	} `json:"mtls"`
}

// HandshakeSecurity limite les handshakes par IP (anti scan)
type HandshakeSecurity struct {
	PerSecond int `json:"per_second"`
	Burst     int `json:"burst"`
}

type ProxyTLSConfig struct {
	Enabled  bool   `json:"enabled"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type Config struct {
	ListenHost    string        `json:"listen_host"`
	ListenPort    int           `json:"listen_port"`
	Hosts         []HostConfig  `json:"hosts"`
	Cluster       ClusterConfig `json:"cluster"`
	RedisAddr     string        `json:"redis_addr"`
	RedisPassword string        `json:"redis_password"`
	UnknownHostMOTD string      `json:"unknown_host_motd"`
	BackendDownMOTD string      `json:"backend_down_motd"`
	ServerName      string      `json:"server_name"`
	ProtocolVersion int         `json:"protocol_version"`
	APIAuth        APIAuthConfig        `json:"api_auth,omitempty"`
	HandshakeSec   HandshakeSecurity    `json:"handshake_security,omitempty"`
	ProxyProtocol  bool                 `json:"proxy_protocol_enabled,omitempty"`
	ProxyTLS       ProxyTLSConfig       `json:"proxy_tls,omitempty"`
	Bedrock        BedrockConfig        `json:"bedrock,omitempty"`
	mu sync.RWMutex `json:"-"`
	FilePath string `json:"-"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil { return nil, err }
	var c Config
	if err := json.Unmarshal(b, &c); err != nil { return nil, err }
	if c.Cluster.GossipInterval == "" { c.Cluster.GossipInterval = "3s" }
	if c.UnknownHostMOTD == "" { c.UnknownHostMOTD = "§cAucun backend n'est configuré pour ce domaine: {domain}" }
	if c.BackendDownMOTD == "" { c.BackendDownMOTD = "§cServeur indisponible - Réessayez" }
	if c.ServerName == "" { c.ServerName = "Abantu" }
	if c.ProtocolVersion == 0 { c.ProtocolVersion = 760 }
	if c.HandshakeSec.PerSecond == 0 { c.HandshakeSec.PerSecond = 20 }
	if c.HandshakeSec.Burst == 0 { c.HandshakeSec.Burst = 40 }
	// Bedrock defaults
	if c.Bedrock.Enabled {
		if c.Bedrock.ListenHost == "" { c.Bedrock.ListenHost = "0.0.0.0" }
		if c.Bedrock.ListenPort == 0 { c.Bedrock.ListenPort = 19132 }
	}
	for i := range c.Hosts { if c.Hosts[i].PingCacheTTL == "" { c.Hosts[i].PingCacheTTL = "5s" } }
	c.FilePath = path
	return &c, nil
}

func (c *Config) FindHost(domain string) *HostConfig {
	c.mu.RLock(); defer c.mu.RUnlock()
	var wildcard *HostConfig
	for i, h := range c.Hosts {
		if h.Domain == "*" { wildcard = &c.Hosts[i]; continue }
		if strings.EqualFold(h.Domain, domain) { return &c.Hosts[i] }
	}
	return wildcard
}

// UpsertHost ajoute ou remplace un host de façon thread-safe
func (c *Config) UpsertHost(h HostConfig) {
	c.mu.Lock(); defer c.mu.Unlock()
	for i, existing := range c.Hosts {
		if strings.EqualFold(existing.Domain, h.Domain) {
			c.Hosts[i] = h
			return
		}
	}
	c.Hosts = append(c.Hosts, h)
}

func (c *Config) GossipInterval() time.Duration {
	d, _ := time.ParseDuration(c.Cluster.GossipInterval)
	if d == 0 { d = 3 * time.Second }
	return d
}

func (c *Config) Mu() *sync.RWMutex { return &c.mu }

func (c *Config) DeleteHost(domain string) {
	c.mu.Lock(); defer c.mu.Unlock()
	for i, h := range c.Hosts {
		if strings.EqualFold(h.Domain, domain) {
			c.Hosts = append(c.Hosts[:i], c.Hosts[i+1:]...)
			return
		}
	}
}

func (c *Config) Save() error {
	c.mu.RLock(); defer c.mu.RUnlock()
	if c.FilePath == "" { return nil }
	b, err := json.MarshalIndent(struct {
		ListenHost string `json:"listen_host"`
		ListenPort int    `json:"listen_port"`
		RedisAddr string  `json:"redis_addr"`
		RedisPassword string `json:"redis_password"`
		UnknownHostMOTD string `json:"unknown_host_motd"`
		BackendDownMOTD string `json:"backend_down_motd"`
		ServerName string `json:"server_name"`
		ProtocolVersion int `json:"protocol_version"`
		Cluster ClusterConfig `json:"cluster"`
		Hosts []HostConfig `json:"hosts"`
		APIAuth APIAuthConfig `json:"api_auth,omitempty"`
		HandshakeSec HandshakeSecurity `json:"handshake_security,omitempty"`
		ProxyProtocol bool `json:"proxy_protocol_enabled,omitempty"`
		ProxyTLS ProxyTLSConfig `json:"proxy_tls,omitempty"`
		Bedrock BedrockConfig `json:"bedrock,omitempty"`
	}{
		ListenHost: c.ListenHost,
		ListenPort: c.ListenPort,
		RedisAddr: c.RedisAddr,
		RedisPassword: c.RedisPassword,
		UnknownHostMOTD: c.UnknownHostMOTD,
		BackendDownMOTD: c.BackendDownMOTD,
		ServerName: c.ServerName,
		ProtocolVersion: c.ProtocolVersion,
		Cluster: c.Cluster,
		Hosts: c.Hosts,
		APIAuth: c.APIAuth,
		HandshakeSec: c.HandshakeSec,
		ProxyProtocol: c.ProxyProtocol,
		ProxyTLS: c.ProxyTLS,
		Bedrock: c.Bedrock,
	}, "", "  ")
	if err != nil { return err }
	return os.WriteFile(c.FilePath, b, 0644)
}

// Bedrock configuration (UDP)
type BedrockConfig struct {
	Enabled    bool          `json:"enabled"`
	ListenHost string        `json:"listen_host"`
	ListenPort int           `json:"listen_port"`
	Hosts      []BedrockHost `json:"hosts"`
}

type BedrockHost struct {
	Label    string    `json:"label"`
	Strategy string    `json:"strategy,omitempty"` // round_robin, random, weighted_random, least_conn (least_conn pas pertinent en UDP, ignoré)
	Backends []Backend `json:"backends"`
}
