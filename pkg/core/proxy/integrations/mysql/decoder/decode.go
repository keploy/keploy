//go:build linux

// Package decoder provides the decoding functions for the MySQL integration.
package decoder

import (
	"context"
	"io"

	// "io"
	"net"
	// "time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/utils"

	intgUtil "go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"

	// "go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func Decode(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	errCh := make(chan error, 1)

	mocks, err := mockDb.GetUnFilteredMocks()
	if err != nil {
		utils.LogError(logger, err, "failed to get unfiltered mocks")
		return err
	}

	// Get the mysql mocks
	configMocks := intgUtil.GetMockByKind(mocks, "MySQL")
	if err != nil {
		utils.LogError(logger, err, "failed to get mysql mocks")
		return err
	}

	if len(configMocks) == 0 {
		utils.LogError(logger, nil, "no mysql mocks found")
		return nil
	}

	go func(errCh chan error, configMocks []*models.Mock, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) {
		defer pUtil.Recover(logger, clientConn, nil)
		defer close(errCh)

		// err := simulateInitialHandshake(ctx, logger, clientConn, configMocks, mockDb)
		// if err != nil {
		// 	utils.LogError(logger, err, "failed to simulate initial handshake")
		// 	errCh <- err
		// 	return
		// }

	}(errCh, configMocks, dstCfg, mockDb, opts)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

// func getFirstSQLMock(configMocks []*models.Mock) (*models.Mock, int, bool) {
// 	for index, mock := range configMocks {
// 		if len(mock.Spec.MySQLResponses) > 0 && mock.Kind == "MySQL" && mock.Spec.MySQLResponses[0].Header.PacketType == "MySQLHandshakeV10" {
// 			return mock, index, true
// 		}
// 	}
// return nil, 0, false
// }
