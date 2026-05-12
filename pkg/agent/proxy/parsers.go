package proxy

// In-tree integrations only. Out-of-tree parsers (aerospike,
// postgres-v3, mongo-v2, http2, grpc, …) ship in the sibling
// keploy/integrations module and are injected at build time by
// the `setup-private-parsers` GitHub Action, which writes
// extraparsers.go importing `github.com/keploy/integrations/pkg/parsers`.
// Keeping out-of-tree imports out of this file keeps `keploy/keploy`
// buildable without `setup-private-parsers` having run and without
// a hard dependency on the integrations module in go.mod.
import (
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
)
