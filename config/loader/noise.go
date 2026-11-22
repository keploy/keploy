package loader

import (
	"bytes"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.yaml.in/yaml/v3"
)

// Files is keploy's config files read into a viper instance as Read and
// Merge read them, with test.globalNoise kept as the files write it, for
// Unmarshal.
//
// viper keeps no key as it is written: it lowercases every key, and splits
// a key on "." into nested keys. That is right for keploy's own options, but
// test.globalNoise's keys below a bucket are the user's: a url key is a
// regular expression over request paths, which have case and dots, and a
// body, header or requestbody key is a field path, dotted (data.id). Through
// viper, the url entry "^/api/Users/[0-9]+$" became "^/api/users/[0-9]+$",
// which never matches /api/Users/3, and a dotted key became nested keys that
// do not decode as a list of patterns, so keploy refused keploy.yml with
// "failed to unmarshal the config" -- the entries keploy's own hints tell
// users to write among them.
//
// So Files decodes test.globalNoise from the bytes it hands viper, with
// viper's YAML parser, and keeps every key there as written but for the
// names keploy reads them by, which it lowercases as viper did: global and
// test-sets, and the buckets (url, body, header, requestbody). A test set's
// name, the directory keploy looks it up by, is kept too.
type Files struct {
	v *viper.Viper
	// noise is test.globalNoise as the files read write it, the override
	// merged over keploy.yml as viper merges it; nil when none has one.
	noise any
}

// New is a Files reading into v, which must have its config type set to yaml.
func New(v *viper.Viper) *Files {
	return &Files{v: v}
}

// Read reads the config file into the viper instance, as the package's Read,
// and keeps its test.globalNoise as it is written. Its error is the
// package's Read's.
func (f *Files) Read(file string) error {
	raw, err := readConfigFile(file)
	if err != nil {
		return err
	}
	if err := f.v.ReadConfig(bytes.NewReader(raw)); err != nil {
		return err
	}
	noise, err := globalNoiseOf(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	f.noise = noise
	return nil
}

// Merge merges the config file over what the viper instance holds, as the
// package's Merge, and its test.globalNoise over the one kept, as viper
// merges maps: an entry the file writes replaces the one kept, and a map in
// both is merged key by key. A file with no test.globalNoise keeps a map
// kept and drops any other value, so Unmarshal decodes viper's copy of that
// value: one that is not a map has no keys for viper to rewrite. Its error
// is Read's.
func (f *Files) Merge(file string) error {
	raw, err := readConfigFile(file)
	if err != nil {
		return err
	}
	if err := f.v.MergeConfig(bytes.NewReader(raw)); err != nil {
		return err
	}
	noise, err := globalNoiseOf(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	kept := map[string]any{"globalNoise": f.noise}
	mergeNoise(map[string]any{"globalNoise": noise}, kept)
	f.noise = kept["globalNoise"]
	return nil
}

// globalNoiseType is test.globalNoise's type in keploy's Config.
var globalNoiseType = reflect.TypeOf(config.Globalnoise{})

// Unmarshal decodes the viper instance into cfg, as viper's Unmarshal does,
// with test.globalNoise decoded from the files as they write it in place of
// viper's copy of it. Each value there is decoded as viper decodes it, with
// viper's own decode hooks and weak typing, so a value of the wrong type is
// refused in viper's words. With no file read that has test.globalNoise, it
// is viper's Unmarshal.
func (f *Files) Unmarshal(cfg *config.Config) error {
	return f.v.Unmarshal(cfg, func(dc *mapstructure.DecoderConfig) {
		verbatim := mapstructure.DecodeHookFuncType(func(_ reflect.Type, to reflect.Type, data any) (any, error) {
			if to != globalNoiseType || f.noise == nil {
				return data, nil
			}
			return f.noise, nil
		})
		if dc.DecodeHook == nil {
			dc.DecodeHook = verbatim
			return
		}
		dc.DecodeHook = mapstructure.ComposeDecodeHookFunc(verbatim, dc.DecodeHook)
	})
}

// globalNoiseOf is test.globalNoise in raw, a config file viper has read, as
// it is written (Files); nil when raw has none. raw is parsed as viper parses
// it, and test and globalNoise are found by any case, as viper finds them.
func globalNoiseOf(raw []byte) (any, error) {
	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	test, _ := asMap(lookupFold(doc, "test"))
	gn := lookupFold(test, "globalNoise")
	top, ok := asMap(gn)
	if !ok {
		// None, or not a map: kept, and Unmarshal refuses one in viper's
		// words.
		return gn, nil
	}
	out := map[string]any{}
	for _, k := range sortedKeys(top) {
		name := strings.ToLower(k)
		switch name {
		case "global":
			setMerged(out, name, lowerKeys(top[k]))
		case "test-sets":
			sets, ok := asMap(top[k])
			if !ok {
				setMerged(out, name, top[k])
				continue
			}
			kept := map[string]any{}
			for _, set := range sortedKeys(sets) {
				setMerged(kept, set, lowerKeys(sets[set]))
			}
			setMerged(out, name, kept)
		default:
			setMerged(out, name, top[k])
		}
	}
	return out, nil
}

// lowerKeys is v, a map of buckets to entries, with each bucket's name
// lowercased, as keploy reads buckets by (url, body, header, requestbody),
// and every entry's key kept. Two buckets of one name in different cases are
// merged. A v that is not a map is kept.
func lowerKeys(v any) any {
	m, ok := asMap(v)
	if !ok {
		return v
	}
	out := map[string]any{}
	for _, k := range sortedKeys(m) {
		setMerged(out, strings.ToLower(k), m[k])
	}
	return out
}

// setMerged sets m[k] to v, merged over the value m holds there when both
// are maps (mergeNoise).
func setMerged(m map[string]any, k string, v any) {
	if old, ok := asMap(m[k]); ok {
		if nv, ok := asMap(v); ok {
			mergeNoise(nv, old)
			m[k] = old
			return
		}
	}
	m[k] = v
}

// mergeNoise merges src over tgt as viper's MergeConfig merges maps, with
// keys compared as written: a key only src has is added; a map in both is
// merged; a map in tgt is kept over a value in src that is not one; any
// other value in src replaces tgt's.
func mergeNoise(src, tgt map[string]any) {
	for k, sv := range src {
		tv, ok := tgt[k]
		if !ok {
			tgt[k] = sv
			continue
		}
		if tm, ok := asMap(tv); ok {
			if sm, ok := asMap(sv); ok {
				mergeNoise(sm, tm)
				tgt[k] = tm
			}
			continue
		}
		tgt[k] = sv
	}
}

// lookupFold is m's value at the keys equal to k under case folding, as
// viper looks keys up. Several such keys are merged in key order, as
// lowerKeys merges buckets: maps key by key (setMerged), and any other value
// replaces the one before it.
func lookupFold(m map[string]any, k string) any {
	out := map[string]any{}
	for _, mk := range sortedKeys(m) {
		if strings.EqualFold(mk, k) {
			setMerged(out, k, m[mk])
		}
	}
	return out[k]
}

// asMap is v as a map with string keys, as viper casts a YAML mapping
// (cast.ToStringMap: a key that is not a string is written with %v).
func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprintf("%v", k)] = val
		}
		return out, true
	}
	return nil, false
}

// sortedKeys is m's keys in order, so that keys that fold to one name are
// merged the same way every run.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
