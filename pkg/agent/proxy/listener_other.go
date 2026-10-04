//go:build !linux

package proxy

import "net"

func listenerOwner(net.IP, uint32) (int, bool) { return 0, false }

func descends(int, int) bool { return false }

func listenInodes(net.IP, uint32) map[string]bool { return nil }

func listenTable() map[string]uint32 { return nil }

func ownerOf(string) (int, bool) { return 0, false }
