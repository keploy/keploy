//go:build linux

package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/proxyservice"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Registered["packet-replay"] = Proxy
}

func Proxy(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var proxyCmd = &cobra.Command{
		Use:     "packet-replay",
		Short:   "Replay the recorded network packets",
		Example: "keploy packet-replay --pcap-path ./traffic.pcap",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "packet-replay")
			if err != nil {
				utils.LogError(logger, err, "failed to get packet-replay service")
				return err
			}
			packetReplaySvc, ok := svc.(*proxyservice.ProxyService)
			if !ok {
				utils.LogError(logger, nil, "failed to typecast packet-replay service")
				return err
			}
			packetReplaySvc.StartProxy(ctx)
			return nil
		},
	}
	err := cmdConfigurator.AddFlags(proxyCmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add flags to proxy command")
		return nil
	}
	return proxyCmd
}
