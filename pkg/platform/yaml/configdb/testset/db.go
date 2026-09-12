// Package testset provides functionality for working with keploy testset level configs like templates, post/pre script.
package testset

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// Db is a generic struct to read and write testset config file
type Db[T any] struct {
	logger *zap.Logger
	path   string
	Format yaml.Format
}

type withoutSecrets[T any] interface {
	WithoutSecrets() T
}

func New[T any](logger *zap.Logger, path string) *Db[T] {
	return NewWithFormat[T](logger, path, yaml.FormatYAML)
}

func NewWithFormat[T any](logger *zap.Logger, path string, format yaml.Format) *Db[T] {
	return &Db[T]{
		logger: logger,
		path:   path,
		Format: format,
	}
}

/*
ReadForUpdate reads a config that is about to be REPLACED.

Read deliberately swallows an unmarshal error and hands back a zero value
so secret hydration still has somewhere to land — correct for a reader,
catastrophic for a writer. Write replaces the whole document, so
"read, mutate, write back" over a config.yaml that did not decode writes
the ZERO struct over it: one stray indent in a hand-edited file and the
next `keploy test --updateTemplate` erases preScript, postScript,
appCommand and metadata, with the drop-guard equally blind because it
reads the same zero value.

That is worse than the bug the read-modify-write was introduced to fix,
because a hand-edited config is exactly the one with content worth
keeping. So the update path gets its own read, which reports the
malformed file instead of pretending it was empty.

A MISSING file is not an error: a test-set recorded before config.yaml
existed legitimately has none, and a fresh config is then correct.
*/
func (db *Db[T]) ReadForUpdate(ctx context.Context, testSetID string) (T, error) {
	filePath := filepath.Join(db.path, testSetID)

	var config T
	data, detected, err := yaml.ReadFileAny(ctx, db.logger, filePath, "config", db.Format)
	if err != nil {
		// ONLY A MISSING FILE IS "NOTHING TO READ".
		//
		// This used to map EVERY read error to the empty config, under a
		// comment that said "No file." ReadFileAny is careful to
		// distinguish: it returns the sentinel fs.ErrNotExist when neither
		// format is present, and the raw error for anything else — EACCES,
		// EISDIR, EIO, a cancelled context. Collapsing them meant a
		// config.yaml that could not be READ was rewritten from scratch:
		// preScript, postScript, appCommand and the metadata block (where
		// the UI join annotation lives) replaced with empty values, and no
		// error returned. os.Rename needs write permission on the
		// directory, not the file, so nothing downstream objected, and
		// warnIfDroppingFields was blind for the same reason — it re-reads
		// through this path and got the same zero value.
		//
		// That is precisely the failure this method was added to prevent,
		// reached through the other door.
		if errors.Is(err, fs.ErrNotExist) {
			// A test-set recorded before config.yaml existed legitimately
			// has none. The caller writes a fresh one, correctly.
			return newValue[T](), nil
		}
		// A CANCELLED RUN IS NOT A CORRUPT FILE. Wrapping every error in
		// "your config.yaml could not be read and is about to lose every
		// field" meant that interrupting `keploy test --updateTemplate`
		// logged that at ERROR — telling the user their config is
		// unreadable when in fact they pressed Ctrl-C. The cancellation
		// is returned as itself so the caller can recognise it.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return newValue[T](), err
		}
		return newValue[T](), fmt.Errorf(
			"test-set %q has a config.yaml that could not be read, so it "+
				"cannot be safely rewritten (every field it holds would be "+
				"deleted): %w", testSetID, err)
	}
	if err := yaml.UnmarshalGeneric(detected, data, &config); err != nil {
		return newValue[T](), fmt.Errorf(
			"test-set %q has a config.yaml that could not be parsed, so it "+
				"cannot be safely rewritten (every field not in the new value "+
				"would be deleted): %w", testSetID, err)
	}
	return config, nil
}

func (db *Db[T]) Read(ctx context.Context, testSetID string) (T, error) {
	filePath := filepath.Join(db.path, testSetID)

	var config T

	// Auto-detect format so a testset config recorded in the other format
	// remains readable after a StorageFormat switch.
	data, detected, err := yaml.ReadFileAny(ctx, db.logger, filePath, "config", db.Format)
	if err != nil {
		db.logger.Debug("Config file not found, using default config", zap.String("testSet", testSetID), zap.String("filePath", filePath), zap.Error(err))
		config = newValue[T]()
	} else {
		err := yaml.UnmarshalGeneric(detected, data, &config)
		if err != nil {
			utils.LogError(db.logger, err, "failed to unmarshal test-set config file", zap.String("testSet", testSetID))
			// Don't return early - continue with secret loading even if config is malformed.
			// Use a fresh default value so secret hydration still has somewhere to land.
			config = newValue[T]()
			db.logger.Debug("Using default config due to unmarshal error, continuing with secret loading", zap.String("testSet", testSetID))
		}
	}

	if isNilValue(config) {
		config = newValue[T]()
	}

	// Always try to load secrets, regardless of whether config.yaml existed
	secretValues, err := db.ReadSecret(ctx, testSetID)
	if err != nil {
		db.logger.Debug("Failed to read secret values, continuing without secrets", zap.String("testSet", testSetID), zap.Error(err))
		// Don't return error here - missing secrets shouldn't fail the config loading
		return config, nil
	}

	// Set secrets into the config struct if supported
	secretConfig, ok := any(config).(models.Secret)
	if ok && len(secretValues) > 0 {
		db.logger.Debug("Setting secrets into config", zap.String("testSet", testSetID), zap.Int("secretCount", len(secretValues)))
		secretConfig.SetSecrets(secretValues)
	} else {
		db.logger.Debug("Not setting secrets", zap.String("testSet", testSetID), zap.Bool("configSupportsSecrets", ok), zap.Int("secretCount", len(secretValues)))
	}

	return config, nil
}

/*
warnIfDroppingFields says so when a write is about to delete something.

Write marshals the WHOLE struct over config.yaml, so a caller that builds
a fresh models.TestSet deletes every field it forgets. `keploy test
--updateTemplate` did exactly that — erasing `metadata:` AND
`appCommand:`, while tools/templatize.go additionally reset `preScript:`
and `postScript:` to "" — and the loss was invisible, because a test-set
without any of those is the ordinary case.

FIELD-GENERIC, deliberately. The first version of this checked Metadata
alone, which is the field that had been noticed; it stayed silent for
AppCommand sitting in the same struct literal. A guard that covers only
the bug you already found is not a guard.

It does not PREVENT the drop — clearing a field deliberately is
legitimate and Write cannot tell the two apart — it makes the next one
audible.
*/
func (db *Db[T]) warnIfDroppingFields(ctx context.Context, testSetID string, incoming T) {
	// A CONTEXT THAT CANNOT BE CANCELLED. The guard read used the
	// caller's ctx, so on an already-cancelled one the Read failed, the
	// warning was suppressed, and the write proceeded to destroy the
	// data — the guard was silent in precisely the case it exists for.
	// It also read through the normal path, which LOGS AN ERROR for a
	// malformed config.yaml: a write that succeeds and repairs the file
	// should not emit an error on the way.
	// ReadForUpdate, NOT Read. Two reasons, and the first is the bug:
	//
	//   - Read swallows an unmarshal error and hands back the zero value,
	//     so the guard compared the incoming config against an empty one,
	//     found nothing dropped, and stayed silent — in exactly the case
	//     it exists for.
	//   - Read also LOGS AN ERROR for a malformed config.yaml, and a
	//     write that succeeds and repairs the file should not emit one on
	//     the way. ReadForUpdate returns its complaint instead of logging
	//     it, so this needs no silenced copy of the Db; an earlier version
	//     made one, and it was dead the moment this stopped calling Read.
	existing, err := db.ReadForUpdate(context.WithoutCancel(ctx), testSetID)
	if err != nil {
		// THE GUARD SAYING NOTHING IS NOT THE GUARD SAYING "NOTHING WAS
		// DROPPED". It could not look, and a silent guard reads exactly
		// like a clean one to whoever is reading the logs afterwards.
		db.logger.Warn(
			"could not check whether this test-set config write drops fields; "+
				"the existing config could not be read",
			zap.String("testSet", testSetID),
			zap.Error(err),
		)
		return
	}
	if isNilValue(existing) {
		return
	}

	dropped := droppedFieldNames(reflect.ValueOf(existing), reflect.ValueOf(incoming))
	if len(dropped) == 0 {
		return
	}
	db.logger.Warn(
		"test-set config write drops fields already on disk; read-modify-write instead of building a new struct",
		zap.String("testSet", testSetID),
		zap.Strings("dropped", dropped),
	)
}

/*
droppedFieldNames lists the fields that are set on disk and empty in the
value about to replace it.

Secret is skipped: Write strips it on purpose, so reporting it would make
the warning fire on every single write and become noise people learn to
ignore.
*/

/*
hasContent reports whether a field actually carries something.

IsZero is not the right question for a map: Read decodes an absent
`template:` into a non-nil EMPTY map, which IsZero calls non-zero, so
"the file had no template" and "the write drops the template" looked
identical and every write warned about Template.
*/
func hasContent(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.String:
		return v.Len() > 0
	default:
		return !v.IsZero()
	}
}

func droppedFieldNames(existingV, incomingV reflect.Value) []string {
	// ON reflect.Value, not `any`. Recursing through
	// `.Addr().Interface()` needed an ADDRESSABLE value, which only holds
	// when T is a pointer — so `Db[valueExtended]`, where the embedded
	// models.TestSet is held by value, panicked with
	// "reflect.Value.Addr of unaddressable value" from inside Write. A
	// guard added to make writes safer crashed the write, and Db[T] is
	// deliberately generic.
	before := reflect.Indirect(existingV)
	after := reflect.Indirect(incomingV)
	if before.Kind() != reflect.Struct || after.Kind() != reflect.Struct ||
		before.Type() != after.Type() {
		return nil
	}

	var dropped []string
	for i := range before.NumField() {
		field := before.Type().Field(i)
		if !field.IsExported() || field.Name == "Secret" {
			continue
		}
		if field.Anonymous {
			// An embedded struct: compare its fields, not the struct.
			dropped = append(dropped,
				droppedFieldNames(before.Field(i), after.Field(i))...)
			continue
		}
		if hasContent(before.Field(i)) && !hasContent(after.Field(i)) {
			dropped = append(dropped, field.Name)
		}
	}
	sort.Strings(dropped)
	return dropped
}

func (db *Db[T]) Write(ctx context.Context, testSetID string, config T) error {
	filePath := filepath.Join(db.path, testSetID)

	if isNilValue(config) {
		config = newValue[T]()
	}

	// Strip secrets via the generic withoutSecrets[T] interface so this
	// works for any T that opts in (currently *models.TestSet), instead of
	// hand-rolled type assertions.
	if secretlessConfig, ok := any(config).(withoutSecrets[T]); ok {
		config = secretlessConfig.WithoutSecrets()
	}

	db.warnIfDroppingFields(ctx, testSetID, config)

	data, err := yaml.MarshalGeneric(db.Format, config)
	if err != nil {
		utils.LogError(db.logger, err, "failed to marshal test-set config file", zap.String("testSet", testSetID))
		return err
	}
	err = yaml.WriteFileF(ctx, db.logger, filePath, "config", data, false, db.Format)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write test-set configuration file", zap.String("testSet", testSetID))
		return err
	}

	return nil
}

// ReadSecret reads the secret configuration for a test set
func (db *Db[T]) ReadSecret(ctx context.Context, testSetID string) (map[string]interface{}, error) {
	filePath := filepath.Join(db.path, testSetID)

	secretPath := filepath.Join(filePath, "secret.yaml")
	if _, err := os.Stat(secretPath); os.IsNotExist(err) {
		return make(map[string]interface{}), nil
	}

	data, err := yaml.ReadFile(ctx, db.logger, filePath, "secret")
	if err != nil {
		return nil, err
	}

	var secretConfig map[string]interface{}
	if err := yamlLib.Unmarshal(data, &secretConfig); err != nil {
		utils.LogError(db.logger, err, "failed to unmarshal test-set secret file", zap.String("testSet", testSetID))
		return nil, err
	}

	return secretConfig, nil
}

func newValue[T any]() T {
	var zero T
	typ := reflect.TypeOf(zero)
	if typ == nil {
		return zero
	}

	if typ.Kind() == reflect.Pointer {
		return reflect.New(typ.Elem()).Interface().(T)
	}

	return zero
}

func isNilValue[T any](value T) bool {
	v := reflect.ValueOf(value)
	if !v.IsValid() {
		return true
	}

	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
