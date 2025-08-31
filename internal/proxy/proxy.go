package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
	"github.com/arthurcorso/abantu/internal/cluster"
	"github.com/arthurcorso/abantu/internal/config"
	"github.com/arthurcorso/abantu/internal/metrics"
	"github.com/arthurcorso/abantu/internal/rate"
	"log/slog"
)

type pingEntry struct { json []byte; expire time.Time }

type Proxy struct {
	cfg *config.Config
	lim *rate.Limiter
	cluster *cluster.Cluster
	mu sync.RWMutex
	rrIndex map[string]int
	down map[string]time.Time
	active map[string]int
	fails map[string]int
	pingCache map[string]pingEntry
	hsMu sync.Mutex
	hsCounters map[string]*tokenBucket
}

type tokenBucket struct { tokens float64; last time.Time }

func New(cfg *config.Config, lim *rate.Limiter, cl *cluster.Cluster) *Proxy {
	p := &Proxy{cfg: cfg, lim: lim, cluster: cl, rrIndex: map[string]int{}, down: map[string]time.Time{}, active: map[string]int{}, fails: map[string]int{}, pingCache: map[string]pingEntry{}, hsCounters: map[string]*tokenBucket{}}
	go p.healthLoop()
	return p
}

func (p *Proxy) Start() error {
	addr := net.JoinHostPort(p.cfg.ListenHost, itoa(p.cfg.ListenPort))
	var ln net.Listener
	var err error
	if p.cfg.ProxyTLS.Enabled {
		cert, errLoad := tls.LoadX509KeyPair(p.cfg.ProxyTLS.CertFile, p.cfg.ProxyTLS.KeyFile)
		if errLoad != nil { return errLoad }
		ln, err = tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}})
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil { return err }
	slog.Info("proxy.listen", "addr", addr, "tls", p.cfg.ProxyTLS.Enabled)
	for { c, err := ln.Accept(); if err != nil { continue }; go p.handleConn(c) }
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

func (p *Proxy) allowHandshake(ip string) bool {
	limit := p.cfg.HandshakeSec.PerSecond; burst := p.cfg.HandshakeSec.Burst
	if limit <= 0 { return true }
	p.hsMu.Lock(); defer p.hsMu.Unlock()
	b := p.hsCounters[ip]
	now := time.Now()
	if b == nil { b = &tokenBucket{tokens: float64(burst), last: now}; p.hsCounters[ip] = b }
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * float64(limit)
	if b.tokens > float64(burst) { b.tokens = float64(burst) }
	b.last = now
	if b.tokens < 1 { metrics.HandshakeBlocked.Inc(); return false }
	b.tokens -= 1
	return true
}

func (p *Proxy) handleConn(client net.Conn) {
	defer client.Close()
	client.SetDeadline(time.Now().Add(7 * time.Second))
	br := bufio.NewReader(client)
	remoteIP := client.RemoteAddr().(*net.TCPAddr).IP.String()
	if p.cfg.ProxyProtocol {
		peek, _ := br.Peek(5)
		if string(peek) == "PROXY" {
			line, _ := br.ReadString('\n')
			parts := strings.Split(strings.TrimSpace(line), " ")
			if len(parts) >= 6 { remoteIP = parts[2] }
		}
	}
	if !p.allowHandshake(remoteIP) { slog.Debug("handshake.rate_block", "ip", remoteIP); return }
	hs, err := readHandshake(br)
	if err != nil { slog.Debug("conn.handshake_error", "remote", client.RemoteAddr().String(), "err", err); return }
	domain := hs.ServerAddress; if domain == "" { domain = "default" }
	slog.Debug("conn.handshake", "remote", client.RemoteAddr().String(), "domain", domain, "next_state", hs.NextState, "proto", hs.ProtocolVersion)
	var statusRequested bool
	if hs.NextState == 1 { // status flow
		pktLen, err := readVarInt(br); if err != nil { return }
		pkt := make([]byte, pktLen); if _, err := io.ReadFull(br, pkt); err != nil { return }
		statusRequested = true
	}
	h := p.cfg.FindHost(domain)
	if h == nil && p.cluster != nil {
		if hi, ok := p.cluster.GetHost(domain); ok && !hi.Deleted {
			var backs []config.Backend
			if hi.BackendsJSON != "" { _ = json.Unmarshal([]byte(hi.BackendsJSON), &backs) }
			h = &config.HostConfig{Domain: hi.Domain, TargetHost: hi.TargetHost, TargetPort: hi.TargetPort, Backends: backs, Strategy: hi.Strategy}
		}
	}
	if h == nil { if statusRequested { motd := strings.ReplaceAll(p.cfg.UnknownHostMOTD, "{domain}", domain); p.sendSimpleStatus(client, motd, 0, 0); p.handlePossiblePing(br, client) }; slog.Info("conn.no_host", "remote", client.RemoteAddr().String(), "domain", domain); return }
	ip := client.RemoteAddr().(*net.TCPAddr).IP.String(); if remoteIP != ip { ip = remoteIP }
	if isListed(ip, h.Blacklist) { return }
	if len(h.Whitelist) > 0 && !isListed(ip, h.Whitelist) { return }
	allowed, err := p.lim.Allow(domain+":"+ip, h.RateLimit.ConnectionsPerMinute, h.RateLimit.Burst)
	if err != nil { slog.Warn("rate.error", "domain", domain, "ip", ip, "err", err) }
	if !allowed { metrics.RateLimited.WithLabelValues(domain).Inc(); slog.Info("conn.rate_limited", "domain", domain, "ip", ip); return }
	if statusRequested { p.sendBackendStatus(client, domain, h); p.handlePossiblePing(br, client); return }
	backendHost, backendPort, backendAddr, backend, err := p.dialWithFallback(h, domain)
	if err != nil { motdTemplate := h.BackendDownMOTD; if motdTemplate == "" { motdTemplate = p.cfg.BackendDownMOTD }; motd := strings.ReplaceAll(motdTemplate, "{domain}", domain); p.sendStatus(client, motd); return }
	p.markBackendSuccess(backendHost, backendPort)
	metrics.BackendState.WithLabelValues(backendAddr).Set(1)
	metrics.ConnectionsTotal.WithLabelValues(domain).Inc()
	p.incActive(backendHost, backendPort)
	slog.Info("conn.proxy", "remote", client.RemoteAddr().String(), "domain", domain, "backend", backendAddr)
	defer backend.Close()
	client.SetDeadline(time.Time{})
	connStart := time.Now()
	backend.Write(hs.Raw)
	// Si activé, on précède le flux backend par une ligne PROXY v1 contenant l'IP réelle du client.
	if p.cfg.ForwardProxyProtocol {
		clientAddr := client.RemoteAddr().(*net.TCPAddr)
		backendAddrTCP := backend.RemoteAddr().(*net.TCPAddr)
		family := "TCP4"
		if clientAddr.IP.To4() == nil { family = "TCP6" }
		// Format: PROXY TCP4 198.51.100.10 203.0.113.5 12345 25565\r\n
		line := fmt.Sprintf("PROXY %s %s %s %d %d\r\n", family, clientAddr.IP.String(), backendAddrTCP.IP.String(), clientAddr.Port, backendAddrTCP.Port)
		backend.Write([]byte(line))
	}
	var wg sync.WaitGroup; wg.Add(2)
	metrics.ConnectionsActive.WithLabelValues(domain, backendAddr).Inc()
	go func(){ defer wg.Done(); io.Copy(backend, br) }()
	go func(){ defer wg.Done(); io.Copy(client, backend) }()
	wg.Wait()
	metrics.ConnectionsActive.WithLabelValues(domain, backendAddr).Dec()
	metrics.ConnectionDurationSeconds.WithLabelValues(domain, backendAddr).Observe(time.Since(connStart).Seconds())
	p.decActive(backendHost, backendPort)
}

type handshake struct { ProtocolVersion int; ServerAddress string; ServerPort uint16; NextState int; Raw []byte }

func readHandshake(br *bufio.Reader) (*handshake, error) {
	length, err := readVarInt(br); if err != nil { return nil, err }
	payload := make([]byte, length); if _, err := io.ReadFull(br, payload); err != nil { return nil, err }
	buf := bytes.NewBuffer(payload)
	id, _ := buf.ReadByte(); if id != 0x00 { return nil, errors.New("not handshake") }
	proto, err := readVarInt(buf); if err != nil { return nil, err }
	addrLen, err := readVarInt(buf); if err != nil { return nil, err }
	addr := make([]byte, addrLen); if _, err := io.ReadFull(buf, addr); err != nil { return nil, err }
	var port uint16; if err := binary.Read(buf, binary.BigEndian, &port); err != nil { return nil, err }
	next, err := readVarInt(buf); if err != nil { return nil, err }
	return &handshake{ProtocolVersion: proto, ServerAddress: string(addr), ServerPort: port, NextState: next, Raw: encodePacket(length, payload)}, nil
}

func readVarInt(r io.Reader) (int, error) {
	var num, shift int
	for {
		var b [1]byte; if _, err := r.Read(b[:]); err != nil { return 0, err }
		val := int(b[0] & 0x7F); num |= val << shift
		if (b[0] & 0x80) == 0 { break }
		shift += 7; if shift > 35 { return 0, errors.New("varint too big") }
	}
	return num, nil
}

func writeVarInt(buf *bytes.Buffer, value int) { for { b := byte(value & 0x7F); value >>= 7; if value != 0 { b |= 0x80; buf.WriteByte(b); continue }; buf.WriteByte(b); break } }

func encodePacket(length int, payload []byte) []byte { var out bytes.Buffer; writeVarInt(&out, length); out.Write(payload); return out.Bytes() }

func (p *Proxy) handlePossiblePing(br *bufio.Reader, conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	length, err := readVarInt(br); if err != nil { return }
	data := make([]byte, length); if _, err := io.ReadFull(br, data); err != nil { return }
	if len(data) == 9 && data[0] == 0x01 { conn.Write(encodePacket(length, data)) }
}

func isListed(item string, list []string) bool { for _, v := range list { if v == item { return true } }; return false }

func (p *Proxy) selectBackend(h *config.HostConfig, domain string) (string, int) {
	if len(h.Backends) == 0 { return h.TargetHost, h.TargetPort }
	now := time.Now()
	candidates := make([]config.Backend,0,len(h.Backends))
	for _, b := range h.Backends { if !p.isBackendDown(b.Host, b.Port, now) { candidates = append(candidates, b) } }
	if len(candidates)==0 { candidates = h.Backends }
	strategy := h.Strategy; if strategy=="" { strategy = "round_robin" }
	switch strategy {
	case "random": b := candidates[rand.Intn(len(candidates))]; return b.Host, b.Port
	case "weighted_random":
		total := 0; for _, b := range candidates { w:=b.Weight; if w<=0 { w=1 }; total+=w }
		r := rand.Intn(total); acc:=0; for _, b := range candidates { w:=b.Weight; if w<=0 { w=1 }; acc+=w; if r < acc { return b.Host, b.Port } }
		b := candidates[len(candidates)-1]; return b.Host, b.Port
	case "least_conn":
		p.mu.RLock(); bestIdx:=0; bestActive:=int(^uint(0)>>1); for i,b := range candidates { act:=p.active[backendKey(b.Host,b.Port)]; if act < bestActive { bestActive=act; bestIdx=i } }; chosen:=candidates[bestIdx]; p.mu.RUnlock(); return chosen.Host, chosen.Port
	default: // round_robin
		p.mu.Lock(); idx:=p.rrIndex[domain]; b:=candidates[idx%len(candidates)]; p.rrIndex[domain]=(idx+1)%len(candidates); p.mu.Unlock(); return b.Host, b.Port
	}
}

const (
	backendCooldownBase = 10 * time.Second
	backendCooldownMax  = 5 * time.Minute
	healthInterval      = 10 * time.Second
	healthDialTimeout   = 1500 * time.Millisecond
)

func backendKey(host string, port int) string { return host+":"+itoa(port) }

func (p *Proxy) markBackendFailure(host string, port int) {
	key := backendKey(host, port)
	p.mu.Lock(); defer p.mu.Unlock()
	f := p.fails[key]+1; p.fails[key]=f
	backoff := backendCooldownBase << (f-1); if backoff > backendCooldownMax { backoff = backendCooldownMax }
	p.down[key] = time.Now().Add(backoff)
}

func (p *Proxy) markBackendSuccess(host string, port int) { key:=backendKey(host,port); p.mu.Lock(); delete(p.down,key); delete(p.fails,key); p.mu.Unlock() }

func (p *Proxy) isBackendDown(host string, port int, now time.Time) bool { p.mu.RLock(); until, ok := p.down[backendKey(host,port)]; p.mu.RUnlock(); if !ok { return false }; if now.After(until) { p.mu.Lock(); delete(p.down, backendKey(host,port)); p.mu.Unlock(); return false }; return true }

func (p *Proxy) incActive(host string, port int) { p.mu.Lock(); p.active[backendKey(host,port)]++; p.mu.Unlock() }
func (p *Proxy) decActive(host string, port int) { p.mu.Lock(); if v:=p.active[backendKey(host,port)]; v>1 { p.active[backendKey(host,port)]=v-1 } else { delete(p.active, backendKey(host,port)) }; p.mu.Unlock() }

func (p *Proxy) healthLoop() { for { time.Sleep(healthInterval); p.cfg.Mu().RLock(); hosts := append([]config.HostConfig(nil), p.cfg.Hosts...); p.cfg.Mu().RUnlock(); for _, h := range hosts { for _, b := range h.Backends { go p.healthProbe(b.Host, b.Port) } } } }

func (p *Proxy) healthProbe(host string, port int) { if !p.isBackendDown(host,port,time.Now()) { return }; addr:=net.JoinHostPort(host,itoa(port)); c,err:=net.DialTimeout("tcp",addr,healthDialTimeout); if err!=nil { return }; c.Close(); p.markBackendSuccess(host,port) }

func (p *Proxy) sendStatus(conn net.Conn, motd string) { status := map[string]any{"version":map[string]any{"name":p.cfg.ServerName,"protocol":p.cfg.ProtocolVersion},"players":map[string]any{"max":0,"online":0},"description":map[string]string{"text":motd}}; b,_:=json.Marshal(status); var payload bytes.Buffer; payload.WriteByte(0x00); writeVarInt(&payload,len(b)); payload.Write(b); var frame bytes.Buffer; writeVarInt(&frame,payload.Len()); frame.Write(payload.Bytes()); conn.Write(frame.Bytes()) }

func (p *Proxy) sendSimpleStatus(conn net.Conn, motd string, online,max int) { status := map[string]any{"version":map[string]any{"name":p.cfg.ServerName,"protocol":p.cfg.ProtocolVersion},"players":map[string]any{"max":max,"online":online},"description":map[string]string{"text":motd}}; b,_:=json.Marshal(status); var payload bytes.Buffer; payload.WriteByte(0x00); writeVarInt(&payload,len(b)); payload.Write(b); var frame bytes.Buffer; writeVarInt(&frame,payload.Len()); frame.Write(payload.Bytes()); conn.Write(frame.Bytes()) }

func (p *Proxy) sendBackendStatus(conn net.Conn, domain string, h *config.HostConfig) {
	if data, ok := p.getCachedPing(domain); ok { metrics.StatusCacheHits.Inc(); conn.Write(data); return }
	metrics.StatusCacheMiss.Inc()
	backs := h.Backends; if len(backs)==0 { backs = []config.Backend{{Host: h.TargetHost, Port: h.TargetPort}} }
	var frame []byte
	for _, b := range backs { raw, err := fetchBackendStatusJSON(b.Host, b.Port, p.cfg.ProtocolVersion); if err != nil { continue }; payload := buildStatusPayload(raw); frame = wrapPacket(payload); break }
	if frame == nil { motd := strings.ReplaceAll(p.cfg.BackendDownMOTD, "{domain}", domain); p.sendSimpleStatus(conn, motd, 0,0); return }
	cacheTTL := parseTTL(h.PingCacheTTL); if cacheTTL>0 { p.storeCachedPing(domain, frame, cacheTTL) }
	conn.Write(frame)
}

func parseTTL(s string) time.Duration { d,_:=time.ParseDuration(s); return d }

func fetchBackendStatusJSON(host string, port int, protocol int) ([]byte, error) {
	addr := net.JoinHostPort(host, itoa(port)); conn, err := net.DialTimeout("tcp", addr, 1200*time.Millisecond); if err != nil { return nil, err }; defer conn.Close()
	var hs bytes.Buffer; hs.WriteByte(0x00); writeVarInt(&hs, protocol); writeVarInt(&hs, len(host)); hs.WriteString(host); hs.Write([]byte{byte(port>>8), byte(port)}); writeVarInt(&hs,1)
	conn.SetDeadline(time.Now().Add(1500*time.Millisecond)); conn.Write(packWithLength(hs.Bytes())); conn.Write([]byte{0x01,0x00})
	respLen, err := readVarInt(conn); if err != nil { return nil, err }
	resp := make([]byte, respLen); if _, err := io.ReadFull(conn, resp); err != nil { return nil, err }
	if len(resp)<1 || resp[0]!=0x00 { return nil, fmt.Errorf("unexpected packet id") }
	r2 := bytes.NewReader(resp[1:]); jsonLen, err := readVarInt(r2); if err != nil { return nil, err }
	of := 1 + sizeOfVarInt(jsonLen); if of+jsonLen > len(resp) { return nil, fmt.Errorf("json length overflow") }
	raw := resp[of:of+jsonLen]; return raw, nil
}

func buildStatusPayload(raw []byte) []byte { var payload bytes.Buffer; payload.WriteByte(0x00); writeVarInt(&payload, len(raw)); payload.Write(raw); return payload.Bytes() }
func wrapPacket(payload []byte) []byte { var frame bytes.Buffer; writeVarInt(&frame, len(payload)); frame.Write(payload); return frame.Bytes() }
func packWithLength(p []byte) []byte { var b bytes.Buffer; writeVarInt(&b, len(p)); b.Write(p); return b.Bytes() }
func sizeOfVarInt(v int) int { c:=0; for { c++; v >>=7; if v==0 { break } }; return c }

func (p *Proxy) getCachedPing(domain string) ([]byte,bool) { p.mu.RLock(); e,ok:=p.pingCache[domain]; p.mu.RUnlock(); if !ok || time.Now().After(e.expire) { return nil,false }; return e.json,true }
func (p *Proxy) storeCachedPing(domain string, data []byte, ttl time.Duration) { p.mu.Lock(); p.pingCache[domain] = pingEntry{json:data, expire: time.Now().Add(ttl)}; p.mu.Unlock() }

func (p *Proxy) dialWithFallback(h *config.HostConfig, domain string) (host string, port int, addr string, c net.Conn, err error) {
	tried := map[string]struct{}{}; max := 1; if len(h.Backends)>0 { max = len(h.Backends) }
	for attempt := 0; attempt < max; attempt++ {
		host, port = p.selectBackend(h, domain); addr = net.JoinHostPort(host, itoa(port)); if _,exists := tried[addr]; exists { continue }; tried[addr]=struct{}{}
		metrics.BackendSelected.WithLabelValues(domain, addr, h.Strategy).Inc()
		dialStart := time.Now(); c, err = net.DialTimeout("tcp", addr, 5*time.Second); metrics.BackendDialDurationSeconds.WithLabelValues(domain, addr).Observe(time.Since(dialStart).Seconds())
		if err == nil { return }
		p.markBackendFailure(host, port); metrics.BackendFailures.WithLabelValues(domain, addr).Inc(); metrics.BackendState.WithLabelValues(addr).Set(0); slog.Warn("backend.dial_failed", "domain", domain, "backend", addr, "attempt", attempt+1, "err", err)
	}
	return "",0,"",nil,fmt.Errorf("all backends failed for domain %s", domain)
}
