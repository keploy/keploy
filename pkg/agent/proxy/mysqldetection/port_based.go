package mysqldetection

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// PortBasedDetection implements MySQL detection based on destination port
type PortBasedDetection struct {
	logger *zap.Logger
	ports  []uint32 // List of MySQL ports to check
}

// NewPortBasedDetection creates a new port-based detection strategy
func NewPortBasedDetection(logger *zap.Logger, ports []uint32) *PortBasedDetection {
	// Default to port 3306 if no ports specified
	if len(ports) == 0 {
		ports = []uint32{3306}
	}
	return &PortBasedDetection{
		logger: logger,
		ports:  ports,
	}
}

// ShouldHandle checks if the destination port matches any MySQL port
func (p *PortBasedDetection) ShouldHandle(_ context.Context, destInfo *agent.NetworkAddress, _ []byte) bool {
	for _, port := range p.ports {
		if destInfo.Port == port {
			return true
		}
	}
	return false
}

// HandleConnection handles MySQL connection using port-based detection
func (p *PortBasedDetection) HandleConnection(
	ctx context.Context,
	parserCtx context.Context,
	srcConn net.Conn,
	dstAddr string,
	destInfo *agent.NetworkAddress,
	rule *agent.Session,
	outgoingOpts models.OutgoingOptions,
	mysqlIntegration integrations.Integrations,
	mockManager interface{},
	logger *zap.Logger,
	sendError func(error),
) error {
	var dstConn net.Conn
	var err error

	if rule.Mode != models.MODE_TEST {
		dstConn, err = net.Dial("tcp", dstAddr)
		if err != nil {
			utils.LogError(logger, err, "failed to dial the conn to destination server", 
				zap.String("strategy", "port-based"),
				zap.String("server address", dstAddr))
			return err
		}

		dstCfg := &models.ConditionalDstCfg{
			Port: uint(destInfo.Port),
		}
		outgoingOpts.DstCfg = dstCfg

		// Record the outgoing message into a mock
		err := mysqlIntegration.RecordOutgoing(parserCtx, srcConn, dstConn, rule.MC, outgoingOpts)
		if err != nil {
			utils.LogError(logger, err, "failed to record the outgoing message")
			return err
		}
		return nil
	}

	// Mock mode - get mock manager
	m, ok := mockManager.(integrations.MockMemDb)
	if !ok {
		utils.LogError(logger, nil, "failed to fetch the mock manager")
		return errors.New("invalid mock manager type")
	}

	// Mock the outgoing message
	err = mysqlIntegration.MockOutgoing(parserCtx, srcConn, &models.ConditionalDstCfg{Addr: dstAddr}, m, outgoingOpts)
	if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !isNetworkClosedErr(err) {
		utils.LogError(logger, err, "failed to mock the outgoing message")
		proxyErr := models.ParserError{
			ParserErrorType: models.ErrMockNotFound,
			Err:             err,
		}
		if sendError != nil {
			sendError(proxyErr)
		}
		return err
	}
	return nil
}

// isNetworkClosedErr checks if the error is due to a closed network connection.
// This includes broken pipe, connection reset by peer, and use of closed network connection errors.
func isNetworkClosedErr(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "use of closed network connection") ||
		// Windows-specific error patterns
		strings.Contains(errStr, "wsarecv") ||
		strings.Contains(errStr, "wsasend") ||
		strings.Contains(errStr, "forcibly closed by the remote host")
}
