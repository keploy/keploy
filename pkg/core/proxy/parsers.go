//go:build linux

package proxy

import (
	// import all the integrations
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/generic"
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/http"
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql"
)
