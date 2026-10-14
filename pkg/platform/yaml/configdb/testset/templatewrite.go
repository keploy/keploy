package testset

import (
	"context"
	"fmt"
	"reflect"

	"go.keploy.io/server/v3/pkg/models"
)

/*
WriteTemplatedConfig stores templatized values WITHOUT dropping anything
else in the test-set config.

WHY A HELPER AND NOT A STRUCT LITERAL. Write marshals the WHOLE
models.TestSet over config.yaml, so a caller that builds a fresh struct
deletes every field it forgets. That is not hypothetical: `keploy test
--updateTemplate` erased `metadata:` for a while, and the literal that
caused it ALSO dropped `appCommand:` — which is read back at replay time
— and tools/templatize.go additionally reset `preScript:` and
`postScript:` to "".

Adding one more field to the literal fixes one more field. Mutating the
config that was READ and writing it back cannot drop a field at all,
including fields added to models.TestSet later. The bug class ends here
rather than being paid down one field at a time.

`existing` is the config as read. A nil one means there was nothing on
disk, and a fresh test-set is then correct.
*/
// ConfigStore is the slice of a test-set config store this needs. Named
// here rather than imported so both service packages can use the helper
// without either importing the other.
type ConfigStore interface {
	// ReadForUpdate reports a config.yaml that did not parse, rather than
	// handing back a zero value that would then be written over it.
	ReadForUpdate(ctx context.Context, testSetID string) (*models.TestSet, error)
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
}

func WriteTemplatedConfig(
	ctx context.Context,
	store ConfigStore,
	testSetID string,
	values map[string]interface{},
) error {
	if store == nil {
		return fmt.Errorf("no test-set config store for test-set %q", testSetID)
	}
	// A nil POINTER inside a non-nil interface. Guarded only for kinds
	// that can be nil: IsNil panics on a struct, and a ConfigStore
	// implemented on a value receiver is ordinary Go.
	if v := reflect.ValueOf(store); v.Kind() == reflect.Ptr ||
		v.Kind() == reflect.Interface || v.Kind() == reflect.Map ||
		v.Kind() == reflect.Slice || v.Kind() == reflect.Func {
		if v.IsNil() {
			return fmt.Errorf("no test-set config store for test-set %q", testSetID)
		}
	}

	// THE HELPER READS. It used to take the caller's `existing` value,
	// which meant a caller could pass nil — one token — and reintroduce
	// the whole data loss with every test still green. There is now no
	// argument to get wrong.
	existing, err := store.ReadForUpdate(ctx, testSetID)
	if err != nil {
		return err
	}

	updated := &models.TestSet{}
	if existing != nil {
		copied := *existing
		updated = &copied
	}
	updated.Template = values

	if err := store.Write(ctx, testSetID, updated); err != nil {
		return fmt.Errorf("failed to write templatized values for test-set %q: %w", testSetID, err)
	}
	return nil
}
