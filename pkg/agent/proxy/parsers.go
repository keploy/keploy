package proxy

import (
	// import all the integrations
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
)
