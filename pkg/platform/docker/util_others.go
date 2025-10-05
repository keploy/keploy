//go:build !windows

package docker

import (
	"context"
	"os/exec"
	"syscall"
)

// // ExtractInodeByPid extracts the inode of the PID namespace of a given PID
// func ExtractInodeByPid(pid int) (string, error) {
// 	// Check the namespace file in /proc
// 	nsPath := fmt.Sprintf("/proc/%d/ns/pid", pid)
// 	fileInfo, err := os.Stat(nsPath)
// 	if err != nil {
// 		return "", err
// 	}

// 	// Retrieve inode number
// 	inode := fileInfo.Sys().(*syscall.Stat_t).Ino
// 	return fmt.Sprintf("%d", inode), nil
// }

func PrepareDockerCommand(ctx context.Context, keployAlias string) *exec.Cmd {
	// Use sh -c for Unix-like systems
	cmd := exec.CommandContext(
		ctx,
		"sh",
		"-c",
		keployAlias,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	return cmd
}
