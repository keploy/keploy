package testset

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"time"
)

type registryState struct {
	Mock string `yaml:"mock,omitempty"`
	App  string `yaml:"app,omitempty"`
	User string `yaml:"user,omitempty"`
}

type extendedTestSet struct {
	models.TestSet `yaml:",inline"`
	Registry       *registryState `yaml:"mockRegistry,omitempty"`
}

func (ts *extendedTestSet) WithoutSecrets() *extendedTestSet {
	if ts == nil {
		return &extendedTestSet{}
	}

	testSetCopy := *ts
	testSetCopy.Secret = nil
	return &testSetCopy
}

func TestReadInjectsSecretsWithoutConfigForBaseTestSet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"
	testSetDir := filepath.Join(root, testSetID)
	require.NoError(t, os.MkdirAll(testSetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(testSetDir, "secret.yaml"), []byte("token: abc123\n"), 0o644))

	db := New[*models.TestSet](zap.NewNop(), root)
	cfg, err := db.Read(context.Background(), testSetID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, map[string]interface{}{"token": "abc123"}, cfg.Secret)
	require.Empty(t, cfg.Template)
	require.Empty(t, cfg.Metadata)
}

func TestReadUsesZeroExtendedTypeOnMalformedConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"
	testSetDir := filepath.Join(root, testSetID)
	require.NoError(t, os.MkdirAll(testSetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(testSetDir, "config.yaml"), []byte("template: [\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(testSetDir, "secret.yaml"), []byte("token: abc123\n"), 0o644))

	db := New[*extendedTestSet](zap.NewNop(), root)
	cfg, err := db.Read(context.Background(), testSetID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, map[string]interface{}{"token": "abc123"}, cfg.Secret)
	require.Nil(t, cfg.Registry)
	require.Empty(t, cfg.Template)
	require.Empty(t, cfg.Metadata)
}

func TestReadSampleConfigCompatibilityForExtendedTestSet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"
	testSetDir := filepath.Join(root, testSetID)
	require.NoError(t, os.MkdirAll(testSetDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(testSetDir, "config.yaml"), []byte(`
preScript: ''
postScript: ''
appCommand: ''
template: {}
mockRegistry:
  mock: e227caac693a155a767bf231f1814b237de77c2b39a5ee3c2c1eb65daefbd89b
  app: user-management-service
metadata:
  name: student_login
  scenario-description: 'VST-1: New student registration'
  secret_versions:
    student_login: c08bfa5a-31df-4479-9ad7-4a25e07ee117
`), 0o644))

	db := New[*extendedTestSet](zap.NewNop(), root)
	cfg, err := db.Read(context.Background(), testSetID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, "", cfg.PreScript)
	require.Equal(t, "", cfg.PostScript)
	require.Equal(t, "", cfg.AppCommand)
	require.Empty(t, cfg.Template)
	require.NotNil(t, cfg.Registry)
	require.Equal(t, "e227caac693a155a767bf231f1814b237de77c2b39a5ee3c2c1eb65daefbd89b", cfg.Registry.Mock)
	require.Equal(t, "user-management-service", cfg.Registry.App)
	require.Equal(t, "", cfg.Registry.User)
	require.Equal(t, "student_login", cfg.Metadata["name"])
	require.Equal(t, "VST-1: New student registration", cfg.Metadata["scenario-description"])
}

func TestWriteStripsSecretsForBaseTestSet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"

	db := New[*models.TestSet](zap.NewNop(), root)
	cfg := &models.TestSet{
		PreScript: "echo hi",
		Secret: map[string]interface{}{
			"token": "abc123",
		},
		Metadata: map[string]interface{}{
			"name": "student_login",
		},
	}

	require.NoError(t, db.Write(context.Background(), testSetID, cfg))

	raw, err := os.ReadFile(filepath.Join(root, testSetID, "config.yaml"))
	require.NoError(t, err)
	content := string(raw)
	require.NotContains(t, content, "secret:")
	require.Contains(t, content, "preScript: echo hi")
	require.Contains(t, content, "metadata:")
}

func TestWriteStripsSecretsForExtendedTestSet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"

	db := New[*extendedTestSet](zap.NewNop(), root)
	cfg := &extendedTestSet{
		TestSet: models.TestSet{
			Template: map[string]interface{}{},
			Secret: map[string]interface{}{
				"token": "abc123",
			},
			Metadata: map[string]interface{}{
				"name": "student_login",
			},
		},
		Registry: &registryState{
			Mock: "hash-value",
			App:  "user-management-service",
			User: "demo-user",
		},
	}

	require.NoError(t, db.Write(context.Background(), testSetID, cfg))

	raw, err := os.ReadFile(filepath.Join(root, testSetID, "config.yaml"))
	require.NoError(t, err)
	content := string(raw)
	require.NotContains(t, content, "secret:")
	require.True(t, strings.Contains(content, "template: {}") || strings.Contains(content, "template:\n    {}"))
	require.Contains(t, content, "mockRegistry:")
	require.Contains(t, content, "mock: hash-value")
	require.Contains(t, content, "app: user-management-service")
	require.Contains(t, content, "user: demo-user")
}

/* ------------------------------------------------------------------ */
/*  Metadata survives the round trip, and survives a later write       */
/* ------------------------------------------------------------------ */

/*
TestMetadataSurvivesTheRealDiskRoundTrip is the persistence claim, executed.

models.UIJoinAnnotation is designed to ride in TestSet.Metadata because
that map is "already a free-form map persisted to keploy/<id>/config.yaml"
— and every test for it round-trips in-memory structs and codecs. The one
load-bearing claim, that the thing survives the actual file, was asserted
only in a comment.
*/
func TestMetadataSurvivesTheRealDiskRoundTrip(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"
	db := New[*models.TestSet](zap.NewNop(), root)
	ctx := context.Background()

	recorded := &models.TestSet{}
	require.NoError(t, models.SetUIJoinAnnotation(recorded, &models.UIJoinAnnotation{
		SpecVersion:      models.UIJoinSpecVersion,
		CaptureID:        "cap_01HZY",
		SessionNonce:     "n_7f3a9c",
		T0WallMs:         1789000000000,
		T1WallMs:         1789000060000,
		IngressPorts:     []int{8080, 8443},
		AppOrigins:       []string{"http://localhost:3000"},
		CanonicalKeySpec: "canonical-key.v1",
	}))
	require.NoError(t, db.Write(ctx, testSetID, recorded))

	readBack, err := db.Read(ctx, testSetID)
	require.NoError(t, err)
	got, err := models.GetUIJoinAnnotation(readBack)
	require.NoError(t, err, "the annotation did not survive config.yaml")
	require.Equal(t, "cap_01HZY", got.CaptureID)
	require.Equal(t, []int{8080, 8443}, got.IngressPorts)
	require.Equal(t, []string{"http://localhost:3000"}, got.AppOrigins)
}

func TestWriteWarnsWhenItWouldDropAnyField(t *testing.T) {
	// The loss is silent by nature: a test-set with no metadata, no
	// appCommand and no scripts is the ordinary case, so nothing
	// downstream can tell "never had any" from "had some until the last
	// write".
	//
	// FIELD-GENERIC. The first version of this guard checked Metadata
	// alone — the field that had been noticed — and stayed silent for
	// AppCommand, which the very same struct literal also dropped.
	ctx := context.Background()
	root := t.TempDir()
	core, logs := observer.New(zap.WarnLevel)
	db := New[*models.TestSet](zap.New(core), root)

	require.NoError(t, db.Write(ctx, "ts", &models.TestSet{
		PreScript:  "echo pre",
		PostScript: "echo post",
		AppCommand: "npm start",
		Metadata:   map[string]interface{}{"team": "payments"},
	}))
	require.Equal(t, 0, logs.Len(), "the first write has nothing to drop")

	require.NoError(t, db.Write(ctx, "ts", &models.TestSet{
		Template: map[string]interface{}{"token": "{{string .token}}"},
	}))

	entries := logs.FilterMessageSnippet("drops fields already on disk").All()
	require.Len(t, entries, 1, "a silent deletion must be audible")
	dropped := entries[0].ContextMap()["dropped"]
	require.ElementsMatch(t,
		[]string{"AppCommand", "Metadata", "PostScript", "PreScript"},
		dropped,
		"every dropped field must be named, not just the one someone noticed")

	// Carrying everything forward stays quiet, so the warning does not
	// become noise that people learn to ignore.
	logs.TakeAll()
	require.NoError(t, db.Write(ctx, "ts", &models.TestSet{
		PreScript:  "echo pre",
		PostScript: "echo post",
		AppCommand: "npm start",
		Metadata:   map[string]interface{}{"team": "payments"},
		Template:   map[string]interface{}{"token": "{{string .token}}"},
	}))
	require.Equal(t, 0, logs.FilterMessageSnippet("drops fields").Len())
}

func TestTheDropWarningSurvivesACancelledContext(t *testing.T) {
	// The guard read used the CALLER'S context, so on an already-cancelled
	// one the Read failed, the warning was suppressed, and the write went
	// ahead and destroyed the data — silent in exactly the case it exists
	// for.
	root := t.TempDir()
	core, logs := observer.New(zap.WarnLevel)
	db := New[*models.TestSet](zap.New(core), root)

	require.NoError(t, db.Write(context.Background(), "ts", &models.TestSet{
		Metadata: map[string]interface{}{"team": "payments"},
	}))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = db.Write(cancelled, "ts", &models.TestSet{
		Template: map[string]interface{}{"token": "x"},
	})

	require.Equal(t, 1, logs.FilterMessageSnippet("drops fields already on disk").Len(),
		"the warning must not depend on a context the caller already cancelled")
}

func TestTheDropWarningIgnoresSecrets(t *testing.T) {
	// Write strips Secret on purpose, so reporting it would fire on every
	// single write and turn the warning into noise.
	//
	// THE SECRET HAS TO BE ON DISK. Seeding it through Write does not
	// work — Write strips it, secret.yaml is never created, and the guard
	// read never sees a Secret to skip. The earlier version of this test
	// did exactly that, so deleting the skip left it passing: it asserted
	// nothing.
	ctx := context.Background()
	root := t.TempDir()
	testSetDir := filepath.Join(root, "ts")
	require.NoError(t, os.MkdirAll(testSetDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(testSetDir, "secret.yaml"), []byte("token: abc123\n"), 0o644))

	core, logs := observer.New(zap.WarnLevel)
	db := New[*models.TestSet](zap.New(core), root)

	require.NoError(t, db.Write(ctx, "ts", &models.TestSet{
		Metadata: map[string]interface{}{"team": "payments"},
	}))
	// Precondition: Read really does inject the secret, so there IS one
	// to be skipped.
	seeded, err := db.Read(ctx, "ts")
	require.NoError(t, err)
	require.NotEmpty(t, seeded.Secret, "precondition: the guard read must see a Secret")

	logs.TakeAll()
	require.NoError(t, db.Write(ctx, "ts", &models.TestSet{
		Metadata: map[string]interface{}{"team": "payments"},
	}))
	require.Equal(t, 0, logs.FilterMessageSnippet("drops fields").Len(),
		"Secret is stripped by Write on purpose; reporting it would fire on every write")
}

func TestTheDropWarningReadsQuietly(t *testing.T) {
	// The guard reads through the normal path, which LOGS AN ERROR for a
	// malformed config.yaml — on a write that succeeds and repairs the
	// file. Silencing that read was one of the fixes and nothing pinned
	// it.
	ctx := context.Background()
	root := t.TempDir()
	testSetDir := filepath.Join(root, "ts")
	require.NoError(t, os.MkdirAll(testSetDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(testSetDir, "config.yaml"), []byte("template: [\n"), 0o644))

	core, logs := observer.New(zap.ErrorLevel)
	db := New[*models.TestSet](zap.New(core), root)

	require.NoError(t, db.Write(ctx, "ts", &models.TestSet{
		Metadata: map[string]interface{}{"team": "payments"},
	}))
	require.Equal(t, 0, logs.Len(),
		"repairing a malformed config must not emit an error on the way")
}

func TestTheDropGuardSurvivesAValueTypedConfig(t *testing.T) {
	// Db[T] is deliberately generic — withoutSecrets[T] and
	// extendedTestSet exist so other T's plug in. Recursing through
	// .Addr().Interface() needed an ADDRESSABLE value, which only holds
	// for a pointer T, so a value-typed one panicked from inside Write:
	// a guard added to make writes safer crashed the write.
	// A VALUE-TYPED T. The previous version used *extendedTestSet — a
	// POINTER — and reflect.Indirect of a pointer is addressable, so the
	// .Addr() call never panicked and require.NotPanics asserted nothing
	// while the comment claimed it covered the value case.
	ctx := context.Background()
	core, logs := observer.New(zap.WarnLevel)
	db := New[valueExtended](zap.New(core), t.TempDir())

	require.NotPanics(t, func() {
		require.NoError(t, db.Write(ctx, "ts", valueExtended{
			TestSet:  models.TestSet{Metadata: map[string]interface{}{"a": "b"}},
			Registry: &registryState{Mock: "m"},
		}))
		require.NoError(t, db.Write(ctx, "ts", valueExtended{}))
	})

	// AND IT STILL REPORTS. The fields live inside an EMBEDDED
	// models.TestSet, so without the anonymous-field branch the walker
	// compares the embedded struct as one opaque value and names nothing
	// — a silent drop through the very shape the guard exists for.
	entries := logs.FilterMessageSnippet("drops fields already on disk").All()
	require.Len(t, entries, 1, "a drop inside an embedded struct must still be audible")
	require.ElementsMatch(t,
		[]string{"Metadata", "Registry"},
		entries[0].ContextMap()["dropped"],
		"fields reached through an embedded struct must be named individually")
}

// valueExtended holds models.TestSet BY VALUE, which is what made the
// reflection walker panic: reflect.Indirect of a non-pointer is not
// addressable, so recursing through .Addr().Interface() crashed the
// write it was added to protect. Db[T] is deliberately generic, so a
// value T is a supported shape.
type valueExtended struct {
	models.TestSet `yaml:",inline"`
	Registry       *registryState `yaml:"mockRegistry,omitempty"`
}

func (ts valueExtended) WithoutSecrets() valueExtended {
	ts.Secret = nil
	return ts
}

/* ------------------------------------------------------------------ */
/*  ReadForUpdate                                                      */
/* ------------------------------------------------------------------ */
/*
 * THE METHOD THIS TYPE'S WHOLE UPDATE PATH RESTS ON HAD NO TESTS.
 *
 * ReadForUpdate exists because Read swallows an unmarshal error and
 * returns a zero value — correct for a reader, catastrophic for a writer,
 * because Write replaces the whole document. It was written to report
 * the malformed file instead of pretending it was empty.
 *
 * Nothing exercised it. An independent reviewer reintroduced the exact
 * bug it was written to prevent — returning the zero value on an
 * unmarshal error — and the entire repo stayed green. The helper that
 * calls it was tested against a fake store; the real method was not
 * called by anything, anywhere.
 */

// readForUpdateConfig is the shape these tests round-trip.
func writeTestSetConfig(t *testing.T, root, testSetID, body string) string {
	t.Helper()
	dir := filepath.Join(root, testSetID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func TestReadForUpdateReturnsTheStoredConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestSetConfig(t, root, "ts", "preScript: echo pre\npostScript: echo post\nappCommand: npm start\n")

	db := New[*models.TestSet](zap.NewNop(), root)
	cfg, err := db.ReadForUpdate(context.Background(), "ts")
	require.NoError(t, err)
	require.Equal(t, "echo pre", cfg.PreScript)
	require.Equal(t, "echo post", cfg.PostScript)
	require.Equal(t, "npm start", cfg.AppCommand)
}

func TestReadForUpdateTreatsAMissingConfigAsEmpty(t *testing.T) {
	t.Parallel()

	// A test-set recorded before config.yaml existed legitimately has
	// none, and writing a fresh one is correct.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "ts"), 0o755))

	db := New[*models.TestSet](zap.NewNop(), root)
	cfg, err := db.ReadForUpdate(context.Background(), "ts")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, "", cfg.PreScript)
}

func TestReadForUpdateRefusesAMalformedConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Valid YAML, wrong shape: a sequence where a mapping is expected.
	writeTestSetConfig(t, root, "ts", "- this\n- is\n- a list\n")

	db := New[*models.TestSet](zap.NewNop(), root)
	_, err := db.ReadForUpdate(context.Background(), "ts")
	require.Error(t, err)
	require.Contains(t, err.Error(), "could not be parsed")
	require.Contains(t, err.Error(), "ts")
}

func TestReadForUpdateRefusesAnUnreadableConfig(t *testing.T) {
	t.Parallel()

	/*
	 * THE READ ERROR THE FIRST VERSION SWALLOWED.
	 *
	 * `if err != nil { return newValue[T](), nil }` carried the comment
	 * "No file." — but ReadFileAny returns fs.ErrNotExist ONLY for a
	 * genuinely missing file, and the raw error for everything else:
	 * EACCES, EISDIR, EIO, a cancelled context. Every one of those was
	 * reported as "there was nothing on disk", so the caller wrote a
	 * fresh config over a file it had never read.
	 *
	 * Reproduced before it was fixed: a root-owned or mode-0000
	 * config.yaml — what a `sudo keploy record` or a Docker run leaves
	 * behind — had preScript, postScript, appCommand and metadata
	 * replaced with empty strings, and WriteTemplatedConfig returned nil.
	 * os.Rename needs write permission on the DIRECTORY, not the file, so
	 * nothing stopped it. The drop guard was equally blind: it re-reads
	 * through the same path, got the same zero value, and concluded
	 * nothing had been dropped.
	 *
	 * The metadata block erased here is where UIJoinAnnotation lives.
	 */
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 is still readable")
	}

	root := t.TempDir()
	path := writeTestSetConfig(t, root, "ts", "preScript: echo pre\n")
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	db := New[*models.TestSet](zap.NewNop(), root)
	_, err := db.ReadForUpdate(context.Background(), "ts")
	require.Error(t, err, "an unreadable config must not read as an absent one")
	require.Contains(t, err.Error(), "ts")
}

func TestReadForUpdateRefusesWhenTheConfigIsADirectory(t *testing.T) {
	t.Parallel()

	// EISDIR takes the same path as EACCES and is easier to arrange in
	// environments where permissions do not bite.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "ts", "config.yaml"), 0o755))

	db := New[*models.TestSet](zap.NewNop(), root)
	_, err := db.ReadForUpdate(context.Background(), "ts")
	require.Error(t, err, "a config.yaml that is a directory must not read as absent")
}

func TestReadForUpdateRefusesACancelledContext(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestSetConfig(t, root, "ts", "preScript: echo pre\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	db := New[*models.TestSet](zap.NewNop(), root)
	_, err := db.ReadForUpdate(ctx, "ts")
	require.Error(t, err, "a cancelled read must not report an empty config")
	// AND IT SAYS WHAT HAPPENED. Wrapping this in "your config.yaml could
	// not be read and is about to lose every field" made every Ctrl-C'd
	// --updateTemplate run log a corruption warning at ERROR about a file
	// that is perfectly fine.
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "could not be read",
		"a cancelled run must not be reported as a corrupt config")
}

func TestReadForUpdateReportsADeadlineTheSameWay(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestSetConfig(t, root, "ts", "preScript: echo pre\n")

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	db := New[*models.TestSet](zap.NewNop(), root)
	_, err := db.ReadForUpdate(ctx, "ts")
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotContains(t, err.Error(), "could not be read")
}

func TestTheDropGuardSaysSoWhenItCannotLook(t *testing.T) {
	t.Parallel()

	/*
	 * The guard read through Db.Read, which swallows an unmarshal error
	 * and returns the zero value — so it compared the incoming config
	 * against an empty one, found nothing dropped, and said nothing. A
	 * silent guard reads exactly like a clean one.
	 */
	root := t.TempDir()
	writeTestSetConfig(t, root, "ts", "- not\n- a mapping\n")

	core, logs := observer.New(zap.WarnLevel)
	db := New[*models.TestSet](zap.New(core), root)
	db.warnIfDroppingFields(context.Background(), "ts", &models.TestSet{PreScript: "x"})

	entries := logs.All()
	require.Len(t, entries, 1, "the guard must report that it could not look")
	require.Contains(t, entries[0].Message, "could not check")
}

func TestTheDropGuardStillWarnsOnARealDrop(t *testing.T) {
	t.Parallel()

	// The positive control: the guard's normal job still works, so the
	// test above is not passing because the guard warns about everything.
	root := t.TempDir()
	writeTestSetConfig(t, root, "ts", "preScript: echo pre\nappCommand: npm start\n")

	core, logs := observer.New(zap.WarnLevel)
	db := New[*models.TestSet](zap.New(core), root)
	db.warnIfDroppingFields(context.Background(), "ts", &models.TestSet{PreScript: "echo pre"})

	entries := logs.All()
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Message, "drops fields already on disk")
}

func TestTheDropGuardIsSilentWhenNothingIsDropped(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestSetConfig(t, root, "ts", "preScript: echo pre\n")

	core, logs := observer.New(zap.WarnLevel)
	db := New[*models.TestSet](zap.New(core), root)
	db.warnIfDroppingFields(context.Background(), "ts", &models.TestSet{
		PreScript:  "echo pre",
		AppCommand: "added",
	})

	require.Empty(t, logs.All(), "adding a field is not dropping one")
}
