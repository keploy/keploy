package realclient

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// agent notes what it was handed, and answers with err.
type agent struct {
	pushed, kept [][]string
	run          models.RealRun
	err          error
}

func (a *agent) PushRealDeps(_ context.Context, real []string) error {
	a.pushed = append(a.pushed, real)
	return a.err
}

func (a *agent) KeepRealDeps(_ context.Context, real []string) error {
	a.kept = append(a.kept, real)
	return a.err
}

func (a *agent) RealRun(context.Context) (models.RealRun, error) { return a.run, a.err }

func TestCheck(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	require.NoError(t, Check([]string{"localhost:5432"}))
	require.ErrorContains(t, Check([]string{"localhost"}), "keploy.yml mock.real")
	t.Setenv(provenswitch.Env, "")
	require.NoError(t, Check([]string{"localhost"}), "without the switch the key is not read")
}

// Push hands the agent the list, an empty one too; an agent client that
// cannot is an error only for a list that names anything; an agent without
// the route has nothing to clear.
func TestPush(t *testing.T) {
	ctx, log := context.Background(), zap.NewNop()
	t.Setenv(provenswitch.Env, "1")
	a := &agent{}
	require.NoError(t, Push(ctx, log, a, []string{"db:5432"}))
	require.NoError(t, Push(ctx, log, a, nil))
	require.Equal(t, [][]string{{"db:5432"}, nil}, a.pushed)
	require.NoError(t, Push(ctx, log, struct{}{}, nil))
	require.Error(t, Push(ctx, log, struct{}{}, []string{"db:5432"}))
	require.NoError(t, Push(ctx, log, &agent{err: models.ErrNoRealRoute}, nil))
	require.ErrorIs(t, Push(ctx, log, &agent{err: models.ErrNoRealRoute}, []string{"db:5432"}), models.ErrNoRealRoute)
	t.Setenv(provenswitch.Env, "")
	a = &agent{}
	require.NoError(t, Push(ctx, log, a, []string{"db:5432"}))
	require.Empty(t, a.pushed, "nothing is pushed without the switch")
}

// Keep hands a later part of the run the list; a client that cannot keep
// pushes afresh.
func TestKeep(t *testing.T) {
	ctx, log := context.Background(), zap.NewNop()
	t.Setenv(provenswitch.Env, "1")
	a := &agent{}
	require.NoError(t, Keep(ctx, log, a, []string{"db:5432"}))
	require.Equal(t, [][]string{{"db:5432"}}, a.kept)
	require.Empty(t, a.pushed)
	require.NoError(t, Keep(ctx, log, &agent{err: models.ErrNoRealRoute}, nil))
	// A client that cannot keep pushes afresh.
	p := &pushOnlyAgent{}
	require.NoError(t, Keep(ctx, log, p, []string{"db:5432"}))
	require.Equal(t, [][]string{{"db:5432"}}, p.pushed)
	require.Error(t, Keep(ctx, log, &agent{err: errors.New("x")}, []string{"db:5432"}))
}

// Run reads the run's mock.real under the switch with a list; a read that
// fails is an error.
func TestRun(t *testing.T) {
	ctx := context.Background()
	t.Setenv(provenswitch.Env, "1")
	got, err := Run(ctx, &agent{run: models.RealRun{Reached: []string{"db:5432"}}}, []string{"db:5432"})
	require.NoError(t, err)
	require.Equal(t, []string{"db:5432"}, got.Reached)
	got, err = Run(ctx, &agent{}, nil)
	require.NoError(t, err)
	require.Nil(t, got, "no list, nothing read")
	_, err = Run(ctx, &agent{err: errors.New("gone")}, []string{"db:5432"})
	require.Error(t, err)
	_, err = Run(ctx, struct{}{}, []string{"db:5432"})
	require.Error(t, err)
	t.Setenv(provenswitch.Env, "")
	got, err = Run(ctx, &agent{}, []string{"db:5432"})
	require.NoError(t, err)
	require.Nil(t, got)
}

// pushOnlyAgent is an agent client that cannot keep.
type pushOnlyAgent struct{ pushed [][]string }

func (p *pushOnlyAgent) PushRealDeps(_ context.Context, real []string) error {
	p.pushed = append(p.pushed, real)
	return nil
}
func (p *pushOnlyAgent) RealRun(context.Context) (models.RealRun, error) {
	return models.RealRun{}, nil
}
