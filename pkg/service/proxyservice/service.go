//go:build linux

package proxyservice

import (
	"context"

	corePkg "go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/models"
)

type DestInfoDummy struct{}

func NewDestInfoDummy() *DestInfoDummy {
	return &DestInfoDummy{}
}

func (d *DestInfoDummy) Get(ctx context.Context, srcPort uint16) (*corePkg.NetworkAddress, error) {
	return &corePkg.NetworkAddress{
		AppID:    12345,                 // fake application ID
		Version:  4,                     // version marker
		IPv4Addr: 0x7F000001,            // 127.0.0.1 in hex (little endian safe for IPv4Addr uint32)
		IPv6Addr: [4]uint32{0, 0, 0, 1}, // ::1 (IPv6 loopback)
		Port:     16790,                 // fake port
	}, nil
}

func (d *DestInfoDummy) Delete(ctx context.Context, srcPort uint16) error {
	return nil
}

type MockDB interface {
	InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error
}
