package docker

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"go.uber.org/zap"
)

// AgentContainerPrefix starts the name of every docker-run agent container;
// the rest of the name is the run's random id.
const AgentContainerPrefix = "keploy-v3-"

// runTLSVolumePrefix starts the name of every docker-run run's CA volume
// (TLSVolumeFor). A compose project's is <project>_keploy-tls-certs and an
// older keploy's the bare keploy-tls-certs: neither starts with it.
const runTLSVolumePrefix = KeployTLSVolumeName + "-"

// TLSVolumeFor names the volume a docker-run run's agent writes its CA bundle
// to (ca.crt, keploy-ca.crt, truststore.jks at KeployTLSMountPath) and its
// application container reads it from. It is the run's own: with one name
// for every run, a second run on the host overwrote the first's ca.crt with
// its own CA while the first ran, and the first's application then refused
// its own agent's certificates (curl: unable to get local issuer
// certificate), so the first run's HTTPS calls failed in a test and went
// unrecorded in a record. The id is the agent container's, so the volume
// names the run it belongs to.
//
// Compose keeps KeployTLSVolumeName: compose scopes a top-level volume by
// project, so two projects never share it.
func TLSVolumeFor(agentContainer string) string {
	return runTLSVolumePrefix + strings.TrimPrefix(agentContainer, AgentContainerPrefix)
}

// VolumeRemover is the part of the engine's API the CA volume's removal
// takes.
type VolumeRemover interface {
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
}

// RemoveTLSVolume removes a run's CA volume once its agent's container is
// gone. Never forced: the engine refuses to remove a volume a container still
// mounts (an application container a run's own `docker run` keeps, without
// --rm), and that one is left for SweepTLSVolumes.
func RemoveTLSVolume(ctx context.Context, logger *zap.Logger, api VolumeRemover, name string) {
	if err := api.VolumeRemove(ctx, name, false); err != nil {
		logger.Debug("left the run's CA volume; a later docker-mode run removes it once no container mounts it",
			zap.String("volume", name), zap.Error(err))
	}
}

// SweepTLSVolumes removes the CA volumes earlier runs left (a run that was
// killed, or whose application container still mounted it at its end). It
// removes only those no container mounts: the engine refuses an in-use
// volume, and a run's volume is in use from the moment its agent container is
// created with it until the last of its containers is removed, so a live
// run's is never taken. Never forced, for that reason.
func SweepTLSVolumes(ctx context.Context, logger *zap.Logger, api VolumeRemover) {
	args := filters.NewArgs()
	args.Add("name", runTLSVolumePrefix)
	list, err := api.VolumeList(ctx, volume.ListOptions{Filters: args})
	if err != nil {
		logger.Debug("could not list earlier runs' CA volumes to remove them", zap.Error(err))
		return
	}
	for _, v := range list.Volumes {
		// The engine's name filter matches anywhere in the name.
		if !strings.HasPrefix(v.Name, runTLSVolumePrefix) {
			continue
		}
		if err := api.VolumeRemove(ctx, v.Name, false); err != nil {
			logger.Debug("left an earlier run's CA volume", zap.String("volume", v.Name), zap.Error(err))
		}
	}
}
