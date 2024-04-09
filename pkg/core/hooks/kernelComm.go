package hooks

import (
	"context"
	"fmt"

	"github.com/cilium/ebpf"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/structs"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

//TODO: rename this file.

// Get Used by proxy
func (h *Hooks) Get(_ context.Context, srcPort uint16) (*core.NetworkAddress, error) {
	d, err := h.GetDestinationInfo(srcPort)
	if err != nil {
		return nil, err
	}
	// TODO : need to implement eBPF code to differentiate between different apps
	s, ok := h.sess.Get(0)
	if !ok {
		return nil, fmt.Errorf("session not found")
	}

	return &core.NetworkAddress{
		AppID:    s.ID,
		Version:  d.IPVersion,
		IPv4Addr: d.DestIP4,
		IPv6Addr: d.DestIP6,
		Port:     d.DestPort,
	}, nil
}

// GetDestinationInfo retrieves destination information associated with a source port.
func (h *Hooks) GetDestinationInfo(srcPort uint16) (*structs.DestInfo, error) {
	h.m.Lock()
	defer h.m.Unlock()
	destInfo := structs.DestInfo{}
	if err := h.redirectProxyMap.Lookup(srcPort, &destInfo); err != nil {
		return nil, err
	}
	return &destInfo, nil
}

func (h *Hooks) Delete(_ context.Context, srcPort uint16) error {
	return h.CleanProxyEntry(srcPort)
}

func (h *Hooks) CleanProxyEntry(srcPort uint16) error {
	h.m.Lock()
	defer h.m.Unlock()
	err := h.redirectProxyMap.Delete(srcPort)
	if err != nil {
		utils.LogError(h.logger, err, "failed to remove entry from redirect proxy map")
		return err
	}
	h.logger.Debug("successfully removed entry from redirect proxy map", zap.Any("(Key)/SourcePort", srcPort))
	return nil
}

func (h *Hooks) SendKeployPid(pid uint32) error {
	h.logger.Debug("Sending keploy pid to kernel", zap.Any("pid", pid))
	err := h.keployPid.Update(uint32(0), &pid, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the keploy pid to the ebpf program")
		return err
	}
	return nil
}

// SendAppPid sends the application's process ID (PID) to the kernel.
// This function is used when running Keploy tests along with unit tests of the application.
func (h *Hooks) SendAppPid(pid uint32) error {
	h.logger.Debug("Sending app pid to kernel", zap.Any("app Pid", pid))
	err := h.appPidMap.Update(uint32(0), &pid, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the app pid to the ebpf program")
		return err
	}
	return nil
}

func (h *Hooks) SetKeployModeInKernel(mode uint32) error {
	key := 0
	err := h.keployModeMap.Update(uint32(key), &mode, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to set keploy mode in the epbf program")
		return err
	}
	return nil
}

// SendProxyInfo sends the IP and Port of the running proxy in the eBPF program.
func (h *Hooks) SendProxyInfo(ip4, port uint32, ip6 [4]uint32) error {
	key := 0
	err := h.proxyInfoMap.Update(uint32(key), structs.ProxyInfo{IP4: ip4, IP6: ip6, Port: port}, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the proxy IP & Port to the epbf program")
		return err
	}
	return nil
}

// SendInode sends the inode of the container to ebpf hooks to filter the network traffic
func (h *Hooks) SendInode(_ context.Context, _ uint64, inode uint64) error {
	return h.SendNameSpaceID(0, inode)
}

// SendNameSpaceID function is helpful when user application in running inside a docker container.
func (h *Hooks) SendNameSpaceID(key uint32, inode uint64) error {
	err := h.inodeMap.Update(key, &inode, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the namespace id to the epbf program", zap.Any("key", key), zap.Any("Inode", inode))
		return err
	}
	return nil
}

func (h *Hooks) SendCmdType(isDocker bool) error {
	// to notify the kernel hooks that the user application command is running in native linux or docker/docker-compose.
	key := 0
	err := h.DockerCmdMap.Update(uint32(key), &isDocker, ebpf.UpdateAny)
	if err != nil {
		return err
	}
	return nil
}

func (h *Hooks) SendDNSPort(port uint32) error {
	h.logger.Debug("sending dns server port", zap.Any("port", port))
	key := 0
	err := h.DNSPort.Update(uint32(key), &port, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send dns server port to the epbf program", zap.Any("dns server port", port))
		return err
	}
	return nil
}

func (h *Hooks) PassThroughPortsInKernel(_ context.Context, _ uint64, ports []uint) error {
	return h.SendPassThroughPorts(ports)
}

// SendPassThroughPorts sends the destination ports of the server which should not be intercepted by keploy proxy.
func (h *Hooks) SendPassThroughPorts(filterPorts []uint) error {
	portsSize := len(filterPorts)
	if portsSize > 10 {
		utils.LogError(h.logger, nil, "can not send more than 10 ports to be filtered to the ebpf program")
		return fmt.Errorf("passthrough ports limit exceeded")
	}

	var ports [10]int32

	for i := 0; i < 10; i++ {
		if i < portsSize {
			// Convert uint to int32
			ports[i] = int32(filterPorts[i])
		} else {
			// Fill the remaining elements with -1
			ports[i] = -1
		}
	}

	for i, v := range ports {
		h.logger.Debug(fmt.Sprintf("PassthroughPort(%v):[%v]", i, v))
		err := h.passthroughPorts.Update(uint32(i), &v, ebpf.UpdateAny)
		if err != nil {
			utils.LogError(h.logger, err, "failed to send the passthrough ports to the ebpf program")
			return err
		}
	}
	return nil
}

// For keploy test bench
// The below function is used to send the keploy record binary server port to the ebpf so that the flow first reaches to the keploy record proxy and then keploy test proxy

// SendKeployPorts is used to send keploy recordServer(key-0) or testServer(key-1) Port to the ebpf program
func (h *Hooks) SendKeployPorts(key models.ModeKey, port uint32) error {

	err := h.tbenchFilterPort.Update(key, &port, ebpf.UpdateAny)
	if err != nil {
		return err
	}
	return nil
}

// SendKeployPids is used to send keploy recordServer(key-0) or testServer(key-1) Pid to the ebpf program
func (h *Hooks) SendKeployPids(key models.ModeKey, pid uint32) error {

	err := h.tbenchFilterPid.Update(key, &pid, ebpf.UpdateAny)
	if err != nil {
		return err
	}
	return nil
}

//---------------------------
