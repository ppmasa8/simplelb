# About
Simple Load Balancer Built from Scratch

# Completed Features
## Core Load Balancing

- HTTP reverse proxy
- Round-robin algorithm
- Multiple backend management

## Health & Reliability

- Active health checking
- Automatic failover
- Request retry mechanism

## Operations & Monitoring

- JSON configuration
- Real-time metrics
- Management APIs
- Status monitoring

## Performance

- Concurrent processing
- Connection pooling
- Thread-safe operations

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