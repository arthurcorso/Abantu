package proxy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/arthurcorso/abantu/internal/cluster"
	"github.com/arthurcorso/abantu/internal/config"
	"github.com/arthurcorso/abantu/internal/rate"
)

type Proxy struct {
	cfg     *config.Config
	lim     *rate.Limiter
	cluster *cluster.Cluster
	mu      sync.RWMutex
	rrIndex map[string]int // round robin index par domaine
}

func New(cfg *config.Config, lim *rate.Limiter, cl *cluster.Cluster) *Proxy {
	return &Proxy{cfg: cfg, lim: lim, cluster: cl, rrIndex: make(map[string]int)}
}

func (p *Proxy) Start() error {
	addr := net.JoinHostPort(p.cfg.ListenHost,  itoa(p.cfg.ListenPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil { return err }
	log.Printf("Abantu proxy listening on %s", addr)
	for {
	c, err := ln.Accept()
	if err != nil { continue }
	go p.handleConn(c)
	}
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

func (p *Proxy) handleConn(client net.Conn) {
	defer client.Close()
	client.SetDeadline(time.Now().Add(7 * time.Second))
	br := bufio.NewReader(client)
	hs, err := readHandshake(br)
	if err != nil { log.Printf("conn %s handshake read error: %v", client.RemoteAddr(), err); return }
	domain := hs.ServerAddress
	if domain == "" { domain = "default" }
	log.Printf("conn %s handshake domain=%s nextState=%d proto=%d", client.RemoteAddr(), domain, hs.NextState, hs.ProtocolVersion)
	var statusReq []byte
	if hs.NextState == 1 { // status sequence, on attend la request 0x00 ensuite
		pktLen, err := readVarInt(br)
		if err != nil { return }
		pkt := make([]byte, pktLen)
		if _, err := io.ReadFull(br, pkt); err != nil { return }
		statusReq = encodePacket(pktLen, pkt)
	}
	h := p.cfg.FindHost(domain)
	if h == nil && p.cluster != nil {
		if hi, ok := p.cluster.GetHost(domain); ok {
			if !hi.Deleted {
				var backends []config.Backend
				if hi.BackendsJSON != "" { _ = json.Unmarshal([]byte(hi.BackendsJSON), &backends) }
				h = &config.HostConfig{Domain: hi.Domain, TargetHost: hi.TargetHost, TargetPort: hi.TargetPort, Backends: backends, Strategy: hi.Strategy}
			}
		}
	}
	if h == nil {
		if hs.NextState == 1 {
			motd := strings.ReplaceAll(p.cfg.UnknownHostMOTD, "{domain}", domain)
			p.sendStatus(client, motd)
			p.handlePossiblePing(br, client)
		}
		log.Printf("conn %s no host config for domain=%s", client.RemoteAddr(), domain)
		return
	}
	ip := client.RemoteAddr().(*net.TCPAddr).IP.String()
	if isListed(ip, h.Blacklist) { return }
	if len(h.Whitelist) > 0 && !isListed(ip, h.Whitelist) { return }
	allowed, err := p.lim.Allow(domain+":"+ip, h.RateLimit.ConnectionsPerMinute, h.RateLimit.Burst)
	if err != nil { log.Printf("conn %s rate limiter error: %v (allow=%v)", client.RemoteAddr(), err, allowed) }
	if !allowed { log.Printf("conn %s rate limited domain=%s ip=%s", client.RemoteAddr(), domain, ip); return }
	backendHost, backendPort := p.selectBackend(h, domain)
	backendAddr := net.JoinHostPort(backendHost, itoa(backendPort))
	backend, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		if hs.NextState == 1 {
			motdTemplate := h.BackendDownMOTD
			if motdTemplate == "" { motdTemplate = p.cfg.BackendDownMOTD }
			motd := strings.ReplaceAll(motdTemplate, "{domain}", domain)
			p.sendStatus(client, motd)
			p.handlePossiblePing(br, client)
		}
		log.Printf("conn %s backend dial failed domain=%s backend=%s err=%v", client.RemoteAddr(), domain, backendAddr, err)
		return
	}
	log.Printf("conn %s proxying domain=%s -> %s", client.RemoteAddr(), domain, backendAddr)
	defer backend.Close()
	client.SetDeadline(time.Time{})
	backend.Write(hs.Raw)
	if len(statusReq) > 0 { backend.Write(statusReq) }
	var wg sync.WaitGroup
	wg.Add(2)
	go func(){ defer wg.Done(); io.Copy(backend, br) }()
	go func(){ defer wg.Done(); io.Copy(client, backend) }()
	wg.Wait()
}

// --- Handshake parsing et utilitaires ---

type handshake struct {
	ProtocolVersion int
	ServerAddress   string
	ServerPort      uint16
	NextState       int
	Raw             []byte
}

func readHandshake(br *bufio.Reader) (*handshake, error) {
	length, err := readVarInt(br)
	if err != nil { return nil, err }
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil { return nil, err }
	buf := bytes.NewBuffer(payload)
	id, _ := buf.ReadByte()
	if id != 0x00 { return nil, errors.New("not handshake") }
	proto, err := readVarInt(buf)
	if err != nil { return nil, err }
	addrLen, err := readVarInt(buf)
	if err != nil { return nil, err }
	addr := make([]byte, addrLen)
	if _, err := io.ReadFull(buf, addr); err != nil { return nil, err }
	var port uint16
	if err := binary.Read(buf, binary.BigEndian, &port); err != nil { return nil, err }
	next, err := readVarInt(buf)
	if err != nil { return nil, err }
	return &handshake{ProtocolVersion: proto, ServerAddress: string(addr), ServerPort: port, NextState: next, Raw: encodePacket(length, payload)}, nil
}

func readVarInt(r io.Reader) (int, error) {
	var num, shift int
	for {
		var b [1]byte
		if _, err := r.Read(b[:]); err != nil { return 0, err }
		val := int(b[0] & 0x7F)
		num |= val << shift
		if (b[0] & 0x80) == 0 { break }
		shift += 7
		if shift > 35 { return 0, errors.New("varint too big") }
	}
	return num, nil
}

func writeVarInt(buf *bytes.Buffer, value int) {
	for {
		b := byte(value & 0x7F)
		value >>= 7
		if value != 0 { b |= 0x80; buf.WriteByte(b); continue }
		buf.WriteByte(b); break
	}
}

func encodePacket(length int, payload []byte) []byte {
	var out bytes.Buffer
	writeVarInt(&out, length)
	out.Write(payload)
	return out.Bytes()
}

func (p *Proxy) handlePossiblePing(br *bufio.Reader, conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	length, err := readVarInt(br)
	if err != nil { return }
	data := make([]byte, length)
	if _, err := io.ReadFull(br, data); err != nil { return }
	if len(data) == 9 && data[0] == 0x01 { // ping packet id 0x01 + 8 bytes payload
		conn.Write(encodePacket(length, data))
	}
}

// isDomainChar obsolète (gardé si besoin futur)
// (ancienne fonction isDomainChar retirée)

func isListed(item string, list []string) bool {
	for _, v := range list { if v == item { return true } }
	return false
}

// selectBackend choisit un backend pour un host config.
// Priorité: si Backends vide -> utiliser TargetHost/TargetPort (legacy)
// Stratégies supportées: round_robin (défaut), random, weighted_random (poids), least_conn (placeholder - random fallback)
func (p *Proxy) selectBackend(h *config.HostConfig, domain string) (string, int) {
	if len(h.Backends) == 0 {
		return h.TargetHost, h.TargetPort
	}
	strategy := h.Strategy
	if strategy == "" { strategy = "round_robin" }
	switch strategy {
	case "random":
		b := h.Backends[rand.Intn(len(h.Backends))]
		return b.Host, b.Port
	case "weighted_random":
		total := 0
		for _, b := range h.Backends { w := b.Weight; if w<=0 { w=1 }; total += w }
		r := rand.Intn(total)
		acc := 0
		for _, b := range h.Backends { w := b.Weight; if w<=0 { w=1 }; acc += w; if r < acc { return b.Host, b.Port } }
		b := h.Backends[len(h.Backends)-1]; return b.Host, b.Port
	case "least_conn":
		// Placeholder: nécessite comptage connexions par backend
		b := h.Backends[rand.Intn(len(h.Backends))]
		return b.Host, b.Port
	case "round_robin":
		fallthrough
	default:
		p.mu.Lock()
		idx := p.rrIndex[domain]
		b := h.Backends[idx % len(h.Backends)]
		p.rrIndex[domain] = (idx + 1) % len(h.Backends)
		p.mu.Unlock()
		return b.Host, b.Port
	}
}

// sendStatus renvoie un paquet de status (ping list) simplifié du protocole Minecraft.
// Format: longueur(varint) + ID paquet (0x00) + JSON status (varint longueur + bytes)
func (p *Proxy) sendStatus(conn net.Conn, motd string) {
	// Construction JSON minimal
	status := map[string]any{
		"version": map[string]any{"name": p.cfg.ServerName, "protocol": p.cfg.ProtocolVersion},
		"players": map[string]any{"max": 0, "online": 0},
		"description": map[string]string{"text": motd},
	}
	b, _ := json.Marshal(status)
	var payload bytes.Buffer
	// Packet ID 0x00
	payload.WriteByte(0x00)
	writeVarInt(&payload, len(b))
	payload.Write(b)
	var frame bytes.Buffer
	writeVarInt(&frame, payload.Len())
	frame.Write(payload.Bytes())
	conn.Write(frame.Bytes())
}

// writeVarInt encode un entier selon VarInt Minecraft.
// writeVarInt redéfinie plus haut (version unique)
