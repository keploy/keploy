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
)

var (
	mappingKeysOnce sync.Once
	mappingKeys     map[string]map[string]bool
)

// MappingKeys is the keys keploy reads and writes at a level of a mapping
// file: the top (MappingLevelTop), a test entry in every shape it reads in
// (MappingLevelTest), a mock entry, a boot, a concurrency group and a suite.
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
		}
	})
	return mappingKeys[level]
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
