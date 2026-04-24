package proxy

import (
	"fmt"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/kpcap"
	"go.keploy.io/server/v3/pkg/models"
)

func (p *Proxy) outgoingCaptureContext(mode models.Mode, clientConnID, destConnID int64, srcConn net.Conn, dstAddr string, sourcePort int, destinationPort uint32) kpcap.PacketContext {
	ctx := kpcap.PacketContext{
		Flow:            "outgoing",
		Mode:            string(mode),
		Protocol:        "tcp",
		ConnID:          fmt.Sprint(clientConnID),
		PeerConnID:      fmt.Sprint(destConnID),
		SourcePort:      uint32(sourcePort),
		DestinationPort: destinationPort,
		DestinationAddr: dstAddr,
	}
	if srcConn != nil {
		if srcConn.LocalAddr() != nil {
			ctx.LocalAddr = srcConn.LocalAddr().String()
		}
		if srcConn.RemoteAddr() != nil {
			ctx.RemoteAddr = srcConn.RemoteAddr().String()
			ctx.SourceAddr = srcConn.RemoteAddr().String()
		}
	}
	return ctx
}

func (p *Proxy) wrapOutgoingClientConn(conn net.Conn, ctx kpcap.PacketContext) net.Conn {
	if p == nil || p.packetCapture == nil || !p.packetCapture.Enabled() || conn == nil {
		return conn
	}
	return p.packetCapture.WrapConn(conn, ctx, kpcap.DirectionAppToProxy, kpcap.DirectionProxyToApp)
}

func (p *Proxy) wrapOutgoingDestConn(conn net.Conn, ctx kpcap.PacketContext) net.Conn {
	if p == nil || p.packetCapture == nil || !p.packetCapture.Enabled() || conn == nil {
		return conn
	}
	return p.packetCapture.WrapConn(conn, ctx, kpcap.DirectionUpstreamToProxy, kpcap.DirectionProxyToUpstream)
}

func (p *Proxy) wrapOutgoingConns(srcConn, dstConn net.Conn, ctx kpcap.PacketContext) (net.Conn, net.Conn) {
	return p.wrapOutgoingClientConn(srcConn, ctx), p.wrapOutgoingDestConn(dstConn, ctx)
}
