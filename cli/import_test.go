package cli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/config"
	toolsSvc "go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// A collection's request that has no response is sent to capture one, and
// one the app refuses fails the import with net/http's error, which quotes
// the request's URL whole: the line that says the import failed names it
// without its query, which holds the collection's api keys and tokens,
// unless KEPLOY_LOG_CONTENT is set.
func TestImportPostman_AFailedImportIsLoggedWithoutTheRequestsQuery(t *testing.T) {
	const canary = "kp-canary-import-5b3e"
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusing := "http://" + l.Addr().String()
	_ = l.Close() // the port now refuses
	dir := t.TempDir()
	t.Chdir(dir)
	collection := filepath.Join(dir, "collection.json")
	if err := os.WriteFile(collection, []byte(`{"info":{"name":"orders","schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},
		"item":[{"name":"list","request":{"method":"GET","url":"`+refusing+`/orders?api_key=`+canary+`"},"response":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logtest.Each(t, func(t *testing.T, on bool) {
		// The import asks before it sends a request with no response.
		answer, err := os.CreateTemp(dir, "stdin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := answer.WriteString("yes\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := answer.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		saved := os.Stdin
		os.Stdin = answer
		t.Cleanup(func() { os.Stdin = saved; _ = answer.Close() })

		logger, logs := logtest.Observe()
		tools := toolsSvc.NewTools(logger, nil, nil, nil, nil, &config.Config{})
		cmd := Import(context.Background(), logger, &config.Config{}, agentSvcFactory{svc: tools}, noFlags{})
		postman, _, err := cmd.Find([]string{"postman"})
		if err != nil {
			t.Fatal(err)
		}
		postman.Flags().String("path", collection, "")
		postman.Flags().String("base-path", "", "")
		postman.Flags().Bool("disable-ansi", true, "")
		if err := postman.RunE(postman, nil); err != nil {
			t.Fatalf("RunE returned %v; the import reports its failure on its line", err)
		}
		logtest.Check(t, logs, canary, on, "failed to import Postman collection")
	})
}
