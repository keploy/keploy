package utils

import (
	"context"
	"errors"
	"fmt"
	"go.uber.org/zap"
	"os"
	"os/signal"
	"syscall"
)

var cancel context.CancelFunc

func NewCtx() context.Context {
	// Create a context that can be canceled
	ctx, cancel := context.WithCancel(context.Background())

	SetCancel(cancel)
	// Set up a channel to listen for signals
	sigs := make(chan os.Signal, 1)
	// os.Interrupt is more portable than syscall.SIGINT
	// there is no equivalent for syscall.SIGTERM in os.Signal
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	// Start a goroutine that will cancel the context when a signal is received
	go func() {
		<-sigs
		fmt.Println("Signal received, canceling context...")
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
		return errors.New("logger is not set")
	}
	if cancel == nil {
		err := errors.New("cancel function is not set")
		logger.Error("failed stopping keploy", zap.Error(err))
		return err
	}

	if reason == "" {
		err := errors.New("cannot stop keploy without a reason")
		logger.Error("failed stopping keploy", zap.Error(err))
		return err
	}

	logger.Info("stopping Keploy", zap.String("reason", reason))
	cancel()
	return nil
}

func SetCancel(c context.CancelFunc) {
	cancel = c
}
