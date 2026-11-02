//go:build !darwin

package testnet

import "net"

// connectHolder connects to peer from a port the system picks, with
// holdControl set on the socket.
func connectHolder(peer string) (*net.TCPConn, error) {
	d := net.Dialer{Control: holdControl}
	c, err := d.Dial("tcp", peer)
	if err != nil {
		return nil, err
	}
	return c.(*net.TCPConn), nil
}
