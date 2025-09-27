//go:build linux

package proxy

import (
	// import all the integrations
	_ "go.keploy.io/server/v2/pkg/agent/proxy/integrations/generic"
	_ "go.keploy.io/server/v2/pkg/agent/proxy/integrations/http"
	_ "go.keploy.io/server/v2/pkg/agent/proxy/integrations/mysql"
)
