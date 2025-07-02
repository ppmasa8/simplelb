# How to use
```bash
// Boot backend server
python3 -m http.server 8081 &
python3 -m http.server 8082 &
python3 -m http.server 8083 &

// Boot simplelb
go run main.go


// Check status
curl http://localhost:8080/status / jq

// Check metrics
curl http://localhost:8080/metrics / jq

// Check config
curl http://locahost:8080/config / jq

// Load Testing
seq 1 10 | xargs -I {} curl http://localhost:8080
```