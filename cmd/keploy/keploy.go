package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
)

var dockerComposeFilePath string

func init() {
	flag.StringVar(&dockerComposeFilePath, "docker-compose-file-path", ".", "Path to the Docker Compose file")
}

func main() {
	flag.Parse()

 cmd := exec.Command("docker-compose", "-f", dockerComposeFilePath, "up")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		fmt.Println("Error running Docker Compose:", err)
		os.Exit(1)
	}
}

