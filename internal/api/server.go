package api

import (
	"encoding/json"
	"net/http"
	"time"
	"sync/atomic"
	"io"

	"github.com/arthurcorso/abantu/internal/cluster"
	"github.com/arthurcorso/abantu/internal/config"
	"github.com/arthurcorso/abantu/internal/version"
)

type APIServer struct {
	cfg *config.Config
	cl  *cluster.Cluster
	started atomic.Bool
}

func New(cfg *config.Config, cl *cluster.Cluster) *APIServer {
	return &APIServer{cfg: cfg, cl: cl}
}

func (a *APIServer) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request){ w.Write([]byte("ok")) })
	mux.HandleFunc("/cluster/state", func(w http.ResponseWriter, r *http.Request){
		st := a.cl.Snapshot()
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("/time", func(w http.ResponseWriter, r *http.Request){ w.Write([]byte(time.Now().UTC().Format(time.RFC3339))) })
	mux.HandleFunc("/hosts", a.handleHosts)
	mux.HandleFunc("/hosts/", a.handleHost)
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request){ w.Write([]byte(version.Version)) })
	srv := &http.Server{Addr: addr, Handler: mux}
	return srv.ListenAndServe()
}

// handleHosts: GET list, POST create/update
func (a *APIServer) handleHosts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.cfg.Mu().RLock(); defer a.cfg.Mu().RUnlock()
		json.NewEncoder(w).Encode(a.cfg.Hosts)
	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		var h config.HostConfig
		if err := json.Unmarshal(body, &h); err != nil { http.Error(w, err.Error(), 400); return }
		if h.Domain == "" { http.Error(w, "domain required", 400); return }
		a.cfg.UpsertHost(h)
		// cluster propagation simple version increment timestamp-based
		ver := int(time.Now().Unix())
		backendsData, _ := json.Marshal(h.Backends)
		hostInfo := cluster.HostInfo{Domain: h.Domain, Version: ver, Strategy: h.Strategy}
		if len(h.Backends) > 0 {
			hostInfo.TargetHost = h.Backends[0].Host
			hostInfo.TargetPort = h.Backends[0].Port
			hostInfo.BackendsJSON = string(backendsData)
		} else {
			hostInfo.TargetHost = h.TargetHost
			hostInfo.TargetPort = h.TargetPort
		}
		a.cl.UpdateHost(hostInfo)
		_ = a.cfg.Save()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(h)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleHost: DELETE /hosts/{domain}
func (a *APIServer) handleHost(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Path[len("/hosts/"):]
	if domain == "" { http.Error(w, "domain required", 400); return }
	switch r.Method {
	case http.MethodDelete:
		ver := int(time.Now().Unix())
		a.cl.DeleteHost(domain, ver)
		// remove locally
		a.cfg.DeleteHost(domain)
		_ = a.cfg.Save()
		w.WriteHeader(204)
	case http.MethodGet:
		h := a.cfg.FindHost(domain)
		if h == nil { http.NotFound(w, r); return }
		json.NewEncoder(w).Encode(h)
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		var h config.HostConfig
		if err := json.Unmarshal(body, &h); err != nil { http.Error(w, err.Error(), 400); return }
		if h.Domain == "" { h.Domain = domain }
		a.cfg.UpsertHost(h)
		ver := int(time.Now().Unix())
		backendsData, _ := json.Marshal(h.Backends)
		hostInfo := cluster.HostInfo{Domain: h.Domain, Version: ver, Strategy: h.Strategy}
		if len(h.Backends) > 0 {
			hostInfo.TargetHost = h.Backends[0].Host
			hostInfo.TargetPort = h.Backends[0].Port
			hostInfo.BackendsJSON = string(backendsData)
		} else {
			hostInfo.TargetHost = h.TargetHost
			hostInfo.TargetPort = h.TargetPort
		}
		a.cl.UpdateHost(hostInfo)
		_ = a.cfg.Save()
		json.NewEncoder(w).Encode(h)
	default:
		http.Error(w, "method not allowed", 405)
	}
}
