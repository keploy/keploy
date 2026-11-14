package telemetry

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.keploy.io/server/v3/pkg/models"
)

func TestExtractDomain(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{name: "URL with scheme, port and path", rawURL: "https://api.example.com:8443/v1", want: "api.example.com"},
		{name: "bare host", rawURL: "api.example.com", want: "api.example.com"},
		{name: "bare host with port", rawURL: "db.internal:5432", want: "db.internal"},
		{name: "path without a host", rawURL: "/v1/users", want: ""},
		{name: "empty", rawURL: "", want: ""},
		{name: "unclosed IPv6 bracket", rawURL: "http://[::1", want: ""},
		{name: "invalid escape in host", rawURL: "%zz", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtractDomain(tt.rawURL))
		})
	}
}

func TestExtractDomainsFromMock(t *testing.T) {
	tests := []struct {
		name string
		mock *models.Mock
		want []string
	}{
		{name: "nil mock", mock: nil, want: nil},
		{
			name: "HTTP mock with a URL and a Host header",
			mock: &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{
				URL:    "http://users-svc:8080/v1/users",
				Header: map[string]string{"Host": "users.example.com"},
			}}},
			want: []string{"users-svc", "users.example.com"},
		},
		{
			name: "gRPC mock with an :authority pseudo-header",
			mock: &models.Mock{Kind: models.GRPC_EXPORT, Spec: models.MockSpec{GRPCReq: &models.GrpcReq{
				Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":authority": "orders.example.com:443"}},
			}}},
			want: []string{"orders.example.com"},
		},
		{
			name: "HTTP2 mock with a URL and an authority",
			mock: &models.Mock{Kind: models.HTTP2, Spec: models.MockSpec{HTTP2Req: &models.HTTP2Req{
				URL:       "https://h2.example.com/items",
				Authority: "h2-gateway.example.com:443",
			}}},
			want: []string{"h2.example.com", "h2-gateway.example.com"},
		},
		{name: "generic mock", mock: &models.Mock{Kind: models.GENERIC}, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtractDomainsFromMock(tt.mock))
		})
	}
}

func TestExtractDomainsFromTestCase(t *testing.T) {
	tests := []struct {
		name string
		tc   *models.TestCase
		want []string
	}{
		{name: "nil test case", tc: nil, want: nil},
		{
			name: "HTTP test case with a URL and a Host header",
			tc: &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{
				URL:    "http://localhost:8080/api/users",
				Header: map[string]string{"Host": "app.example.com"},
			}},
			want: []string{"localhost", "app.example.com"},
		},
		{
			name: "gRPC test case with an :authority pseudo-header",
			tc: &models.TestCase{Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{
				Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":authority": "localhost:50051"}},
			}},
			want: []string{"localhost"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtractDomainsFromTestCase(tt.tc))
		})
	}
}

func TestDomainSet(t *testing.T) {
	tests := []struct {
		name   string
		add    []string
		addAll []string
		want   []string
	}{
		{name: "empty set", want: nil},
		{name: "same host added twice", add: []string{"api.example.com", "api.example.com"}, want: []string{"api.example.com"}},
		{
			name:   "hosts from Add and AddAll come back sorted and unique",
			add:    []string{"db.example.com"},
			addAll: []string{"cache.example.com", "db.example.com"},
			want:   []string{"cache.example.com", "db.example.com"},
		},
		{name: "empty strings are not kept", add: []string{""}, addAll: []string{"", "api.example.com"}, want: []string{"api.example.com"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := NewDomainSet()
			for _, d := range tt.add {
				set.Add(d)
			}
			set.AddAll(tt.addAll)
			assert.Equal(t, tt.want, set.ToSlice())
		})
	}
}

func TestDomainSet_ConcurrentAddsKeepEachHostOnce(t *testing.T) {
	set := NewDomainSet()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			set.Add("api.example.com")
			set.AddAll([]string{"api.example.com", "db.example.com"})
		})
	}
	wg.Wait()

	assert.Equal(t, []string{"api.example.com", "db.example.com"}, set.ToSlice())
}
