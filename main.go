package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	URL          *url.URL
	Alive        bool
	mux          sync.RWMutex
	ReverseProxy *httputil.ReverseProxy
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

type ServerPool struct {
	backends []*Backend
	current  uint64
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

func (s *ServerPool) MarkBackendStatus(backendUrl *url.URL, alive bool) {
	for _, b := range s.backends {
		if b.URL.String() == backendUrl.String() {
			b.SetAlive(alive)
			break
		}
	}
}

func (s *ServerPool) HealthCheck() {
	for _, b := range s.backends {
		go func(backend *Backend) {
			client := &http.Client{
				Timeout: 5 * time.Second,
			}

			res, err := client.Get(backend.URL.String())
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
}

func (s *ServerPool) loadBalance(w http.ResponseWriter, r *http.Request) {
	peer := s.GetNextPeer()
	if peer != nil {
		log.Printf("Routing request to: %s", peer.URL.Host)
		peer.ReverseProxy.ServeHTTP(w, r)
		return
	}
	http.Error(w, "Service not available", http.StatusServiceUnavailable)
}

func (s *ServerPool) healthCheckRunner(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
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

func NewBackend(serverURL string) (*Backend, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}

	return &Backend{
		URL:          u,
		Alive:        true,
		ReverseProxy: httputil.NewSingleHostReverseProxy(u),
	}, nil
}

func main() {
	backends := []string{
		"http://localhost:8081",
		"http://localhost:8082",
		"http://localhost:8083",
	}

	serverPool := &ServerPool{}

	for _, backend := range backends {
		be, err := NewBackend(backend)
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

	fmt.Println("Starting proxy server on :8080")
	fmt.Printf("Configured %d backends\n", len(serverPool.backends))

	log.Fatal(http.ListenAndServe(":8080", nil))
}
