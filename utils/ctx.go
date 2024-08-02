// Package utils provides utility functions for the Keploy application.
package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
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

func CheckForUpdate(ctx context.Context) {
	currentVersion := "v" + Version
	logger := zap.NewExample()

	releaseInfo, err := GetLatestGitHubRelease(ctx, logger)
	latestVersion := releaseInfo.TagName

	if err != nil {
		fmt.Printf("failed to fetch latest GitHub release version: %v\n", err)
	}

	if currentVersion != latestVersion {
		fmt.Println("New version of Keploy is available:")
		fmt.Printf(currentVersion + " ----> " + latestVersion + "\n")
		fmt.Println("Run `keploy update` to update")

		var input string
		fmt.Print("Do you want to update? (y/n): ")
		fmt.Scanln(&input)

		if input == "y" {
			// Here add command to run `keploy update`
			
			fmt.Println("Keploy updated successfully!")
		} else {
			fmt.Println("Update canceled.")

			// Here add save the prefrence in .keploy file alongside with installation id
		}
	}
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
		LogError(logger, err, "failed stopping keploy")
		return err
	}

	if reason == "" {
		err := errors.New("cannot stop keploy without a reason")
		LogError(logger, err, "failed stopping keploy")
		return err
	}

	logger.Info("stopping Keploy", zap.String("reason", reason))
	ExecCancel()
	return nil
}

func ExecCancel() {
	cancel()
}

func SetCancel(c context.CancelFunc) {
	cancel = c
}
