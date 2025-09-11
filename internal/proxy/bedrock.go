package proxy

import (
    "log/slog"
    "math/rand"
    "net"
    "sync/atomic"
    "time"

    "github.com/arthurcorso/abantu/internal/config"
)

// Very small UDP forwarder for Bedrock: selects a backend per source and forwards packets both ways.
// Note: This is a simplistic approach; Bedrock has RakNet with connection state. For production, a stateful relay is preferred.

type bedrockProxy struct {
    cfg   *config.Config
    stop  atomic.Bool
}

func (p *Proxy) startBedrock() error {
    if p.cfg.Bedrock.Enabled == false { return nil }
    laddr := net.UDPAddr{IP: net.ParseIP(p.cfg.Bedrock.ListenHost), Port: p.cfg.Bedrock.ListenPort}
    conn, err := net.ListenUDP("udp", &laddr)
    if err != nil { return err }
    slog.Info("bedrock.listen", "addr", laddr.String())
    go p.bedrockLoop(conn)
    return nil
}

func (p *Proxy) bedrockLoop(conn *net.UDPConn) {
    defer conn.Close()
    // Map source -> selected backend addr
    backendMap := make(map[string]*net.UDPAddr)
    buf := make([]byte, 1500)
    for !p.stopping.Load() {
        conn.SetReadDeadline(time.Now().Add(2 * time.Second))
        n, src, err := conn.ReadFromUDP(buf)
        if err != nil {
            nerr, ok := err.(net.Error)
            if ok && nerr.Timeout() { continue }
            if p.stopping.Load() { return }
            slog.Debug("bedrock.read_error", "err", err)
            continue
        }
        baddr := backendMap[src.String()]
        if baddr == nil {
            baddr = p.selectBedrockBackend()
            if baddr == nil { continue }
            backendMap[src.String()] = baddr
        }
        // forward to backend
        _, err = conn.WriteToUDP(buf[:n], baddr)
        if err != nil {
            slog.Debug("bedrock.forward_error", "err", err)
            continue
        }
        // try to read immediate response (best-effort)
        conn.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
        for {
            n2, from, err2 := conn.ReadFromUDP(buf)
            if err2 != nil { break }
            // only accept from selected backend
            if from.IP.Equal(baddr.IP) && from.Port == baddr.Port {
                // send back to client
                conn.WriteToUDP(buf[:n2], src)
            }
        }
    }
}

func (p *Proxy) selectBedrockBackend() *net.UDPAddr {
    // Simple selection: first bedrock host list aggregated
    var candidates []config.Backend
    for _, h := range p.cfg.Bedrock.Hosts {
        candidates = append(candidates, h.Backends...)
    }
    if len(candidates) == 0 { return nil }
    b := candidates[rand.Intn(len(candidates))]
    return &net.UDPAddr{IP: net.ParseIP(b.Host), Port: b.Port}
}
