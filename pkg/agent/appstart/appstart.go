package appstart

import (
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

var (
	mu     sync.Mutex
	starts []models.AppStart
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
