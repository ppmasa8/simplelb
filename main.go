package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

func main() {
	backendURL := "http://localhost:8081"

	target, err := url.Parse(backendURL)
	if err != nil {
		log.Fatal(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		log.Printf("Proxy error: %v", e)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Proxying request to %s", target.Host)
		proxy.ServeHTTP(w, r)
	})

	fmt.Println("Starting proxy server on :8080")
	fmt.Printf("Proxying to: %s\n", backendURL)

	log.Fatal(http.ListenAndServe(":8080", nil))
}
