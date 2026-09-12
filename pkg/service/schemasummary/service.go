package schemasummary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Service is the public surface the CLI command consumes. Mirrors the
// shape of other keploy services (one method per sub-action).
type Service interface {
	Run(ctx context.Context) error
}

// Options bundles every input the command takes. Populated from CLI
// flags by the cli/provider layer.
type Options struct {
	APIServerURL string
	Token        string
	Namespace    string
	Deployment   string
	Cluster      string
	Release      string // optional
	// Out is where the rendered table is written. Defaults to os.Stdout.
	Out io.Writer
}

// schemaSummarySvc is the default implementation. Single-purpose: hit
// the endpoint, parse, render.
type schemaSummarySvc struct {
	logger *zap.Logger
	opts   Options
	client *http.Client
}

func New(logger *zap.Logger, opts Options) Service {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	return &schemaSummarySvc{
		logger: logger,
		opts:   opts,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *schemaSummarySvc) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}

	report, err := s.fetch(ctx)
	if err != nil {
		return err
	}

	_, err = fmt.Fprint(s.opts.Out, Render(*report))
	return err
}

func (s *schemaSummarySvc) validate() error {
	if s.opts.APIServerURL == "" {
		return errors.New("api-server URL required (--api-server or KEPLOY_API_URL)")
	}
	if s.opts.Token == "" {
		return errors.New("auth token required (--token or KEPLOY_TOKEN)")
	}
	if s.opts.Namespace == "" || s.opts.Deployment == "" || s.opts.Cluster == "" {
		return errors.New("--namespace, --deployment, and --cluster are all required")
	}
	return nil
}

func (s *schemaSummarySvc) fetch(ctx context.Context) (*Report, error) {
	q := url.Values{}
	q.Set("namespace", s.opts.Namespace)
	q.Set("deployment", s.opts.Deployment)
	q.Set("clusterName", s.opts.Cluster)
	if s.opts.Release != "" {
		q.Set("appRelease", s.opts.Release)
	}

	target := strings.TrimRight(s.opts.APIServerURL, "/") + "/k8s-proxy/get/schema-summary?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.opts.Token)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			s.logger.Debug("close response body", zap.Error(cerr))
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode response (status=%d): %w", resp.StatusCode, err)
	}

	if !env.Success || env.Summary == nil {
		if env.Error != nil {
			msg := fmt.Sprintf("api %s: %s", env.Error.Code, env.Error.Message)
			if env.Error.Details != "" {
				msg += " — " + env.Error.Details
			}
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("api returned !success (status=%d)", resp.StatusCode)
	}

	return env.Summary, nil
}
