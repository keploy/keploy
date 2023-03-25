package regression

import (
	"go.keploy.io/server/pkg/models"
)

// TestCaseReq is a struct for Http API request JSON body.
//
// Deprecated: TestCaseReq is shifted to "pkg/models" pkg. This struct is not
// removed from "http/regression" package because of backward compatibilty.
// Since, earlier versions before v0.8.3 of go-sdk depends on regression package.
type TestCaseReq = models.TestCaseReq

// TestReq is a struct for Http API request JSON body.
//
// Deprecated: TestReq is shifted to "pkg/models" pkg. This struct is not removed from
// "http/regression" package because of backward compatibilty. Since, earlier versions
// before v0.8.3 of go-sdk depends on regression package
type TestReq = models.TestReq
