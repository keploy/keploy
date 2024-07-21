package main

import (
	"fmt"
	"os/exec"
	"strings"
)

func main() {
	cmd := exec.Command("go", "list", "-m", "-u", "all")
	output, err := cmd.Output()
	if err != nil {
		fmt.Println("Error running go list:", err)
		return
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "deprecated") {
			fmt.Println("Deprecated dependency found:", line)
		}
	}
}
