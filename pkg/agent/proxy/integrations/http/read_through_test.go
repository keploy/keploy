package http

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

var _ integrations.ReadThroughMatcher = (*rtTestDb)(nil)

type rtTestDb struct {
	*mockMemDb
	perTest     []*models.Mock
	readThrough []*models.Mock
}

func (d *rtTestDb) GetFilteredMocks() ([]*models.Mock, error)         { return d.perTest, nil }
func (d *rtTestDb) GetFilteredMocksInWindow() ([]*models.Mock, error) { return d.perTest, nil }
func (d *rtTestDb) GetPerTestMocksInWindow() ([]*models.Mock, error)  { return d.perTest, nil }
func (d *rtTestDb) GetSessionMocks() ([]*models.Mock, error)          { return nil, nil }
func (d *rtTestDb) GetUnFilteredMocks() ([]*models.Mock, error)       { return nil, nil }
func (d *rtTestDb) GetSessionScopedMocks() ([]*models.Mock, error)    { return nil, nil }

func (d *rtTestDb) MatchReadThrough(live *models.Mock) (*models.Mock, bool) {
	lk, ok := models.ReadThroughRequestKey(live)
	if !ok {
		return nil, false
	}
	for _, rt := range d.readThrough {
		if !models.IsReadThrough(rt) {
			continue
		}
		if rk, ok := models.ReadThroughKey(rt); ok && lk == rk {
			return rt, true
		}
	}
	return nil, false
}

func newHTTPReadThroughMock(name, method, rawURL, body string, headers map[string]string, ts time.Time) *models.Mock {
	var urlParams map[string]string
	if u, err := url.Parse(rawURL); err == nil && len(u.Query()) > 0 {
		urlParams = make(map[string]string, len(u.Query()))
		for k, vals := range u.Query() {
			if len(vals) > 0 {
				urlParams[k] = vals[0]
			}
		}
	}
	return &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    name,
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			HTTPReq: &models.HTTPReq{
				Method:    models.Method(method),
				URL:       rawURL,
				URLParams: urlParams,
				Header:    headers,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Body:       body,
			},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(5 * time.Millisecond),
		},
	}
}

func TestHTTPReadThrough_FallbackExactWriteExclusionPrecedenceAndRegressionGuards(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)

			h := &HTTP{Logger: zap.NewNop()}
			t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

			rtJWKS := newHTTPReadThroughMock(
				"mock-rt-jwks", "GET", "http://auth.internal/v1/jwks?tenant=acme",
				`{"source":"read-through"}`,
				map[string]string{"Host": "auth.internal", "Accept": "application/json"},
				t0.Add(-30*time.Second),
			)
			if !models.MarkReadThrough(rtJWKS) {
				t.Fatal("MarkReadThrough failed for GET /v1/jwks")
			}

			// Hand-tagged POST write MUST never be served from read-through.
			rtPost := newHTTPReadThroughMock(
				"mock-rt-post", "POST", "http://auth.internal/v1/tokens",
				`{"created":true}`,
				map[string]string{"Host": "auth.internal"},
				t0.Add(-20*time.Second),
			)
			rtPost.Spec.Metadata[models.MetadataReadThroughKey] = models.MetadataReadThroughValue

			ownJWKS := newHTTPReadThroughMock(
				"mock-own-jwks", "GET", "/v1/jwks?tenant=acme",
				`{"source":"per-test-own"}`,
				map[string]string{
					"Host":   "auth.internal",
					"Accept": "application/json",
				},
				t0.Add(10*time.Second),
			)
			ownJWKS.TestModeInfo.IsFiltered = true
			ownJWKS.TestModeInfo.Lifetime = models.LifetimePerTest
			ownJWKS.TestModeInfo.LifetimeDerived = true

			db := &rtTestDb{
				mockMemDb:   &mockMemDb{deleteFilteredReturn: true},
				perTest:     []*models.Mock{ownJWKS},
				readThrough: []*models.Mock{rtJWKS, rtPost},
			}

			mkReq := func(method, rawURL string, hdr http.Header) *req {
				u, err := url.Parse(rawURL)
				if err != nil {
					t.Fatal(err)
				}
				// Mirror decode.go: live HTTP/1.1 requests carry relative path+query
				// on input.url and Host in input.header.
				return &req{method: method, url: &url.URL{Path: u.Path, RawQuery: u.RawQuery}, header: hdr}
			}
			liveJWKS := mkReq("GET", "http://auth.internal/v1/jwks?tenant=acme", http.Header{
				"Host":   []string{"auth.internal"},
				"Accept": []string{"application/json"},
			})

			// 1. Per-test precedence: primary match returns ownJWKS first.
			ok, stub, diag1, err := h.match(context.Background(), liveJWKS, db, nil, nil, nil, false, false, false, false, false)
			if err != nil || !ok || stub == nil {
				t.Fatalf("expected per-test match, got ok=%v stub=%v diag=%+v err=%v", ok, stub, diag1, err)
			}
			if stub.Name != ownJWKS.Name {
				t.Fatalf("per-test precedence violated: got %s, want %s", stub.Name, ownJWKS.Name)
			}

			// 2. When per-test pool has no recording for this route (cold cache),
			//    primary match misses and tryReadThrough serves exact match.
			db.perTest = nil
			ok, _, diag, err := h.match(context.Background(), liveJWKS, db, nil, nil, nil, false, false, false, false, false)
			if err != nil || ok {
				t.Fatalf("expected primary miss with empty per-test pool, got ok=%v err=%v", ok, err)
			}
			rt, hit := h.tryReadThrough(context.Background(), liveJWKS, db, diag, nil)
			if !hit || rt == nil || rt.Name != rtJWKS.Name {
				t.Fatalf("expected exact read-through hit, got hit=%v rt=%v", hit, rt)
			}

			// 3. Regression guards: never mask MatchPhaseStrict (changed call on
			//    test's own route), variable mismatch, exhausted stateful walk,
			//    or full-round learnMiss (changed call / moved / answered / closest).
			if m, hit := h.tryReadThrough(context.Background(), liveJWKS, db, &matchDiag{phase: models.MatchPhaseStrict}, nil); hit || m != nil {
				t.Fatal("tryReadThrough must refuse MatchPhaseStrict (changed call regression)")
			}
			if m, hit := h.tryReadThrough(context.Background(), liveJWKS, db, &matchDiag{variable: &varProblem{}}, nil); hit || m != nil {
				t.Fatal("tryReadThrough must refuse diag.variable != nil")
			}
			if m, hit := h.tryReadThrough(context.Background(), liveJWKS, db, &matchDiag{answered: &answeredSequence{}}, nil); hit || m != nil {
				t.Fatal("tryReadThrough must refuse diag.answered != nil")
			}
			for name, lm := range map[string]*learnMiss{
				"changed":  {res: learn.Result{Reason: models.MissRequestChanged}},
				"moved":    {res: learn.Result{Reason: models.MissUnmockedCall}, moved: true},
				"answered": {res: learn.Result{Reason: models.MissUnmockedCall}, answered: &answeredSequence{}},
				"closest":  {res: learn.Result{Reason: models.MissUnmockedCall}, closest: ownJWKS},
				"empty":    {res: learn.Result{Reason: ""}},
			} {
				if m, hit := h.tryReadThrough(context.Background(), liveJWKS, db, nil, lm); hit || m != nil {
					t.Fatalf("tryReadThrough must refuse learnMiss %s, got %v", name, m)
				}
			}
			// Genuine unmocked full-round learnMiss (MissUnmockedCall, no closest,
			// not moved/answered) MAY fall back to read-through.
			if m, hit := h.tryReadThrough(context.Background(), liveJWKS, db, nil, &learnMiss{res: learn.Result{Reason: models.MissUnmockedCall}}); !hit || m == nil || m.Name != rtJWKS.Name {
				t.Fatalf("expected read-through hit for unmocked learnMiss, got hit=%v m=%v", hit, m)
			}

			// 4. Exact-match only: different query parameter MUST miss.
			diffTenant := mkReq("GET", "http://auth.internal/v1/jwks?tenant=other", http.Header{
				"Host":   []string{"auth.internal"},
				"Accept": []string{"application/json"},
			})
			if m, hit := h.tryReadThrough(context.Background(), diffTenant, db, nil, nil); hit || m != nil {
				t.Fatalf("different query tenant MUST miss read-through, got %v", m)
			}

			// 5. Real HTTPS/1.1 record->replay symmetry: buildHTTPMock and
			//    parseFinalHTTP receive origin-form HTTP/1.1 wire bytes
			//    ("GET /v1/jwks?tenant=secure HTTP/1.1") with no scheme in the
			//    request-target. When recorded over TLS (DstCfg.TLSCfg != nil
			//    or FinalHTTP.TLS == true), both MUST stamp meta["tls"]="true"
			//    so cold TLS replay matches and plaintext HTTP misses, while
			//    plaintext recordings MUST NOT stamp meta["tls"].
			rawHTTPSReq := []byte("GET /v1/jwks?tenant=secure HTTP/1.1\r\nHost: auth.internal\r\nAccept: application/json\r\n\r\n")
			rawHTTPSResp := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 31\r\n\r\n{\"source\":\"https-read-through\"}")
			tlsOpts := models.OutgoingOptions{
				DstCfg: &models.ConditionalDstCfg{
					Addr:   "auth.internal:443",
					Port:   443,
					TLSCfg: &tls.Config{MinVersion: tls.VersionTLS12},
				},
			}
			rtHTTPS, err := h.buildHTTPMock(context.Background(), &FinalHTTP{
				Req:              rawHTTPSReq,
				Resp:             rawHTTPSResp,
				ReqTimestampMock: t0.Add(-25 * time.Second),
				ResTimestampMock: t0.Add(-25*time.Second + 5*time.Millisecond),
			}, 443, "conn-https-v2", tlsOpts)
			if err != nil || rtHTTPS == nil {
				t.Fatalf("buildHTTPMock(TLS) = (%v, %v)", rtHTTPS, err)
			}
			if rtHTTPS.Spec.HTTPReq.URL != "/v1/jwks?tenant=secure" {
				t.Fatalf("expected origin-form recorded URL, got %q", rtHTTPS.Spec.HTTPReq.URL)
			}
			if rtHTTPS.Spec.Metadata["tls"] != "true" {
				t.Fatalf("buildHTTPMock over TLS must stamp meta[tls]=true, got %+v", rtHTTPS.Spec.Metadata)
			}
			rtHTTPS.Name = "mock-rt-https-jwks"
			if !models.MarkReadThrough(rtHTTPS) {
				t.Fatal("MarkReadThrough failed for buildHTTPMock HTTPS GET /v1/jwks")
			}
			db.readThrough = append(db.readThrough, rtHTTPS)

			liveHTTPS := mkReq("GET", "http://auth.internal/v1/jwks?tenant=secure", http.Header{
				"Host":   []string{"auth.internal"},
				"Accept": []string{"application/json"},
			})
			liveHTTPS.tls = true
			if m, hit := h.tryReadThrough(context.Background(), liveHTTPS, db, nil, nil); !hit || m == nil || m.Name != rtHTTPS.Name {
				t.Fatalf("live TLS request must match buildHTTPMock-recorded HTTPS read-through mock, got hit=%v m=%v", hit, m)
			}
			livePlainSecurePath := mkReq("GET", "http://auth.internal/v1/jwks?tenant=secure", http.Header{
				"Host":   []string{"auth.internal"},
				"Accept": []string{"application/json"},
			})
			if m, hit := h.tryReadThrough(context.Background(), livePlainSecurePath, db, nil, nil); hit || m != nil {
				t.Fatalf("plaintext HTTP request must NOT match buildHTTPMock-recorded HTTPS read-through mock, got %v", m)
			}

			// Plaintext buildHTTPMock MUST NOT stamp meta["tls"].
			plainRec, err := h.buildHTTPMock(context.Background(), &FinalHTTP{
				Req:              rawHTTPSReq,
				Resp:             rawHTTPSResp,
				ReqTimestampMock: t0.Add(-24 * time.Second),
				ResTimestampMock: t0.Add(-24*time.Second + 5*time.Millisecond),
			}, 80, "conn-plain-v2", models.OutgoingOptions{})
			if err != nil || plainRec == nil {
				t.Fatalf("buildHTTPMock(plain) = (%v, %v)", plainRec, err)
			}
			if _, hasTLS := plainRec.Spec.Metadata["tls"]; hasTLS {
				t.Fatalf("plaintext buildHTTPMock must NOT stamp meta[tls], got %+v", plainRec.Spec.Metadata)
			}
			keyPlainV2, okPlainV2 := models.ReadThroughKey(plainRec)
			if !okPlainV2 {
				t.Fatal("ReadThroughKey(plainRec) unexpectedly false")
			}

			// Recorded absolute https:// URL must also match live TLS replay
			// and miss plaintext HTTP.
			rtAbsHTTPS := newHTTPReadThroughMock(
				"mock-rt-abs-https-jwks", "GET", "https://auth.internal/v1/jwks?tenant=abs",
				`{"source":"abs-https"}`,
				map[string]string{"Host": "auth.internal", "Accept": "application/json"},
				t0.Add(-24*time.Second),
			)
			if !models.MarkReadThrough(rtAbsHTTPS) {
				t.Fatal("MarkReadThrough failed for absolute https:// mock")
			}
			absDB := &rtTestDb{
				mockMemDb:   &mockMemDb{deleteFilteredReturn: true},
				readThrough: []*models.Mock{rtAbsHTTPS},
			}
			liveAbsHTTPS := mkReq("GET", "http://auth.internal/v1/jwks?tenant=abs", http.Header{
				"Host":   []string{"auth.internal"},
				"Accept": []string{"application/json"},
			})
			liveAbsHTTPS.tls = true
			if m, hit := h.tryReadThrough(context.Background(), liveAbsHTTPS, absDB, nil, nil); !hit || m == nil || m.Name != rtAbsHTTPS.Name {
				t.Fatalf("live TLS request must match recorded absolute https:// mock, got hit=%v m=%v", hit, m)
			}
			liveAbsPlain := mkReq("GET", "http://auth.internal/v1/jwks?tenant=abs", http.Header{
				"Host":   []string{"auth.internal"},
				"Accept": []string{"application/json"},
			})
			if m, hit := h.tryReadThrough(context.Background(), liveAbsPlain, absDB, nil, nil); hit || m != nil {
				t.Fatalf("plaintext request must NOT match recorded absolute https:// mock, got %v", m)
			}

			// Legacy parseFinalHTTP TLS paths (both DstCfg.TLSCfg != nil and
			// direct *tls.Conn via FinalHTTP.TLS with DstCfg == nil) MUST stamp
			// meta["tls"]="true" and produce the exact same read-through key as
			// buildHTTPMock, while plaintext parseFinalHTTP MUST NOT stamp it.
			keyV2, okV2 := models.ReadThroughKey(rtHTTPS)
			if !okV2 {
				t.Fatal("ReadThroughKey(rtHTTPS) unexpectedly false")
			}
			for _, tc := range []struct {
				name    string
				final   *FinalHTTP
				opts    models.OutgoingOptions
				wantTLS bool
			}{
				{
					name: "dstcfg-tls",
					final: &FinalHTTP{
						Req:              rawHTTPSReq,
						Resp:             rawHTTPSResp,
						ReqTimestampMock: t0.Add(-23 * time.Second),
						ResTimestampMock: t0.Add(-23*time.Second + 5*time.Millisecond),
					},
					opts:    tlsOpts,
					wantTLS: true,
				},
				{
					name: "direct-tls-conn",
					final: &FinalHTTP{
						Req:              rawHTTPSReq,
						Resp:             rawHTTPSResp,
						ReqTimestampMock: t0.Add(-22 * time.Second),
						ResTimestampMock: t0.Add(-22*time.Second + 5*time.Millisecond),
						TLS:              true,
					},
					opts:    models.OutgoingOptions{},
					wantTLS: true,
				},
				{
					name: "plain",
					final: &FinalHTTP{
						Req:              rawHTTPSReq,
						Resp:             rawHTTPSResp,
						ReqTimestampMock: t0.Add(-21 * time.Second),
						ResTimestampMock: t0.Add(-21*time.Second + 5*time.Millisecond),
					},
					opts:    models.OutgoingOptions{},
					wantTLS: false,
				},
			} {
				var legacyRec *models.Mock
				legacyCtx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "conn-legacy-"+tc.name)
				legacyCtx = syncMock.NewContext(legacyCtx, syncMock.New(zap.NewNop()))
				if err := h.parseFinalHTTP(legacyCtx, tc.final, 443, make(chan *models.Mock, 1), tc.opts, func(m *models.Mock) {
					legacyRec = m
				}); err != nil || legacyRec == nil {
					t.Fatalf("parseFinalHTTP(%s) = (%v, %v)", tc.name, legacyRec, err)
				}
				gotTLS := legacyRec.Spec.Metadata["tls"] == "true"
				if gotTLS != tc.wantTLS {
					t.Fatalf("parseFinalHTTP(%s) meta[tls]=%q, wantTLS=%v", tc.name, legacyRec.Spec.Metadata["tls"], tc.wantTLS)
				}
				if tc.wantTLS {
					keyLegacy, okLegacy := models.ReadThroughKey(legacyRec)
					if !okLegacy || keyLegacy != keyV2 {
						t.Fatalf("parseFinalHTTP(%s) key (%q,%v) != buildHTTPMock key %q", tc.name, keyLegacy, okLegacy, keyV2)
					}
					legacyRec.Name = "mock-rt-https-legacy-" + tc.name
					if !models.MarkReadThrough(legacyRec) {
						t.Fatalf("MarkReadThrough failed for parseFinalHTTP(%s)", tc.name)
					}
					legacyDB := &rtTestDb{
						mockMemDb:   &mockMemDb{deleteFilteredReturn: true},
						readThrough: []*models.Mock{legacyRec},
					}
					if m, hit := h.tryReadThrough(context.Background(), liveHTTPS, legacyDB, nil, nil); !hit || m == nil || m.Name != legacyRec.Name {
						t.Fatalf("live TLS request must match parseFinalHTTP(%s) recorded mock, got hit=%v m=%v", tc.name, hit, m)
					}
					if m, hit := h.tryReadThrough(context.Background(), livePlainSecurePath, legacyDB, nil, nil); hit || m != nil {
						t.Fatalf("plaintext request must NOT match parseFinalHTTP(%s) TLS mock, got %v", tc.name, m)
					}
				} else {
					keyLegacyPlain, okLegacyPlain := models.ReadThroughKey(legacyRec)
					if !okLegacyPlain || keyLegacyPlain != keyPlainV2 {
						t.Fatalf("parseFinalHTTP(%s) plain key (%q,%v) != buildHTTPMock plain key %q", tc.name, keyLegacyPlain, okLegacyPlain, keyPlainV2)
					}
				}
			}

			// Real encodeHTTP over a *tls.Conn client connection with DstCfg==nil
			// MUST propagate clientIsTLS -> FinalHTTP.TLS -> meta["tls"]="true"
			// and produce the exact same read-through key as buildHTTPMock.
			ts := httptest.NewTLSServer(nil)
			cert := ts.TLS.Certificates[0]
			ts.Close()
			appRaw, proxyClientRaw := tcpPair(t)
			up, proxyDest := tcpPair(t)
			proxyTLSClient := tls.Server(proxyClientRaw, &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			})
			appTLSClient := tls.Client(appRaw, &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // self-signed test fixture cert on loopback
				MinVersion:         tls.VersionTLS12,
			})
			hsErrCh := make(chan error, 1)
			go func() {
				hsErrCh <- appTLSClient.Handshake()
			}()
			if err := proxyTLSClient.Handshake(); err != nil {
				t.Fatalf("proxy TLS handshake: %v", err)
			}
			if err := <-hsErrCh; err != nil {
				t.Fatalf("app TLS handshake: %v", err)
			}
			go func() {
				_ = readUntil(up, "\r\n\r\n", 5*time.Second)
				_, _ = up.Write(rawHTTPSResp)
				_ = up.Close()
			}()
			encRecorded := make(chan *models.Mock, 1)
			encCtx, encCancel := context.WithTimeout(context.Background(), 5*time.Second)
			encGroup, encGctx := errgroup.WithContext(encCtx)
			encGctx = context.WithValue(encGctx, models.ErrGroupKey, encGroup)
			encGctx = context.WithValue(encGctx, models.ClientConnectionIDKey, "conn-encode-tls")
			encGctx = syncMock.NewContext(encGctx, syncMock.New(zap.NewNop()))
			encDone := make(chan error, 1)
			wrappedProxyClient := pUtil.NewSafeConn(&pUtil.Conn{Conn: proxyTLSClient, Logger: zap.NewNop()}, zap.NewNop())
			go func() {
				encDone <- h.encodeHTTP(encGctx, rawHTTPSReq, wrappedProxyClient, proxyDest, make(chan *models.Mock, 1), models.OutgoingOptions{}, func(m *models.Mock) {
					encRecorded <- m
				})
			}()
			respBuf := make([]byte, len(rawHTTPSResp))
			if _, err := io.ReadFull(appTLSClient, respBuf); err != nil {
				t.Fatalf("read TLS response from encodeHTTP: %v", err)
			}
			_ = appTLSClient.Close()
			_ = proxyTLSClient.Close()
			_ = proxyDest.Close()
			<-encDone
			_ = encGroup.Wait()
			encCancel()
			select {
			case encMock := <-encRecorded:
				if encMock.Spec.Metadata["tls"] != "true" {
					t.Fatalf("encodeHTTP over *tls.Conn with DstCfg==nil must stamp meta[tls]=true, got %+v", encMock.Spec.Metadata)
				}
				keyEnc, okEnc := models.ReadThroughKey(encMock)
				if !okEnc || keyEnc != keyV2 {
					t.Fatalf("encodeHTTP(*tls.Conn) key (%q,%v) != buildHTTPMock(TLS) key %q", keyEnc, okEnc, keyV2)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("encodeHTTP over *tls.Conn did not record mock")
			}

			// End-to-end YAML persistence check: persist buildHTTPMock's origin-form
			// HTTPS read-through mock via real mockdb, reload from disk, and verify
			// tryReadThrough matches live TLS and misses plaintext HTTP.
			yamlDir := t.TempDir()
			yamlDB := mockdb.New(zap.NewNop(), yamlDir, "mocks")
			if err := yamlDB.InsertMock(context.Background(), rtHTTPS, "test-set-https"); err != nil {
				t.Fatalf("InsertMock(rtHTTPS): %v", err)
			}
			reloaded, err := yamlDB.GetTestSetMocks(context.Background(), "test-set-https", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatalf("GetTestSetMocks: %v", err)
			}
			reloadedRT := append(reloaded.Filtered, reloaded.Unfiltered...)
			reloadedDB := &rtTestDb{
				mockMemDb:   &mockMemDb{deleteFilteredReturn: true},
				readThrough: reloadedRT,
			}
			if m, hit := h.tryReadThrough(context.Background(), liveHTTPS, reloadedDB, nil, nil); !hit || m == nil {
				t.Fatalf("live TLS request must match YAML-reloaded buildHTTPMock HTTPS mock, got hit=%v m=%v", hit, m)
			}
			if m, hit := h.tryReadThrough(context.Background(), livePlainSecurePath, reloadedDB, nil, nil); hit || m != nil {
				t.Fatalf("plaintext request must NOT match YAML-reloaded buildHTTPMock HTTPS mock, got %v", m)
			}

			// 6. Write exclusion: POST MUST miss even if hand-tagged readThrough.
			livePost := mkReq("POST", "http://auth.internal/v1/tokens", http.Header{
				"Host": []string{"auth.internal"},
			})
			if m, hit := h.tryReadThrough(context.Background(), livePost, db, nil, nil); hit || m != nil {
				t.Fatalf("POST write MUST NEVER match read-through, got %v", m)
			}
		})
	}
}

// TestHTTPReadThrough_ProxylessExplicitTLSFlag verifies that observe-only /
// proxyless capture (SkipTLSMITM=true, DstCfg.TLSCfg==nil, non-TLS conn,
// origin-form /v1/jwks) stamps meta["tls"]="true" when OutgoingOptions.TLS is
// set, producing the exact same read-through key as MITM TLS replay.
func TestHTTPReadThrough_ProxylessExplicitTLSFlag(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	rawReq := []byte("GET /v1/jwks HTTP/1.1\r\nHost: auth.internal\r\nAccept: application/json\r\n\r\n")
	rawResp := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 22\r\n\r\n{\"keys\":[\"proxyless\"]}")
	opts := models.OutgoingOptions{
		SkipTLSMITM: true,
		TLS:         true,
		DstCfg:      &models.ConditionalDstCfg{Addr: "auth.internal:443", Port: 443},
	}

	recV2, err := h.buildHTTPMock(context.Background(), &FinalHTTP{
		Req:              rawReq,
		Resp:             rawResp,
		ReqTimestampMock: t0,
		ResTimestampMock: t0.Add(5 * time.Millisecond),
	}, 443, "conn-proxyless", opts)
	if err != nil || recV2 == nil {
		t.Fatalf("buildHTTPMock(OutgoingOptions.TLS) = (%v, %v)", recV2, err)
	}
	if recV2.Spec.Metadata["tls"] != "true" {
		t.Fatalf("buildHTTPMock(OutgoingOptions.TLS) did not stamp meta[tls]=true: %+v", recV2.Spec.Metadata)
	}
	if !models.MarkReadThrough(recV2) {
		t.Fatal("MarkReadThrough failed for OutgoingOptions.TLS")
	}
	recV2.Name = "mock-rt-proxyless"

	// Legacy parseFinalHTTP must also honor OutgoingOptions.TLS.
	var recLegacy *models.Mock
	legacyCtx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "conn-proxyless-legacy")
	legacyCtx = syncMock.NewContext(legacyCtx, syncMock.New(zap.NewNop()))
	if err := h.parseFinalHTTP(legacyCtx, &FinalHTTP{
		Req:              rawReq,
		Resp:             rawResp,
		ReqTimestampMock: t0,
		ResTimestampMock: t0.Add(5 * time.Millisecond),
	}, 443, make(chan *models.Mock, 1), opts, func(m *models.Mock) {
		recLegacy = m
	}); err != nil || recLegacy == nil {
		t.Fatalf("parseFinalHTTP(OutgoingOptions.TLS) = (%v, %v)", recLegacy, err)
	}
	if recLegacy.Spec.Metadata["tls"] != "true" {
		t.Fatalf("parseFinalHTTP(OutgoingOptions.TLS) did not stamp meta[tls]=true: %+v", recLegacy.Spec.Metadata)
	}

	kV2, okV2 := models.ReadThroughKey(recV2)
	kLeg, okLeg := models.ReadThroughKey(recLegacy)
	if !okV2 || !okLeg || kV2 != kLeg {
		t.Fatalf("proxyless keys drifted: v2=(%q,%v) legacy=(%q,%v)", kV2, okV2, kLeg, okLeg)
	}

	// Replay over MITM TLS (DstCfg.TLSCfg != nil) must match; plaintext HTTP must miss.
	db := &rtTestDb{
		mockMemDb:   &mockMemDb{deleteFilteredReturn: true},
		readThrough: []*models.Mock{recV2},
	}
	appTLS, proxyTLS := tcpPair(t)
	ctxTLS, cancelTLS := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTLS()
	go func() {
		_ = h.decodeHTTP(ctxTLS, rawReq, proxyTLS, &models.ConditionalDstCfg{
			Addr:   "auth.internal:443",
			Port:   443,
			TLSCfg: &tls.Config{MinVersion: tls.VersionTLS12},
		}, db, models.OutgoingOptions{})
	}()
	gotTLS := readUntil(appTLS, `{"keys":["proxyless"]}`, 2*time.Second)
	_ = appTLS.Close()
	if !strings.HasPrefix(gotTLS, "HTTP/1.1 200") || !strings.HasSuffix(gotTLS, `{"keys":["proxyless"]}`) {
		t.Fatalf("MITM TLS replay missed proxyless-recorded HTTPS read-through mock: %q", gotTLS)
	}

	appPlain, proxyPlain := tcpPair(t)
	ctxPlain, cancelPlain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPlain()
	go func() {
		_ = h.decodeHTTP(ctxPlain, rawReq, proxyPlain, &models.ConditionalDstCfg{
			Addr: "auth.internal:80",
			Port: 80,
		}, db, models.OutgoingOptions{})
	}()
	gotPlain := readUntil(appPlain, "\r\n\r\n", 2*time.Second)
	_ = appPlain.Close()
	if !strings.HasPrefix(gotPlain, "HTTP/1.1 502") {
		t.Fatalf("plaintext HTTP replay must miss proxyless HTTPS read-through mock, got: %q", gotPlain)
	}
}

type placedRTTestDb struct {
	*placedDb
	readThrough []*models.Mock
}

func (d *placedRTTestDb) MatchReadThrough(live *models.Mock) (*models.Mock, bool) {
	lk, ok := models.ReadThroughRequestKey(live)
	if !ok {
		return nil, false
	}
	for _, rt := range d.readThrough {
		if !models.IsReadThrough(rt) {
			continue
		}
		if rk, ok := models.ReadThroughKey(rt); ok && lk == rk {
			return rt, true
		}
	}
	return nil, false
}

// TestHTTPReadThrough_NeverHeldByAnswerRelease verifies that an HTTP answer
// served from the session read-through tier is written immediately even when
// an HTTP AnswerRelease is registered and the mock's recorded [reqTime, resTime]
// interval spans the current replay point (which would otherwise hold the
// answer in heldAnswerOf until test timeout).
func TestHTTPReadThrough_NeverHeldByAnswerRelease(t *testing.T) {
	registerStandIn(t)
	rt := singleAnswer(t, releaseAt(45))
	rt.Name = "mock-rt-spanning"
	if !models.MarkReadThrough(rt) {
		t.Fatal("MarkReadThrough failed for singleAnswer")
	}

	// Sanity check: heldAnswerOf WOULD hold this mock if repeat=false.
	h := &HTTP{Logger: zap.NewNop()}
	checkDb := newPlacedDb()
	if held, err := h.heldAnswerOf(rt, releaseRequest(t, releaseCall), checkDb, false, nil); err != nil || held == nil {
		t.Fatalf("precondition failed: expected heldAnswerOf(repeat=false) to hold spanning mock, got held=%v err=%v", held, err)
	}

	// Stage ONLY in read-through tier (empty per-test pool) while replay sits at
	// test-1 open (releaseAt(5) due, releaseAt(45) NOT due) and NEVER advance.
	db := &placedRTTestDb{
		placedDb:    newPlacedDb(),
		readThrough: []*models.Mock{rt},
	}
	app, _ := serveHeld(t, db, nil)
	got := readUntil(app, `{"state":"done"}`, 2*time.Second)
	if !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.HasSuffix(got, `{"state":"done"}`) {
		t.Fatalf("read-through HTTP answer must return immediately without being held by AnswerRelease, got: %q", got)
	}
	if db.waiting() != 0 {
		t.Fatalf("read-through HTTP answer must never wait on RunPlaceChanged, waiting=%d", db.waiting())
	}
}
