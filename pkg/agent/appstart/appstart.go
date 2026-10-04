package appstart

import (
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

var (
	mu      sync.Mutex
	starts  []models.AppStart
	workers = map[int]bool{}
)

func Note(pid uint32, port uint16) {
	mu.Lock()
	starts = append(starts, models.AppStart{At: time.Now(), PID: pid, Port: port})
	mu.Unlock()
}

func List() []models.AppStart {
	mu.Lock()
	defer mu.Unlock()
	return append([]models.AppStart(nil), starts...)
}

func Worker(pid int) {
	mu.Lock()
	workers[pid] = true
	mu.Unlock()
}

func IsWorker(pid int) bool {
	mu.Lock()
	defer mu.Unlock()
	return workers[pid]
}
