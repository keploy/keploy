package http

import (
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// Strict HTTP matching under the proven switch (design v14 row S1; §9's
// "every call keploy mocks is held"): a recorded request answers a live one
// only when it went to the same place.
//
// The place is the authority the client sent: the Host header, or the URL's
// host for a request without one, with its port. Two upstreams behind one
// address (a gateway or LocalStack routing by virtual host) are two places,
// and so are two services that share a path ("/v1/charge" on two hosts).
//
// The container a request reached is not compared yet. Its key (package
// containerkey) is recorded on the mock, but it numbers an image's
// containers by their first use, which concurrent first uses can swap. Until
// the key names a container by its own configuration instead, a container
// reached on a port Docker publishes at random (Testcontainers) is another
// authority each run, and its recordings miss under the switch.

// destination is where an HTTP request went, as the strict matcher compares
// it: its authority, lowercased.
type destination struct {
	host string
}

// liveDestination is where the live request goes: the authority it sent.
func liveDestination(input *req) destination {
	d := destination{}
	if input.header != nil {
		d.host = input.header.Get("Host")
	}
	if d.host == "" && input.url != nil {
		d.host = input.url.Host
	}
	d.host = strings.ToLower(d.host)
	return d
}

// recordedDestination is where a recorded request went: its recorded
// authority (models.Mock.RecordedDestination).
func recordedDestination(m *models.Mock) destination {
	d := destination{}
	if host, ok := m.RecordedDestination(); ok {
		d.host = strings.ToLower(host)
	}
	return d
}

// sameDestination reports whether a recorded request went where the live one
// goes: to the same authority.
func sameDestination(live, rec destination) bool {
	return live.host == rec.host
}

// String is the destination as a report names it: its authority.
func (d destination) String() string {
	return d.host
}

// destinationDiff is the field difference a report shows when the closest
// recording went elsewhere, nil when it went to live: "host", the authority.
func destinationDiff(live destination, m *models.Mock) []models.MockFieldDiff {
	rec := recordedDestination(m)
	if sameDestination(live, rec) {
		return nil
	}
	return []models.MockFieldDiff{{Path: "host", Kind: models.DiffKindValueChanged, Expected: rec.host, Actual: live.host}}
}

// sameDestinationMocks are the mocks recorded for live's destination.
func sameDestinationMocks(live destination, mocks []*models.Mock) []*models.Mock {
	var out []*models.Mock
	for _, m := range mocks {
		if m != nil && m.Spec.HTTPReq != nil && sameDestination(live, recordedDestination(m)) {
			out = append(out, m)
		}
	}
	return out
}
