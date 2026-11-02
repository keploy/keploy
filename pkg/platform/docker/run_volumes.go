package docker

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"go.uber.org/zap"
)

// AgentContainerPrefix starts the name of every docker-run agent container;
// the rest of the name is the run's random id.
const AgentContainerPrefix = "keploy-v3-"

// RunVolume is a named volume of which each docker-run run has its own,
// RunVolumeFor(Base, the run's agent container), mounted at Target in the
// run's agent container and in its application container. With one name for
// every run, two runs on one host shared what it holds while both ran.
// Keploy's own is the CA volume (TLSVolumeFor), which keploy mounts itself;
// an embedder names its own in DockerConfig.RunVolumes.
type RunVolume struct {
	Base   string
	Target string
}

// RunVolumeFor names the run's own volume of base: base, a dash, and the run's
// id, the agent container's name less AgentContainerPrefix. A compose
// project's is <project>_<base> and an older keploy's the bare base: neither
// is one.
func RunVolumeFor(base, agentContainer string) string {
	return base + "-" + strings.TrimPrefix(agentContainer, AgentContainerPrefix)
}

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
	return RunVolumeFor(KeployTLSVolumeName, agentContainer)
}

// runVolumeBases names the bases of every volume a run has its own of: the
// CA volume's and each embedder's.
func runVolumeBases() []string {
	bases := []string{KeployTLSVolumeName}
	for _, v := range DockerConfig.RunVolumes {
		bases = append(bases, v.Base)
	}
	return bases
}

// isRunVolumeOf reports whether name is some run's volume of base: base, a
// dash, and a run's id (agentUtils.GenerateRandomContainerName: 16 lowercase
// hex digits). A volume of the user's that only starts with the base is not.
func isRunVolumeOf(name, base string) bool {
	id, ok := strings.CutPrefix(name, base+"-")
	if !ok || len(id) != 16 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// VolumeRemover is the part of the engine's API the CA volume's removal
// takes.
type VolumeRemover interface {
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
}

// RemoveRunVolumes removes a run's own volumes once its agent's container is
// gone. Never forced: the engine refuses to remove a volume a container still
// mounts (an application container a run's own `docker run` keeps, without
// --rm), and that one is left for SweepRunVolumes.
func RemoveRunVolumes(ctx context.Context, logger *zap.Logger, api VolumeRemover, agentContainer string) {
	for _, base := range runVolumeBases() {
		name := RunVolumeFor(base, agentContainer)
		if err := api.VolumeRemove(ctx, name, false); err != nil {
			logger.Debug("left the run's volume; a later docker-mode run removes it once no container mounts it",
				zap.String("volume", name), zap.Error(err))
		}
	}
}

// SweepRunVolumes removes the volumes earlier runs left (a run that was
// killed, or whose application container still mounted one at its end). It
// removes only those no container mounts: the engine refuses an in-use
// volume, and a run's volume is in use from the moment its agent container is
// created with it until the last of its containers is removed, so a live
// run's is never taken. Never forced, for that reason.
func SweepRunVolumes(ctx context.Context, logger *zap.Logger, api VolumeRemover) {
	for _, base := range runVolumeBases() {
		args := filters.NewArgs()
		args.Add("name", base+"-")
		list, err := api.VolumeList(ctx, volume.ListOptions{Filters: args})
		if err != nil {
			logger.Debug("could not list earlier runs' volumes to remove them", zap.String("volume", base), zap.Error(err))
			continue
		}
		for _, v := range list.Volumes {
			// The engine's name filter matches anywhere in the name.
			if !isRunVolumeOf(v.Name, base) {
				continue
			}
			if err := api.VolumeRemove(ctx, v.Name, false); err != nil {
				logger.Debug("left an earlier run's volume", zap.String("volume", v.Name), zap.Error(err))
			}
		}
	}
}

// RunVolumesForApp readies an application's `docker run` command for the run
// of agentContainer's embedder volumes (DockerConfig.RunVolumes). A command
// written for a keploy that gave every run one volume mounts its bare Base at
// its Target; that mount is renamed to the run's own, and its Base is in
// renamed for a WARN. Every other gets a -v flag in flags, for the caller to
// add beside its own.
func RunVolumesForApp(cmd, agentContainer string) (out, flags string, renamed []string) {
	out = cmd
	for _, v := range DockerConfig.RunVolumes {
		own := RunVolumeFor(v.Base, agentContainer)
		var did bool
		out, did = renameVolumeMount(out, v, own)
		if did {
			renamed = append(renamed, v.Base)
			continue
		}
		flags += fmt.Sprintf("-v %s:%s ", own, v.Target)
	}
	return out, flags, renamed
}

// renameVolumeMount renames to name every mount in cmd of the volume v.Base at
// v.Target: -v, --volume and --volume= with any options, and --mount and
// --mount= with source or src, and target, dst or destination.
func renameVolumeMount(cmd string, v RunVolume, name string) (string, bool) {
	did := false
	short := regexp.MustCompile(`(^|\s)((?:-v|--volume)(?:\s+|=)["']?)` + regexp.QuoteMeta(v.Base) +
		`(:` + regexp.QuoteMeta(v.Target) + `(?::[A-Za-z,]*)?["']?)(\s|$)`)
	cmd = short.ReplaceAllStringFunc(cmd, func(m string) string {
		did = true
		g := short.FindStringSubmatch(m)
		return g[1] + g[2] + name + g[3] + g[4]
	})
	long := regexp.MustCompile(`(^|\s)(--mount(?:\s+|=)["']?)([^\s"']+)`)
	cmd = long.ReplaceAllStringFunc(cmd, func(m string) string {
		g := long.FindStringSubmatch(m)
		fields := strings.Split(g[3], ",")
		src, target := -1, false
		for i, f := range fields {
			k, val, _ := strings.Cut(f, "=")
			switch k {
			case "source", "src":
				if val == v.Base {
					src = i
				}
			case "target", "dst", "destination":
				target = val == v.Target
			}
		}
		if src < 0 || !target {
			return m
		}
		did = true
		k, _, _ := strings.Cut(fields[src], "=")
		fields[src] = k + "=" + name
		return g[1] + g[2] + strings.Join(fields, ",")
	})
	return cmd, did
}
