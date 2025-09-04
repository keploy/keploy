package load

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"

	"github.com/pkg/browser"
	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

type DashboardExposer struct {
	config      *config.Config
	logger      *zap.Logger
	port        int
	instanceID  string
	instanceURL string
}

func NewDashboardExposer(cfg *config.Config, logger *zap.Logger, loadTestID string) *DashboardExposer {
	return &DashboardExposer{
		config:      cfg,
		logger:      logger,
		port:        3000, // Default port for the dashboard, until it's configured otherwise // TODO
		instanceID:  loadTestID,
		instanceURL: "http://localhost:3000/?dashboard=" + loadTestID,
	}
}

func (de *DashboardExposer) Expose(ctx context.Context) {
	dashboardServer := &http.Server{
		Addr:    ":" + strconv.Itoa(de.port),
		Handler: de.handler(),
	}

	de.logger.Info("Starting dashboard server", zap.String("URL", "http://localhost:"+strconv.Itoa(de.port)))

	go func() {
		if err := dashboardServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			de.logger.Error("Dashboard server failed", zap.Error(err))
		}
	}()

	de.logger.Info("Opening browser on dashboard", zap.String("URL", de.instanceURL))
	de.openBrowser()

	go func() {
		<-ctx.Done()
		de.logger.Info("Shutting down dashboard exposer...")
		if err := dashboardServer.Shutdown(context.Background()); err != nil {
			de.logger.Error("Failed to shutdown dashboard server", zap.Error(err))
		}
	}()
}

// =========================================================================================================

func (de *DashboardExposer) openBrowser() {
	if isWSL() {
		err := exec.Command("cmd.exe", "/c", "start", de.instanceURL).Run()
		if err != nil {
			de.logger.Error("Failed to open browser in WSL", zap.Error(err))
		}
	} else {
		err := browser.OpenURL(de.instanceURL)
		if err != nil {
			de.logger.Error("Failed to open browser", zap.Error(err))
		}
	}
}

func isWSL() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	content := strings.ToLower(string(data))
	return strings.Contains(content, "microsoft") || strings.Contains(content, "wsl")
}

// =========================================================================================================

//go:embed out/*
var content embed.FS

// fileSystem returns an http.FileSystem rooted at /.
func (de *DashboardExposer) fileSystem() http.FileSystem {
	fsys, err := fs.Sub(content, "out")
	if err != nil {
		log.Fatal("embed fs setup failed:", err)
	}
	return http.FS(fsys)
}

// Handler returns a ready-to-use http.Handler that:
//   - Serves compressed assets transparently (if the browser supports it)
//   - Falls back to index.html for unknown routes (handy for client-side routers)
func (de *DashboardExposer) handler() http.Handler {
	fs := de.fileSystem()
	fileServer := http.FileServer(fs)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try the exact path first
		if _, err := fs.Open(path.Clean(r.URL.Path)); err != nil {
			// Not found? send SPA entry point
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}
