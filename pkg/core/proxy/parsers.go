
package proxy

import (
	// import all the integrations
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/generic"
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/grpc"
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/http"
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/mongo"
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql"
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/postgres/v1"
	_ "go.keploy.io/server/v2/pkg/core/proxy/integrations/redis"
)
