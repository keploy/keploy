package routes

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// realIncomingSvc streams test cases, and names the mock.real destinations a
// window used.
type realIncomingSvc struct {
	incomingSvc
	used  []string
	asked chan [2]time.Time
	keeps *[]bool
}

func (s realIncomingSvc) KeepRealUses(keep bool) { *s.keeps = append(*s.keeps, keep) }

func (s realIncomingSvc) RealUsedDuring(from, to time.Time) []string {
	s.asked <- [2]time.Time{from, to}
	return s.used
}

// streamedCases reads every test case a HandleIncoming stream carried, whole.
func streamedCases(t *testing.T, resp *http.Response) []models.TestCase {
	t.Helper()
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	require.NoError(t, err)
	mr := multipart.NewReader(resp.Body, params["boundary"])
	var out []models.TestCase
	for {
		p, err := mr.NextPart()
		if err != nil {
			return out
		}
		if strings.Contains(p.Header.Get("Content-Disposition"), `name="metadata"`) {
			var tc models.TestCase
			b, _ := io.ReadAll(p)
			require.NoError(t, json.Unmarshal(b, &tc))
			out = append(out, tc)
		}
	}
}

// Under the proven switch each recorded test case is streamed with the
// mock.real destinations its request used, by its window; without it the
// agent is not asked and the test case carries none.
func TestHandleIncomingNamesATestCasesRealDestinations(t *testing.T) {
	for _, on := range []bool{true, false} {
		if on {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "")
		}
		tcs := make(chan *models.TestCase, 1)
		keeps := &[]bool{}
		svc := realIncomingSvc{incomingSvc: incomingSvc{tc: tcs}, used: []string{"db:5432"}, asked: make(chan [2]time.Time, 1), keeps: keeps}
		a := &Agent{logger: zap.NewNop(), svc: svc}
		srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
		body, _ := json.Marshal(models.IncomingReq{})
		resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		req := newStretch()
		tcs <- &models.TestCase{Name: "tc", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: req}, HTTPResp: models.HTTPResp{Timestamp: req.Add(time.Millisecond)}}
		close(tcs)
		got := streamedCases(t, resp)
		_ = resp.Body.Close()
		srv.Close()
		require.Len(t, got, 1)
		if !on {
			require.Nil(t, got[0].Real)
			require.Empty(t, svc.asked)
			require.Empty(t, *keeps, "without the switch no use is kept")
			continue
		}
		require.Equal(t, []bool{true, false}, *keeps, "uses are kept while the stream is open, and no longer after")
		require.Equal(t, []string{"db:5432"}, got[0].Real)
		w := <-svc.asked
		require.True(t, w[0].Equal(req) && w[1].Equal(req.Add(time.Millisecond)), "asked for %v", w)
	}
}
