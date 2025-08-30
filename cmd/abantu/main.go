package main

import (
	"encoding/json"
	"errors"
	"flag"
	"log"
	"os"
	"time"

	"github.com/arthurcorso/abantu/internal/api"
	"github.com/arthurcorso/abantu/internal/cluster"
	"github.com/arthurcorso/abantu/internal/config"
	"github.com/arthurcorso/abantu/internal/proxy"
	"github.com/arthurcorso/abantu/internal/rate"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	apiAddr := flag.String("api", ":8080", "api listen addr")
	flag.Parse()
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("config file %s absent, génération d'un fichier par défaut", *cfgPath)
			def := defaultConfig()
			if err := writeDefaultConfig(*cfgPath, def); err != nil { log.Fatalf("écriture config défaut: %v", err) }
			cfg = def
		} else {
			log.Fatalf("config: %v", err)
		}
	}

	clusterObj := cluster.New(cfg.Cluster.BindAddr, cfg.Cluster.Peers)
	clusterObj.AttachConfig(cfg)
	clusterObj.Start(cfg.GossipInterval())
	// Publie la config initiale dans le cluster
	for _, h := range cfg.Hosts {
		b, _ := json.Marshal(h.Backends)
		clusterObj.UpdateHost(cluster.HostInfo{Domain: h.Domain, TargetHost: h.TargetHost, TargetPort: h.TargetPort, Version: 1, BackendsJSON: string(b), Strategy: h.Strategy})
	}

	rlim := rate.New(cfg.RedisAddr, cfg.RedisPassword)
	pr := proxy.New(cfg, rlim, clusterObj)
	go func(){ log.Fatal(pr.Start()) }()

	apiSrv := api.New(cfg, clusterObj)
	log.Fatal(apiSrv.Start(*apiAddr))
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
