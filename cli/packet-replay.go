//go:build linux

package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Registered["packet-replay"] = PacketReplay
}

func PacketReplay(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var packetReplayCmd = &cobra.Command{
		Use:     "packet-replay",
		Short:   "Replay the recorded network packets",
		Example: "keploy packet-replay --pcap-path ./traffic.pcap",
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "record")
			if err != nil {
				utils.LogError(logger, err, "failed to get packet-replay service")
				return err
			}
			recordSvc, ok := svc.(record.Service)
			if !ok {
				utils.LogError(logger, nil, "failed to typecast packet-replay service")
				return err
			}

			err = recordSvc.StartNetworkPacketReplay(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to start network packet replay")
				return err
			}

			return nil
		},
	}
	err := cmdConfigurator.AddFlags(packetReplayCmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add flags to packet-replay command")
		return nil
	}
	return packetReplayCmd
}
