// Package routes defines the routes for the agent service.
package routes

import (
	"fmt"
	"net/http"

	"go.uber.org/zap"
)

func StartAgentServer(logger *zap.Logger, port int, router http.Handler) {
	logger.Info("Starting Agent's HTTP server on :", zap.Int("port", port))
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), router); err != nil {
		logger.Error("failed to start HTTP server", zap.Error(err))
		return
	}
	logger.Info("HTTP server started successfully on port ", zap.Int("port", port))

}
