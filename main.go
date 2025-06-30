package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

type Backend struct {
	URL          *url.URL
	ReverseProxy *httputil.ReverseProxy
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
		return s.backends[idx]
	}
	return nil
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

func NewBackend(serverURL string) (*Backend, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}

	return &Backend{
		URL:          u,
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

	http.HandleFunc("/", serverPool.loadBalance)

	fmt.Println("Starting proxy server on :8080")
	fmt.Printf("Configured %d backends\n", len(serverPool.backends))

	log.Fatal(http.ListenAndServe(":8080", nil))
}
