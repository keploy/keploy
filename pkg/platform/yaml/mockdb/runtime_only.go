package mockdb

import (
	"reflect"

	"go.keploy.io/server/v3/pkg/models"
)

// runtimeOnlyMockFields is the index path, in models.Mock, of every field
// clearRuntimeOnly zeroes (runtimeOnlyPaths).
var runtimeOnlyMockFields = runtimeOnlyPaths(reflect.TypeOf(models.Mock{}))

// runtimeOnlyPaths returns the index path of every exported field of t, and of
// the structs t holds by value at any depth, that is tagged "-" for both json
// and bson. Unexported fields are skipped: no encoder writes them, and
// reflection cannot set them. A struct reached through a pointer, slice or map
// is not walked.
func runtimeOnlyPaths(t reflect.Type) [][]int {
	var paths [][]int
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("json") == "-" && f.Tag.Get("bson") == "-" {
			paths = append(paths, []int{i})
			continue
		}
		if f.Type.Kind() == reflect.Struct {
			for _, sub := range runtimeOnlyPaths(f.Type) {
				paths = append(paths, append([]int{i}, sub...))
			}
		}
	}
	return paths
}

// clearRuntimeOnly zeroes every field of m, and of the structs m holds by
// value (TestModeInfo, Spec), tagged "-" for both json and bson. Those tags
// mark a field runtime-only: the YAML and JSON stores never write it, and a
// read starts it at zero. encoding/gob ignores struct tags, so the gob writer
// clears them on its copy before it encodes it, and the gob reader clears them
// on every mock it decodes. Whatever the storage format, a recording then
// reads back the same. A field added with those tags is cleared with no change
// here.
//
// What a gob file that kept them would do:
//   - Lifetime and LifetimeDerived: DeriveLifetime would short-circuit on the
//     reloaded flag instead of re-deriving, so the same recording would
//     replay into a different tier depending on the storage format: gob
//     would keep the recorder's per-test tag, while yaml drops it and the lax
//     kind-fallback promotes the mock to session. A call two tests depend on
//     would then be consumed by the first and missing for the second.
//   - Consume: DeriveLifetime sets it only on its cursor branches, so a value
//     read from a file would survive the re-derivation.
//   - SourcePID and StartRef: the process that made the call and the
//     in-process reference of the start it came from, hints the agent hands
//     the recorder. Start, the start's recorded key, is kept.
//   - IsStartup, ServedIn, StagedSet, Concurrent and LoadSeq: the
//     recorder's buffer and the replay's load, staging and serving set them
//     on live mocks; a file would carry a past run's values.
//   - RepeatRead: the agent marks it on its copies of a test's recordings
//     when the test opens, from the reads this replay's namer named
//     (models.MarkRepeatReads). A file that kept a past run's mark would
//     hand exact serving a read no namer of this run named (keploy alone
//     names none). Where the recordings prove that read, exact serving
//     would answer one call more than recorded and not charge one of its
//     recordings left unmade. A pointer, it is cleared whole (nil).
//
// Parts of the mock held through a pointer, slice or map (Spec's HTTPReq and
// the other payloads) are not walked: walking every payload would cost every
// read and write. Today one field there is tagged so, HTTPReq.URLRendered,
// and only a replay sets it, on the request it sends for a test case.
func clearRuntimeOnly(m *models.Mock) {
	v := reflect.ValueOf(m).Elem()
	for _, path := range runtimeOnlyMockFields {
		v.FieldByIndex(path).SetZero()
	}
}
