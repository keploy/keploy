package http

import (
	"context"
	"io"
	"net/http"

	"go.keploy.io/server/v3/pkg"
)

// newAgentRequest is http.NewRequestWithContext for a URL made from the
// agent's URI. Its error goes through keploy's mask (pkg.ShownAgentURIError):
// url.Parse's names the whole URI, a password in it too. Every request this
// package makes goes through it and doAgent, so no line and no reason can say
// that text unmasked (TestEveryAgentRequestGoesThroughTheMask).
func newAgentRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	return req, pkg.ShownAgentURIError(err)
}

// doAgent is client.Do for a request to the agent. Its error goes through
// keploy's mask: net/http takes out only what url.URL.User holds, and a URI
// with a raw character in its password keeps that password elsewhere in it.
func doAgent(client *http.Client, req *http.Request) (*http.Response, error) {
	res, err := client.Do(req)
	return res, pkg.ShownAgentURIError(err)
}
