package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
    ConnectionsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name:"abantu_connections_total", Help:"Nombre de connexions acceptées"},
        []string{"domain"},
    )
    ConnectionsActive = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name:"abantu_connections_active", Help:"Connexions actives proxy->backend"},
        []string{"domain", "backend"},
    )
    BackendFailures = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name:"abantu_backend_failures_total", Help:"Dial failures par backend"},
        []string{"domain", "backend"},
    )
    BackendSelected = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name:"abantu_backend_selected_total", Help:"Sélections de backend"},
        []string{"domain", "backend", "strategy"},
    )
    RateLimited = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name:"abantu_ratelimited_total", Help:"Connexions bloquées par rate limiting"},
        []string{"domain"},
    )
    BackendState = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name:"abantu_backend_state", Help:"Etat backend (1=up,0=down)"},
        []string{"backend"},
    )
    BackendDialDurationSeconds = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name: "abantu_backend_dial_duration_seconds",
            Help: "Latence de dial vers le backend (succès ou échec)",
            Buckets: []float64{0.005,0.01,0.02,0.05,0.1,0.2,0.3,0.5,1,2,5},
        },
        []string{"domain","backend"},
    )
    ConnectionDurationSeconds = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name: "abantu_connection_duration_seconds",
            Help: "Durée des connexions proxy (après handshake jusqu'à fermeture)",
            Buckets: prometheus.ExponentialBuckets(5, 2, 8), // 5s .. ~10m40s
        },
        []string{"domain","backend"},
    )
    HandshakeBlocked = prometheus.NewCounter(
        prometheus.CounterOpts{Name:"abantu_handshake_blocked_total", Help:"Handshakes bloqués (anti scan)"},
    )
    GossipSigInvalid = prometheus.NewCounter(
        prometheus.CounterOpts{Name:"abantu_gossip_sig_invalid_total", Help:"Paquets gossip rejetés (signature)"},
    )
    GossipPackets = prometheus.NewCounter(
        prometheus.CounterOpts{Name:"abantu_gossip_packets_total", Help:"Paquets gossip reçus"},
    )
    StatusCacheHits = prometheus.NewCounter(
        prometheus.CounterOpts{Name:"abantu_status_cache_hits_total", Help:"Status query cache hits"},
    )
    StatusCacheMiss = prometheus.NewCounter(
        prometheus.CounterOpts{Name:"abantu_status_cache_miss_total", Help:"Status query cache misses"},
    )
)

func MustRegister(reg prometheus.Registerer) {
    reg.MustRegister(ConnectionsTotal, ConnectionsActive, BackendFailures, BackendSelected, RateLimited, BackendState, BackendDialDurationSeconds, ConnectionDurationSeconds, HandshakeBlocked, GossipSigInvalid, GossipPackets, StatusCacheHits, StatusCacheMiss)
}
