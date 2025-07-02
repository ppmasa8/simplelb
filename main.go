package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Port                int      `json:"port"`
	Backends            []string `json:"backends"`
	HealthCheckPath     string   `json:"health_check_path"`
	HealthCheckInterval int      `json:"health_check_interval"`
	MaxRetries          int      `json:"max_retries"`
	Timeout             int      `json:"timeout"`
}

type Metrics struct {
	TotalRequests   uint64
	SuccessRequests uint64
	FailedRequests  uint64
	HealthChecks    uint64
}

type Backend struct {
	URL          *url.URL
	Alive        bool
	mux          sync.RWMutex
	ReverseProxy *httputil.ReverseProxy
	Requests     uint64
}

func (b *Backend) SetAlive(alive bool) {
	b.mux.Lock()
	defer b.mux.Unlock()
	b.Alive = alive
}

func (b *Backend) IsAlive() bool {
	b.mux.RLock()
	defer b.mux.RUnlock()
	return b.Alive
}

func (b *Backend) IncrementRequests() {
	atomic.AddUint64(&b.Requests, 1)
}

func (b *Backend) GetRequestCount() uint64 {
	return atomic.LoadUint64(&b.Requests)
}

type ServerPool struct {
	backends []*Backend
	current  uint64
	config   *Config
	metrics  *Metrics
}

func (s *ServerPool) GetNextPeer() *Backend {
	next := int(atomic.AddUint64(&s.current, uint64(1)) % uint64(len(s.backends)))
	l := len(s.backends) + next
	for i := next; i < l; i++ {
		idx := i % len(s.backends)
		if s.backends[idx].IsAlive() {
			if i != next {
				atomic.StoreUint64(&s.current, uint64(idx))
			}
			return s.backends[idx]
		}
	}
	return nil
}

func (s *ServerPool) loadBalance(w http.ResponseWriter, r *http.Request) {
	atomic.AddUint64(&s.metrics.TotalRequests, 1)

	attempts := GetRetryFromContext(r)
	if attempts > s.config.MaxRetries {
		atomic.AddUint64(&s.metrics.FailedRequests, 1)
		log.Printf("Max retry attempts reached, terminating")
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}

	peer := s.GetNextPeer()
	if peer != nil {
		peer.IncrementRequests()
		log.Printf("Attempt %d: Routing to: %s", attempts+1, peer.URL.Host)
		peer.ReverseProxy.ServeHTTP(w, r)
		atomic.AddUint64(&s.metrics.SuccessRequests, 1)
		return
	}

	atomic.AddUint64(&s.metrics.FailedRequests, 1)
	http.Error(w, "Service not available", http.StatusServiceUnavailable)
}

func (s *ServerPool) HealthCheck() {
	atomic.AddUint64(&s.metrics.HealthChecks, 1)

	var wg sync.WaitGroup
	for _, b := range s.backends {
		wg.Add(1)
		go func(backend *Backend) {
			defer wg.Done()

			client := &http.Client{
				Timeout: time.Duration(s.config.Timeout) * time.Second,
			}

			healthURL := backend.URL.String() + s.config.HealthCheckPath
			res, err := client.Get(healthURL)
			alive := err == nil && res.StatusCode >= 200 && res.StatusCode < 300

			if res != nil {
				res.Body.Close()
			}

			backend.SetAlive(alive)

			status := "down"
			if alive {
				status = "up"
			}
			log.Printf("Health check - %s is %s", backend.URL.Host, status)
		}(b)
	}
	wg.Wait()
}

func (s *ServerPool) healthCheckRunner(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(s.config.HealthCheckInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Println("Starting health check...")
			s.HealthCheck()
		}
	}
}

func (s *ServerPool) statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	status := struct {
		Backends []struct {
			URL      string `json:"url"`
			Alive    bool   `json:"alive"`
			Requests uint64 `json:"requests"`
		} `json:"backends"`
		Metrics *Metrics `json:"metrics"`
	}{
		Metrics: s.metrics,
	}

	for _, b := range s.backends {
		status.Backends = append(status.Backends, struct {
			URL      string `json:"url"`
			Alive    bool   `json:"alive"`
			Requests uint64 `json:"requests"`
		}{
			URL:      b.URL.String(),
			Alive:    b.IsAlive(),
			Requests: b.GetRequestCount(),
		})
	}

	json.NewEncoder(w).Encode(status)
}

func (s *ServerPool) metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.metrics)
}

func GetRetryFromContext(r *http.Request) int {
	if retry, ok := r.Context().Value("retry").(int); ok {
		return retry
	}
	return 0
}

func NewBackend(serverURL string, config *Config, serverPool *ServerPool) (*Backend, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(u)

	proxy.Transport = &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(config.Timeout) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	backend := &Backend{
		URL:          u,
		Alive:        true,
		ReverseProxy: proxy,
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		log.Printf("Proxy error for %s: %v", u.Host, e)

		backend.SetAlive(false)

		retries := GetRetryFromContext(r)
		if retries < config.MaxRetries {
			log.Printf("Retrying request, attempt $d", retries+1)
			ctx := context.WithValue(r.Context(), "retry", retries+1)
			time.Sleep(10 * time.Millisecond)
			serverPool.loadBalance(w, r.WithContext(ctx))
		} else {
			atomic.AddUint64(&serverPool.metrics.FailedRequests, 1)
			http.Error(w, "Service not available", http.StatusBadGateway)
		}
	}

	return backend, nil
}

func LoadConfig(filename string) (*Config, error) {
	config := &Config{
		Port:                8080,
		Backends:            []string{"http://localhost:8081", "http://localhost:8082", "http://localhost:8083"},
		HealthCheckPath:     "/",
		HealthCheckInterval: 10,
		MaxRetries:          3,
		Timeout:             5,
	}

	if filename == "" {
		return config, nil
	}

	file, err := os.Open(filename)
	if err != nil {
		log.Printf("Warning: Could not open config file %s, using default", filename)
		return config, nil
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(config); err != nil {
		return nil, fmt.Errorf("error parsing config file: %v", err)
	}

	return config, nil
}

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", "", "Configuration file path")
	flag.Parse()

	config, err := LoadConfig(configFile)
	if err != nil {
		log.Fatal(err)
	}

	serverPool := &ServerPool{
		config:  config,
		metrics: &Metrics{},
	}

	for _, backend := range config.Backends {
		be, err := NewBackend(backend, config, serverPool)
		if err != nil {
			log.Fatal(err)
		}
		serverPool.backends = append(serverPool.backends, be)
		fmt.Printf("Added backend: %s\n", backend)
	}

	log.Println("Starting initial health check...")
	serverPool.HealthCheck()
	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go serverPool.healthCheckRunner(ctx)

	http.HandleFunc("/", serverPool.loadBalance)

	http.HandleFunc("/status", serverPool.statusHandler)
	http.HandleFunc("/metrics", serverPool.metricsHandler)

	http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(config)
	})

	fmt.Printf("Load Balancer started on :%d\n", config.Port)
	fmt.Printf("Backends: %v\n", config.Backends)
	fmt.Printf("Health check interval: %d seconds\n", config.HealthCheckInterval)
	fmt.Printf("Max retries: %d\n", config.MaxRetries)
	fmt.Println("Management APIs:")
	fmt.Printf("  Status:  http://localhost:%d/status\n", config.Port)
	fmt.Printf("  Metrics: http://localhost:%d/metrics\n", config.Port)
	fmt.Printf("  Config:  http://localhost:%d/config\n", config.Port)

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.Port), nil))
}
