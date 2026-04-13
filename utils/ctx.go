// Package utils provides utility functions for the Keploy application.
package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
)

var cancel context.CancelFunc

func NewCtx(logger ...*zap.Logger) context.Context {
	// Create a context that can be canceled
	ctx, cancel := context.WithCancel(context.Background())

	SetCancel(cancel)
	// Set up a channel to listen for signals
	sigs := make(chan os.Signal, 1)
	// os.Interrupt is more portable than syscall.SIGINT
	// there is no equivalent for syscall.SIGTERM in os.Signal
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)

	// Start a goroutine that will cancel the context when a signal is received
	go func() {
		sig := <-sigs // this received signal will be inside keploy docker container if running in docker else on the host.
		if len(logger) > 0 && logger[0] != nil {
			logger[0].Info("Signal received, canceling context...", zap.String("signal", sig.String()))
		} else {
			fmt.Printf("Signal received: %s, canceling context...\n", sig)
		}
		cancel()
	}()

	return ctx
}

// Stop requires a reason to stop the server.
// this is to ensure that the server is not stopped accidentally.
// and to trace back the stopper
func Stop(logger *zap.Logger, reason string) error {
	// Stop the server.
	if logger == nil {
		return errors.New("Stop called with nil logger: cannot log shutdown reason, ensure logger is initialized before calling Stop")
	}
	if cancel == nil {
		err := fmt.Errorf("Stop called but cancel function is nil (reason: %s): NewCtx may not have been called, or cancel was already consumed", reason)
		LogError(logger, err, "failed stopping keploy")
		return err
	}

	if reason == "" {
		err := errors.New("Stop called with empty reason: a shutdown reason is required for traceability")
		LogError(logger, err, "failed stopping keploy")
		return err
	}

	logger.Info("stopping Keploy", zap.String("reason", reason))
	ExecCancel()
	return nil
}

func ExecCancel() {
	if cancel != nil {
		cancel()
	}
}

// ExecCancelWithTimeout cancels the context associated with the global cancel function.
// It currently only triggers cancellation and does not wait for shutdown completion.
// The timeout parameter is accepted for forward compatibility but is not used to enforce a deadline.
func ExecCancelWithTimeout(timeout time.Duration) error {
	if cancel == nil {
		return errors.New("cancel function is not set, cannot execute cancellation")
	}
	// timeout is intentionally unused; kept for API compatibility.
	_ = timeout
	cancel()
	return nil
}

func SetCancel(c context.CancelFunc) {
	cancel = c
}
