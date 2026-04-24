package proxy

import (
	"fmt"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/kpcap"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
)

func (pm *IngressProxyManager) incomingCaptureContext(protocol string, clientConn, appConn net.Conn, finalAppAddr string, appPort uint16) kpcap.PacketContext {
	ctx := kpcap.PacketContext{
		Flow:            "incoming",
		Protocol:        protocol,
		ConnID:          fmt.Sprint(util.GetNextID()),
		PeerConnID:      fmt.Sprint(util.GetNextID()),
		DestinationAddr: finalAppAddr,
		AppPort:         appPort,
	}
	if clientConn != nil {
		if clientConn.LocalAddr() != nil {
			ctx.LocalAddr = clientConn.LocalAddr().String()
		}
		if clientConn.RemoteAddr() != nil {
			ctx.RemoteAddr = clientConn.RemoteAddr().String()
			ctx.SourceAddr = clientConn.RemoteAddr().String()
		}
	}
	if appConn != nil && appConn.RemoteAddr() != nil {
		ctx.DestinationAddr = appConn.RemoteAddr().String()
	}
	return ctx
}

func (pm *IngressProxyManager) wrapIncomingClientConn(conn net.Conn, ctx kpcap.PacketContext) net.Conn {
	if pm == nil || pm.packetCapture == nil || !pm.packetCapture.Enabled() || conn == nil {
		return conn
	}
	return pm.packetCapture.WrapConn(conn, ctx, kpcap.DirectionClientToIngress, kpcap.DirectionIngressToClient)
}

func (pm *IngressProxyManager) wrapIncomingAppConn(conn net.Conn, ctx kpcap.PacketContext) net.Conn {
	if pm == nil || pm.packetCapture == nil || !pm.packetCapture.Enabled() || conn == nil {
		return conn
	}
	return pm.packetCapture.WrapConn(conn, ctx, kpcap.DirectionAppToIngress, kpcap.DirectionIngressToApp)
}

func (pm *IngressProxyManager) wrapIncomingConns(clientConn, appConn net.Conn, ctx kpcap.PacketContext) (net.Conn, net.Conn) {
	return pm.wrapIncomingClientConn(clientConn, ctx), pm.wrapIncomingAppConn(appConn, ctx)
}
