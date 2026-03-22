package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"go.keploy.io/server/v3/pkg/runregistry"
	"go.keploy.io/server/v3/utils"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func init() {
	Register("runs", Runs)
}

func Runs(_ context.Context, logger *zap.Logger, _ *config.Config, _ ServiceFactory, _ CmdConfigurator) *cobra.Command {

	var runsCmd = &cobra.Command{
		Use:   "runs",
		Short: "List stored test runs",
		RunE: func(cmd *cobra.Command, _ []string) error {

			runs, err := runregistry.ListRuns()
			if err != nil {
				utils.LogError(logger, err, "failed to list runs")
				return nil
			}

			if len(runs) == 0 {
				fmt.Println("No test runs found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tDATE\tTOTAL\tPASSED\tFAILED\tDURATION")

			for _, r := range runs {
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\n",
					r.ID,
					r.Timestamp.Format(time.RFC822),
					r.Total,
					r.Passed,
					r.Failed,
					r.Duration.String(),
				)
			}

			w.Flush()
			return nil
		},
	}

	return runsCmd
}
