//go:build linux

// Package mysql provides integration with MySQL outgoing.
package mysql

import "fmt"

func decodeAuthNextFactor(data []byte) error {
	if data[0] == 0x02 {
		fmt.Println("detected auth next factor packet")
	}
	return fmt.Errorf("multi factor authentication is not supported")
}
