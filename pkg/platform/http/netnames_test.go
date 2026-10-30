package http

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

type fakeNameInspector struct {
	containers map[string]container.InspectResponse
	networks   map[string]network.Inspect
}

func (f fakeNameInspector) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	c, ok := f.containers[id]
	if !ok {
		return container.InspectResponse{}, errors.New("no such container")
	}
	return c, nil
}

func (f fakeNameInspector) NetworkInspect(_ context.Context, id string, _ network.InspectOptions) (network.Inspect, error) {
	n, ok := f.networks[id]
	if !ok {
		return network.Inspect{}, errors.New("no such network")
	}
	return n, nil
}

func onNetworks(eps map[string]*network.EndpointSettings) container.InspectResponse {
	return container.InspectResponse{NetworkSettings: &container.NetworkSettings{Networks: eps}}
}

// The list holds what docker's resolver answers on the agent container's
// networks: each container's DNSNames, or from an older daemon its name,
// aliases and short ID, each also qualified by the network. The default
// bridge, host and none hold no names; a container gone while listed keeps
// only its endpoint's name.
func TestListNetworkNames(t *testing.T) {
	const legacyID = "0123456789abcdef0123"
	f := fakeNameInspector{
		containers: map[string]container.InspectResponse{
			"keploy-v3-agent": onNetworks(map[string]*network.EndpointSettings{
				"shop_default": {}, "bridge": {},
			}),
			"dbid": onNetworks(map[string]*network.EndpointSettings{
				"shop_default": {DNSNames: []string{"shop-db-1", "DB", "4e14c014c7c4"}},
			}),
			legacyID: onNetworks(map[string]*network.EndpointSettings{
				"shop_default": {Aliases: []string{"cache"}},
			}),
		},
		networks: map[string]network.Inspect{
			"shop_default": {Containers: map[string]network.EndpointResource{
				"dbid":   {Name: "shop-db-1"},
				legacyID: {Name: "shop-cache-1"},
				"goneid": {Name: "shop-gone-1"},
			}},
		},
	}
	l, err := listNetworkNames(context.Background(), f, "keploy-v3-agent")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"0123456789ab", "0123456789ab.shop_default", "4e14c014c7c4", "4e14c014c7c4.shop_default",
		"cache", "cache.shop_default", "db", "db.shop_default",
		"shop-cache-1", "shop-cache-1.shop_default", "shop-db-1", "shop-db-1.shop_default",
		"shop-gone-1", "shop-gone-1.shop_default",
	}
	if !slices.Equal(l.Names, want) {
		t.Fatalf("names = %v\nwant    %v", l.Names, want)
	}
	if !slices.Equal(l.Networks, []string{"shop_default"}) {
		t.Fatalf("networks = %v, want [shop_default]", l.Networks)
	}
	if _, err := listNetworkNames(context.Background(), f, "missing"); err == nil {
		t.Fatal("an agent container that cannot be inspected listed names")
	}
}
