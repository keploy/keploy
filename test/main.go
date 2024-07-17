package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Print "Hello, World!"
	fmt.Println("Hello, World!")

	// Create a channel to receive OS signals
	sigChan := make(chan os.Signal, 1)

	// Notify the channel on `SIGINT` and `SIGTERM` signals
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)

	// Block until a signal is received
	sig := <-sigChan

	// Print the received signal (optional)
	fmt.Printf("Received signal: %s\n", sig)

	// Print "Bye" before exiting
	fmt.Println("Bye")
}