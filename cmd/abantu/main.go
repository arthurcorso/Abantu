package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/arthurcorso/abantu/internal/api"
	"github.com/arthurcorso/abantu/internal/cluster"
	"github.com/arthurcorso/abantu/internal/config"
	"github.com/arthurcorso/abantu/internal/proxy"
	"github.com/arthurcorso/abantu/internal/rate"
	"github.com/arthurcorso/abantu/internal/version"
	"github.com/arthurcorso/abantu/internal/metrics"
	"github.com/arthurcorso/abantu/internal/logging"
	"github.com/prometheus/client_golang/prometheus"
    "log/slog"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	apiAddr := flag.String("api", ":8080", "api listen addr")
	logFormat := flag.String("log.format", "text", "log format (text|json)")
	logLevel := flag.String("log.level", "info", "log level (debug|info|warn|error)")
	logFile := flag.String("log.file", "", "fichier de log (rotation simple par redémarrage)")
	logSync := flag.Bool("log.sync", false, "forcer fsync après chaque ligne (impact perf)")
	flag.Parse()
	// Valeur par défaut implicite si non fournie
	if *logFile == "" {
		defaultPath := "logs/abantu.log"
		logFile = &defaultPath
	}
	logging.Init(*logFormat, *logLevel, *logFile, *logSync)
	slog.Info("abantu.start", "version", version.Version)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("config.missing", "path", *cfgPath, "action", "generate_default")
			def := defaultConfig()
			if err := writeDefaultConfig(*cfgPath, def); err != nil { slog.Error("config.default_write_error", "err", err); os.Exit(1) }
			cfg = def
		} else {
			slog.Error("config.load_error", "err", err)
			os.Exit(1)
		}
	}

	clusterObj := cluster.New(cfg.Cluster.BindAddr, cfg.Cluster.Peers)
	clusterObj.AttachConfig(cfg)
	clusterObj.Start(cfg.GossipInterval())
	metrics.MustRegister(prometheus.DefaultRegisterer)
	// Publie la config initiale dans le cluster
	for _, h := range cfg.Hosts {
		b, _ := json.Marshal(h.Backends)
		clusterObj.UpdateHost(cluster.HostInfo{Domain: h.Domain, TargetHost: h.TargetHost, TargetPort: h.TargetPort, Version: 1, BackendsJSON: string(b), Strategy: h.Strategy})
	}

	rlim := rate.New(cfg.RedisAddr, cfg.RedisPassword)
	pr := proxy.New(cfg, rlim, clusterObj)
	go func(){ if err := pr.Start(); err != nil { slog.Error("proxy.start_error", "err", err); os.Exit(1) } }()

	apiSrv := api.New(cfg, clusterObj)
	slog.Info("api.start", "addr", *apiAddr)
	if err := apiSrv.Start(*apiAddr); err != nil { slog.Error("api.error", "err", err); os.Exit(1) }
}

func defaultConfig() *config.Config {
	return &config.Config{
		ListenHost: "0.0.0.0",
		ListenPort: 25565,
		RedisAddr: "127.0.0.1:6379",
		RedisPassword: "",
		UnknownHostMOTD: "§cAucun backend n'est configuré pour ce domaine: {domain}",
		BackendDownMOTD: "§cBackend indisponible pour {domain}",
		ServerName: "Abantu Proxy",
		ProtocolVersion: 760,
		Cluster: config.ClusterConfig{NodeID: "node-" + time.Now().Format("150405"), BindAddr: "127.0.0.1:14000", Peers: []string{}, GossipInterval: "3s"},
	Bedrock: config.BedrockConfig{Enabled: false, ListenHost: "0.0.0.0", ListenPort: 19132, Hosts: []config.BedrockHost{{Label: "default", Strategy: "random", Backends: []config.Backend{{Host: "127.0.0.1", Port: 19133}}}}},
		Hosts: []config.HostConfig{
			{
				Domain: "localhost",
				Strategy: "round_robin",
				Backends: []config.Backend{{Host: "127.0.0.1", Port: 25566}, {Host: "127.0.0.1", Port: 25567}},
				RateLimit: config.RateLimitConfig{ConnectionsPerMinute: 60, Burst: 20},
				BackendDownMOTD: "§cServeur local indisponible pour {domain}",
			},
		},
	}
}

func writeDefaultConfig(path string, cfg *config.Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil { return err }
	return os.WriteFile(path, b, 0644)
}
