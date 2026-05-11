package proxy

import (
	// import all the integrations
	_ "github.com/keploy/integrations/pkg/aerospike"
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
)
