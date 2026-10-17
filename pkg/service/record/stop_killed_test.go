package record

import (
	"context"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// confStore is a test-set config store holding one config, whose Read also
// hands out the set's secret, as the yaml store's does.
type confStore struct {
	conf    *models.TestSet
	written *models.TestSet
}

func (c *confStore) Read(context.Context, string) (*models.TestSet, error) {
	conf := *c.conf
	conf.Secret = map[string]interface{}{"token": "s3cr3t"}
	return &conf, nil
}

func (c *confStore) Write(_ context.Context, _ string, conf *models.TestSet) error {
	c.written = conf
	return nil
}

// updatingStore is confStore with the yaml store's ReadForUpdate, which reads
// no secret.
type updatingStore struct{ confStore }

func (u *updatingStore) ReadForUpdate(context.Context, string) (*models.TestSet, error) {
	conf := *u.conf
	return &conf, nil
}

// A recording whose stop had to kill the app says so in its test-set's
// config, under the proven switch: the rest of the config is kept, and no
// secret is written into it. A store that cannot read a config for update
// is not written (its Read cannot tell a config it could not parse from none,
// and would have it replaced). With the switch off nothing is written.
func TestARecordingWhoseStopKilledSaysSoInItsConfig(t *testing.T) {
	const why = "keploy stopped the application at the recording's end and a process of it had not exited 3s later (the grace keploy's stop gives each of its process groups), so keploy killed it"
	recorded := func() *models.TestSet {
		return &models.TestSet{PostScript: "make check", Metadata: map[string]interface{}{"join": "ui"}}
	}
	t.Setenv(provenswitch.Env, "1")
	store := &updatingConf{&updatingStore{confStore{conf: recorded()}}}
	r := &Recorder{logger: zap.NewNop(), testSetConf: store}
	r.markStopKilled(context.Background(), "test-set-0", why)
	got := store.got()
	if got == nil || got.RecordStopKilled != why {
		t.Fatalf("the config written is %+v; want the stop's kill in it", got)
	}
	if got.PostScript != "make check" || got.Metadata["join"] != "ui" {
		t.Errorf("the config's other fields were not kept: %+v", got)
	}
	if got.Secret != nil {
		t.Errorf("a secret was written into the config: %v", got.Secret)
	}
	plain := &plainConf{&confStore{conf: recorded()}}
	(&Recorder{logger: zap.NewNop(), testSetConf: plain}).markStopKilled(context.Background(), "test-set-0", why)
	if plain.got() != nil {
		t.Fatalf("a store with no read for update was written: %+v", plain.got())
	}
	t.Setenv(provenswitch.Env, "")
	off := &confStore{conf: recorded()}
	(&Recorder{logger: zap.NewNop(), testSetConf: off}).markStopKilled(context.Background(), "test-set-0", why)
	if off.written != nil {
		t.Fatalf("with the switch off the config was written: %+v", off.written)
	}
}

// The app's run says why its stop killed it; only a run that says so is kept.
func TestTheAppsRunsStopKillIsKept(t *testing.T) {
	var kept atomic.Pointer[string]
	noteStopKilled(&kept, models.AppError{AppErrorType: models.ErrCtxCanceled})
	if kept.Load() != nil {
		t.Fatal("a run that killed nothing was kept as a kill")
	}
	noteStopKilled(&kept, models.AppError{AppErrorType: models.ErrCtxCanceled, StopKilled: "killed"})
	if why := kept.Load(); why == nil || *why != "killed" {
		t.Fatalf("a run's kill was not kept: %v", why)
	}
}

type updatingConf struct{ *updatingStore }

func (u *updatingConf) got() *models.TestSet { return u.written }

type plainConf struct{ *confStore }

func (p *plainConf) got() *models.TestSet { return p.written }
