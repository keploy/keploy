//go:build !darwin

package testnet

import "net"

// connectHolder connects to peer from a port the system picks.
func connectHolder(peer string) (*net.TCPConn, error) {
	c, err := net.Dial("tcp", peer)
	if err != nil {
		return nil, err
	}
	return c.(*net.TCPConn), nil
}
