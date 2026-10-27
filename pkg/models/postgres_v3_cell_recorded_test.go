package models

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// cellEncodings are the GobEncode bytes of every cell in v, in field order
// (a map's entries in the order of their keys printed).
func cellEncodings(t *testing.T, v reflect.Value, out *[][]byte) {
	t.Helper()
	cellType := reflect.TypeOf(PostgresV3Cell{})
	switch {
	case v.Type() == cellType:
		b, err := v.Interface().(PostgresV3Cell).GobEncode()
		if err != nil {
			t.Fatalf("encode a cell: %v", err)
		}
		*out = append(*out, b)
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			cellEncodings(t, v.Elem(), out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				cellEncodings(t, v.Field(i), out)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			cellEncodings(t, v.Index(i), out)
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		for _, k := range keys {
			cellEncodings(t, v.MapIndex(k), out)
		}
	}
}

func specCells(t *testing.T, spec *PostgresV3Spec) [][]byte {
	t.Helper()
	var out [][]byte
	cellEncodings(t, reflect.ValueOf(spec), &out)
	return out
}

// readSpecBy gob-decodes enc, an encoded PostgresV3Spec, with each cell read
// by gob from its first message when byGob, and returns it and the gob
// decoders its cells built.
func readSpecBy(t *testing.T, enc []byte, byGob bool) (*PostgresV3Spec, int64) {
	t.Helper()
	cellReadByGob.Store(byGob)
	defer cellReadByGob.Store(false)
	before := cellGobDecoders.Load()
	var spec PostgresV3Spec
	if err := gob.NewDecoder(bytes.NewReader(enc)).Decode(&spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &spec, cellGobDecoders.Load() - before
}

// The recorded sets KEPLOY_RECORDED_PG_SETS names (mocks.yaml files, split by
// ":") read the same with each cell read by gob and by cellStream: every
// PostgresV3 mock's spec, gob-encoded as the agent's store encodes it, decodes
// to deeply equal specs whose cells encode to the bytes the recorded cells
// encode to. (The specs' own encodings are not compared: gob writes a map's
// entries in the map's order.) It says how many cells were compared and how
// many gob decoders each read built for them.
func TestRecordedPostgresSetsReadAsGobReadsThem(t *testing.T) {
	sets := os.Getenv("KEPLOY_RECORDED_PG_SETS")
	if sets == "" {
		t.Skip("KEPLOY_RECORDED_PG_SETS names no recorded set")
	}
	for _, path := range strings.Split(sets, ":") {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(f)
		var mocks, cells int
		var byGob, byStream int64
		for {
			var doc struct {
				Kind string `yaml:"kind"`
				Name string `yaml:"name"`
				Spec struct {
					PostgresV3 *PostgresV3Spec `yaml:"postgresV3"`
				} `yaml:"spec"`
			}
			if err := dec.Decode(&doc); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Fatalf("%s: %v", path, err)
			}
			if doc.Kind != "PostgresV3" || doc.Spec.PostgresV3 == nil {
				continue
			}
			mocks++
			var enc bytes.Buffer
			if err := gob.NewEncoder(&enc).Encode(doc.Spec.PostgresV3); err != nil {
				t.Fatalf("%s %s: encode: %v", path, doc.Name, err)
			}
			want, g := readSpecBy(t, enc.Bytes(), true)
			got, s := readSpecBy(t, enc.Bytes(), false)
			byGob, byStream = byGob+g, byStream+s
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s: read %+v, gob's reference read %+v", path, doc.Name, got, want)
			}
			recorded, read, ref := specCells(t, doc.Spec.PostgresV3), specCells(t, got), specCells(t, want)
			cells += len(recorded)
			if !reflect.DeepEqual(read, recorded) || !reflect.DeepEqual(ref, recorded) {
				t.Errorf("%s %s: the cells read encode to other bytes than the recorded ones (cellStream's alike: %v, gob's: %v)",
					path, doc.Name, reflect.DeepEqual(read, recorded), reflect.DeepEqual(ref, recorded))
			}
		}
		_ = f.Close()
		if mocks == 0 {
			t.Errorf("%s holds no PostgresV3 mock", path)
		}
		t.Logf("%s: %d PostgresV3 mocks, %d cells read alike; gob decoders built for the cells: %d read by gob, %d read by cellStream",
			path, mocks, cells, byGob, byStream)
	}
}

// BenchmarkRecordedPostgresSetRead decodes every PostgresV3 spec of the first
// set KEPLOY_RECORDED_PG_SETS names, gob-encoded as the agent's store encodes
// it, with each cell read by gob (as before cellStream) and by cellStream.
func BenchmarkRecordedPostgresSetRead(b *testing.B) {
	sets := os.Getenv("KEPLOY_RECORDED_PG_SETS")
	if sets == "" {
		b.Skip("KEPLOY_RECORDED_PG_SETS names no recorded set")
	}
	path, _, _ := strings.Cut(sets, ":")
	f, err := os.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var encs [][]byte
	dec := yaml.NewDecoder(f)
	for {
		var doc struct {
			Kind string `yaml:"kind"`
			Spec struct {
				PostgresV3 *PostgresV3Spec `yaml:"postgresV3"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			b.Fatal(err)
		}
		if doc.Kind != "PostgresV3" || doc.Spec.PostgresV3 == nil {
			continue
		}
		var enc bytes.Buffer
		if err := gob.NewEncoder(&enc).Encode(doc.Spec.PostgresV3); err != nil {
			b.Fatal(err)
		}
		encs = append(encs, enc.Bytes())
	}
	for _, byGob := range []bool{true, false} {
		name := "cellStream"
		if byGob {
			name = "gob"
		}
		b.Run(name, func(b *testing.B) {
			cellReadByGob.Store(byGob)
			defer cellReadByGob.Store(false)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for _, enc := range encs {
					var spec PostgresV3Spec
					if err := gob.NewDecoder(bytes.NewReader(enc)).Decode(&spec); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*len(encs)), "µs/mock")
		})
	}
}
