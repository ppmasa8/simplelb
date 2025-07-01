package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MaxRetries   = 3
	RetryKeyName = "retry"
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

func GetRetryFromContext(r *http.Request) int {
	if retry, ok := r.Context().Value(RetryKeyName).(int); ok {
		return retry
	}
	return 0
}

func (s *ServerPool) loadBalance(w http.ResponseWriter, r *http.Request) {
	attempts := GetRetryFromContext(r)
	if attempts > MaxRetries {
		log.Printf("Max retry attempts reached, terminating")
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}

	peer := s.GetNextPeer()
	if peer != nil {
		log.Printf("Routing request to: %s", peer.URL.Host)
		peer.ReverseProxy.ServeHTTP(w, r)
		return
	}
	http.Error(w, "Service not available", http.StatusServiceUnavailable)
}

func (s *ServerPool) HealthCheck() {
	var wg sync.WaitGroup

	for _, b := range s.backends {
		wg.Add(1)
		go func(backend *Backend) {
			defer wg.Done()

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
	wg.Wait()
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

	proxy := httputil.NewSingleHostReverseProxy(u)

	proxy.Transport = &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
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
		log.Printf("Retrying request, attempt %d", retries+1)

		ctx := context.WithValue(r.Context(), RetryKeyName, retries+1)

		time.Sleep(10 * time.Millisecond)

		serverPool.loadBalance(w, r.WithContext(ctx))
	}

	return backend, nil
}

var serverPool *ServerPool

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

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"backends": [`)
		for i, b := range serverPool.backends {
			if i > 0 {
				fmt.Fprintf(w, `,`)
			}
			fmt.Fprintf(w, `{"url":"%s","alive":%t}`, b.URL.String(), b.IsAlive())
		}
		fmt.Fprintf(w, `]}`)
	})

	fmt.Println("Load Balancer started on :8080")
	fmt.Printf("Health check interval: 10 seconds\n")
	fmt.Printf("Max retries: %d\n", MaxRetries)
	fmt.Println("Status endpoint: http://localhost:8080/status")

	log.Fatal(http.ListenAndServe(":8080", nil))
}
