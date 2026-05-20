package telemetry

import (
	"net/url"
	"sort"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// ExtractDomain extracts the hostname (domain without port) from a raw URL string.
// Returns empty string if parsing fails or no host is present.
func ExtractDomain(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	// Ensure there's a scheme so url.Parse works correctly for relative URLs like "/path"
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	// Return hostname without port for cleaner telemetry data
	host := u.Hostname()
	if host == "" {
		return ""
	}
	return host
}

// ExtractDomainsFromTestCase extracts host domains from an HTTP or gRPC test case.
// Only processes HTTP and gRPC kinds for performance.
func ExtractDomainsFromTestCase(tc *models.TestCase) []string {
	if tc == nil {
		return nil
	}
	var domains []string

	switch tc.Kind {
	case models.HTTP:
		// Extract from URL
		if d := ExtractDomain(tc.HTTPReq.URL); d != "" {
			domains = append(domains, d)
		}
		// Extract from Host header as fallback
		if host, ok := tc.HTTPReq.Header["Host"]; ok {
			if d := ExtractDomain(host); d != "" {
				domains = append(domains, d)
			}
		}
	case models.GRPC_EXPORT:
		// Extract from :authority pseudo-header
		if authority, ok := tc.GrpcReq.Headers.PseudoHeaders[":authority"]; ok {
			if d := ExtractDomain(authority); d != "" {
				domains = append(domains, d)
			}
		}
	}
	return domains
}

// ExtractDomainsFromMock extracts host domains from an HTTP or gRPC mock.
// Only processes HTTP and gRPC kinds for performance.
func ExtractDomainsFromMock(mock *models.Mock) []string {
	if mock == nil {
		return nil
	}
	var domains []string

	switch mock.Kind {
	case models.HTTP:
		if mock.Spec.HTTPReq != nil {
			if d := ExtractDomain(mock.Spec.HTTPReq.URL); d != "" {
				domains = append(domains, d)
			}
			if host, ok := mock.Spec.HTTPReq.Header["Host"]; ok {
				if d := ExtractDomain(host); d != "" {
					domains = append(domains, d)
				}
			}
		}
	case models.GRPC_EXPORT:
		if mock.Spec.GRPCReq != nil {
			if authority, ok := mock.Spec.GRPCReq.Headers.PseudoHeaders[":authority"]; ok {
				if d := ExtractDomain(authority); d != "" {
					domains = append(domains, d)
				}
			}
		}
	case models.HTTP2:
		if mock.Spec.HTTP2Req != nil {
			if d := ExtractDomain(mock.Spec.HTTP2Req.URL); d != "" {
				domains = append(domains, d)
			}
			if mock.Spec.HTTP2Req.Authority != "" {
				if d := ExtractDomain(mock.Spec.HTTP2Req.Authority); d != "" {
					domains = append(domains, d)
				}
			}
		}
	}
	return domains
}

// DomainSet is a thread-safe set for collecting unique domain strings.
type DomainSet struct {
	mu      sync.Mutex
	domains map[string]struct{}
}

// NewDomainSet creates a new DomainSet.
func NewDomainSet() *DomainSet {
	return &DomainSet{domains: make(map[string]struct{})}
}

// Add adds a domain to the set. Safe for concurrent use.
func (ds *DomainSet) Add(domain string) {
	if domain != "" {
		ds.mu.Lock()
		ds.domains[domain] = struct{}{}
		ds.mu.Unlock()
	}
}

// AddAll adds multiple domains to the set. Safe for concurrent use.
func (ds *DomainSet) AddAll(domains []string) {
	if len(domains) == 0 {
		return
	}
	ds.mu.Lock()
	for _, d := range domains {
		if d != "" {
			ds.domains[d] = struct{}{}
		}
	}
	ds.mu.Unlock()
}

// ToSlice returns the domains as a deterministically sorted slice.
func (ds *DomainSet) ToSlice() []string {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if len(ds.domains) == 0 {
		return nil
	}
	result := make([]string, 0, len(ds.domains))
	for d := range ds.domains {
		result = append(result, d)
	}
	sort.Strings(result)
	return result
}
