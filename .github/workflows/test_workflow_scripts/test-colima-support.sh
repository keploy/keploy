#!/bin/bash
set -e
mkdir -p test-app

# Write main.go
mkdir -p test-app
cat > test-app/main.go <<'EOF'
package main

import (
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "sync"
)

var (
    store = make(map[string]string)
    mu    sync.RWMutex
)

type Item struct {
    Key   string `json:"key"`
    Value string `json:"value"`
}

func main() {
    http.HandleFunc("/items", handleItems)
    http.HandleFunc("/health", handleHealth)
    
    fmt.Println("Server starting on :8080")
    log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
}

func handleItems(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    
    switch r.Method {
    case http.MethodPost:
        var item Item
        if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
            http.Error(w, err.Error(), http.StatusBadRequest)
            return
        }
        
        mu.Lock()
        store[item.Key] = item.Value
        mu.Unlock()
        
        w.WriteHeader(http.StatusCreated)
        json.NewEncoder(w).Encode(map[string]string{"status": "created", "key": item.Key})
        
    case http.MethodGet:
        mu.RLock()
        defer mu.RUnlock()
        json.NewEncoder(w).Encode(store)
        
    default:
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
    }
}
EOF



# Write Dockerfile
cat > test-app/Dockerfile <<'EOF'
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY main.go .
RUN go build -o server main.go

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/server .
EXPOSE 8080
CMD ["./server"]
EOF