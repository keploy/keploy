// Package schemasummary fetches and renders the OpenAPI schema-coverage
// summary returned by the api-server's /k8s-proxy/get/schema-summary
// endpoint. The shape mirrors api-server's models.SchemaSummaryReport so
// any new field on the server side becomes available here by adding one
// field below.
package schemasummary

// Status is the tri-state the engine reports per endpoint / per
// component schema.
type Status string

const (
	StatusCovered   Status = "covered"
	StatusPartial   Status = "partial"
	StatusUncovered Status = "uncovered"
)

// EndpointSummary is one row in the per-endpoint table.
type EndpointSummary struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status Status `json:"status"`
}

// ComponentSchemaSummary is one row in the per-component-schema table.
type ComponentSchemaSummary struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
}

// StatusBreakdown counts endpoints by status — drives the
// "Covered/Partial/Uncovered" header block.
type StatusBreakdown struct {
	Covered   int `json:"covered"`
	Partial   int `json:"partial"`
	Uncovered int `json:"uncovered"`
}

// Report is the body of GET /k8s-proxy/get/schema-summary.
type Report struct {
	Cluster            string                   `json:"cluster"`
	Namespace          string                   `json:"namespace"`
	Deployment         string                   `json:"deployment"`
	AppRelease         string                   `json:"appRelease"`
	LastUpdated        int64                    `json:"lastUpdated"`
	CoveragePercentage float64                  `json:"coveragePercentage"`
	TotalEndpoints     int                      `json:"totalEndpoints"`
	CoveredEndpoints   int                      `json:"coveredEndpoints"`
	StatusBreakdown    StatusBreakdown          `json:"statusBreakdown"`
	Endpoints          []EndpointSummary        `json:"endpoints"`
	Schemas            []ComponentSchemaSummary `json:"schemas"`
}

// apiEnvelope is the {success, error, summary} wrapper api-server uses.
type apiEnvelope struct {
	Success bool    `json:"success"`
	Summary *Report `json:"summary,omitempty"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details string `json:"details"`
	} `json:"error,omitempty"`
}
