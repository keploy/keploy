package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func http1MockForTest(name string, header map[string]string, url string) *models.Mock {
	mk := newMockForTest(name, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), models.LifetimeSession)
	mk.Kind = models.HTTP
	mk.Spec.HTTPReq = &models.HTTPReq{URL: url, Header: header}
	return mk
}

func TestReplayTargetHasHTTP1Mock(t *testing.T) {
	tests := []struct {
		name    string
		mocks   []*models.Mock
		sniHost string
		port    uint32
		want    bool
	}{
		{
			name:  "host header with port matches on port",
			mocks: []*models.Mock{http1MockForTest("a", map[string]string{"Host": "openbao.svc:8200"}, "/v1/auth/kubernetes/login")},
			port:  8200,
			want:  true,
		},
		{
			name:  "host header key is matched case-insensitively",
			mocks: []*models.Mock{http1MockForTest("a", map[string]string{"host": "openbao.svc:8200"}, "/v1/login")},
			port:  8200,
			want:  true,
		},
		{
			name:  "absolute url is used when host header is absent",
			mocks: []*models.Mock{http1MockForTest("a", nil, "http://openbao.svc:8200/v1/login")},
			port:  8200,
			want:  true,
		},
		{
			name:    "host disambiguates when sni is known",
			mocks:   []*models.Mock{http1MockForTest("a", map[string]string{"Host": "openbao.svc:8200"}, "/v1/login")},
			sniHost: "vault.other",
			port:    8200,
			want:    false,
		},
		{
			name:  "different port does not match",
			mocks: []*models.Mock{http1MockForTest("a", map[string]string{"Host": "openbao.svc:8200"}, "/v1/login")},
			port:  9999,
			want:  false,
		},
		{
			name:  "no http1 mocks",
			mocks: []*models.Mock{h2MockForTest("h2", "openbao.svc:8200")},
			port:  8200,
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			mm.SetUnFilteredMocks(tc.mocks)
			p := &Proxy{mockManager: mm}
			if got := p.replayTargetHasHTTP1Mock(tc.sniHost, tc.port); got != tc.want {
				t.Fatalf("replayTargetHasHTTP1Mock(%q, %d) = %v, want %v", tc.sniHost, tc.port, got, tc.want)
			}
		})
	}
}
