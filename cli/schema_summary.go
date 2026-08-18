package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	schemaSummarySvc "go.keploy.io/server/v3/pkg/service/schemasummary"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

const (
	defaultAPIServerURL = "https://api.keploy.io"
	envAPIServerURL     = "KEPLOY_API_URL"
	envToken            = "KEPLOY_TOKEN"
)

func init() {
	Register("schema-summary", SchemaSummary)
}

// SchemaSummary registers `keploy schema-summary` — a thin client over
// api-server's /k8s-proxy/get/schema-summary endpoint that prints OpenAPI
// coverage as a CLI table (covered / partial / uncovered per endpoint
// and per component schema).
//
// The service is constructed inline (no service-factory plumbing) because
// it has no shared dependencies with other commands; flags map 1:1 onto
// schemaSummarySvc.Options.
func SchemaSummary(ctx context.Context, logger *zap.Logger, _ *config.Config, _ ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema-summary",
		Short: "Show OpenAPI schema coverage for a deployment",
		Example: `keploy schema-summary -n default -d apigateway -c my-cluster
  # token can be a user JWT or a PAT (kep_...)
  # api-server defaults to https://api.keploy.io; override with --api-server`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts, err := readFlags(cmd)
			if err != nil {
				utils.LogError(logger, err, "invalid arguments")
				return nil
			}
			svc := schemaSummarySvc.New(logger, opts)
			if err := svc.Run(ctx); err != nil {
				utils.LogError(logger, err, "failed to render schema summary")
				return nil
			}
			return nil
		},
	}

	if err := cmdConfigurator.AddFlags(cmd); err != nil {
		utils.LogError(logger, err, "failed to add schema-summary flags")
		return nil
	}

	return cmd
}

func readFlags(cmd *cobra.Command) (schemaSummarySvc.Options, error) {
	get := func(name string) string {
		v, _ := cmd.Flags().GetString(name)
		return v
	}

	apiURL := get("api-server")
	if apiURL == "" {
		apiURL = os.Getenv(envAPIServerURL)
	}
	if apiURL == "" {
		apiURL = defaultAPIServerURL
	}

	token := get("token")
	if token == "" {
		token = os.Getenv(envToken)
	}
	if token == "" {
		return schemaSummarySvc.Options{}, fmt.Errorf("auth token required (--token or %s env var; accepts user JWT or PAT)", envToken)
	}

	opts := schemaSummarySvc.Options{
		APIServerURL: apiURL,
		Token:        token,
		Namespace:    get("namespace"),
		Deployment:   get("deployment"),
		Cluster:      get("cluster"),
		Release:      get("release"),
	}
	if opts.Namespace == "" || opts.Deployment == "" || opts.Cluster == "" {
		return opts, errors.New("--namespace, --deployment, and --cluster are all required")
	}
	return opts, nil
}
