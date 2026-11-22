package models

import (
	"reflect"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// MergedMappings is the mappings the values of a node's merge keys (<<)
// name: a mapping, an alias of one, or a sequence of those, each followed
// by the mappings its own merge keys name, as YAML resolves them; each once.
func MergedMappings(values []*yaml.Node) []*yaml.Node {
	var out []*yaml.Node
	seen := map[*yaml.Node]bool{}
	var add func(*yaml.Node)
	add = func(v *yaml.Node) {
		for v != nil && v.Kind == yaml.AliasNode && v.Alias != nil {
			v = v.Alias
		}
		switch {
		case v == nil || seen[v]:
		case v.Kind == yaml.MappingNode:
			seen[v] = true
			out = append(out, v)
			for i := 0; i+1 < len(v.Content); i += 2 {
				if v.Content[i].Value == "<<" {
					add(v.Content[i+1])
				}
			}
		case v.Kind == yaml.SequenceNode:
			for _, c := range v.Content {
				add(c)
			}
		}
	}
	for _, v := range values {
		add(v)
	}
	return out
}

// The levels of a mapping file (MappingKeys).
const (
	MappingLevelTop   = "mapping"
	MappingLevelTest  = "test"
	MappingLevelEntry = "entry"
	MappingLevelBoot  = "boot"
	MappingLevelGroup = "group"
	MappingLevelSuite = "suite"
	MappingLevelGiven = "given"
)

var (
	mappingKeysOnce sync.Once
	mappingKeys     map[string]map[string]bool
)

// MappingKeys is the keys keploy reads and writes at a level of a mapping
// file: the top (MappingLevelTop), a test entry in every shape it reads in
// (MappingLevelTest), a mock entry, a boot, a concurrency group, a suite and
// a value a test gave (MappingLevelGiven, an item of a test's given:).
// A key a writer leaves out at its level is keploy's to leave out; any other
// is a hand edit (mapdb keeps it).
func MappingKeys(level string) map[string]bool {
	mappingKeysOnce.Do(func() {
		test := yamlKeys(reflect.TypeOf(writtenTestFormat{}))
		test["mocks"] = true
		mappingKeys = map[string]map[string]bool{
			MappingLevelTop:   yamlKeys(reflect.TypeOf(Mapping{})),
			MappingLevelTest:  test,
			MappingLevelEntry: yamlKeys(reflect.TypeOf(MockEntry{})),
			MappingLevelBoot:  yamlKeys(reflect.TypeOf(BootSpec{})),
			MappingLevelGroup: yamlKeys(reflect.TypeOf(ConcurrencyGroup{})),
			MappingLevelSuite: yamlKeys(reflect.TypeOf(SuiteSpan{})),
			MappingLevelGiven: yamlKeys(reflect.TypeOf(GivenValue{})),
		}
	})
	return mappingKeys[level]
}

var (
	mappingJSONKeysOnce sync.Once
	mappingJSONKeys     map[string]map[string]bool
)

// MappingJSONKeys is MappingKeys for a mapping file in JSON: the keys the
// json tags name at each level, in lower case, since a JSON decode matches
// a key to its field whatever its case.
func MappingJSONKeys(level string) map[string]bool {
	mappingJSONKeysOnce.Do(func() {
		test := jsonKeys(reflect.TypeOf(writtenTestFormat{}))
		test["mocks"] = true
		mappingJSONKeys = map[string]map[string]bool{
			MappingLevelTop:   jsonKeys(reflect.TypeOf(Mapping{})),
			MappingLevelTest:  test,
			MappingLevelEntry: jsonKeys(reflect.TypeOf(MockEntry{})),
			MappingLevelBoot:  jsonKeys(reflect.TypeOf(BootSpec{})),
			MappingLevelGroup: jsonKeys(reflect.TypeOf(ConcurrencyGroup{})),
			MappingLevelSuite: jsonKeys(reflect.TypeOf(SuiteSpan{})),
			MappingLevelGiven: jsonKeys(reflect.TypeOf(GivenValue{})),
		}
	})
	return mappingJSONKeys[level]
}

// jsonKeys is the keys the json tags of t's fields name, in lower case,
// embedded structs' included: what a JSON decode of t knows.
func jsonKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
		case name == "" && f.Anonymous && f.Type.Kind() == reflect.Struct:
			for k := range jsonKeys(f.Type) {
				keys[k] = true
			}
		case name != "":
			keys[strings.ToLower(name)] = true
		case f.IsExported():
			keys[strings.ToLower(f.Name)] = true
		}
	}
	return keys
}

// yamlKeys is the keys the yaml tags of t's fields name, inline structs'
// included: what a decode of t knows.
func yamlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		switch {
		case name == "-":
		case strings.Contains(opts, "inline") && f.Type.Kind() == reflect.Struct:
			for k := range yamlKeys(f.Type) {
				keys[k] = true
			}
		case name != "":
			keys[name] = true
		case f.IsExported():
			keys[strings.ToLower(f.Name)] = true
		}
	}
	return keys
}
