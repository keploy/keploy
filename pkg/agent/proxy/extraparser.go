//go:build linux

package proxy

import (
	// This blank import registers the private parsers from the
	// keploy/integrations repository.
	_ "github.com/keploy/integrations/pkg/parsers"
	_ "github.com/keploy/integrations/pkg/postgres/v2/types"
)
