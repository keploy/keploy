package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// HOW THE WORDS FORMAT IS READ AND WRITTEN (WordsEntry, WordSlot), in YAML and
// JSON alike. gob carries both types field by field, Unknown and Seen with
// them, so nothing here is needed for it.
//
//   - An entry of this build's version (v: WordsVersion) reads its known keys
//     (v, digest, layout, slots) into their fields and the reserved seen into
//     Seen. Every other key is carried in Unknown, as written, and the entry
//     is void (WordsWrittenByNewer).
//   - An entry of any other version reads only its v, which an integer must
//     be (a v that is not one is carried with the rest). Every other key is
//     carried in Unknown, so it decodes whatever shape a newer keploy wrote
//     and never fails a document's decode. It is void.
//   - A slot reads seg, run, kind, layout and skeleton, and carries every
//     other key in its Unknown. A slot with any voids its entry.
//   - Writing is the known keys first, in the order above, then the carried
//     ones, sorted. A carried key is never written twice. An entry and a
//     slot that carry nothing are written as they always were.
//
// A known key of the wrong type (digest: [1]) fails the decode, as a
// malformed field anywhere in a mock does. So do a duplicate key, a key that
// is not a string, and a carried value too large or too deeply aliased to
// carry (maxCarriedNodes).

// The keys a WordsEntry of this version and a WordSlot read.
var (
	wordsEntryKeys = []string{"v", "digest", "layout", "slots", "seen"}
	wordSlotKeys   = []string{"seg", "run", "kind", "layout", "skeleton"}
)

// maxCarriedNodes bounds the YAML nodes one carried value may expand to,
// aliases followed, so that no document can make its decode take unbounded
// time or memory.
const maxCarriedNodes = 1 << 14

// wordsField is one key of an entry or a slot as it is written: its value
// typed (val) or carried (raw).
type wordsField struct {
	key string
	val interface{}
	raw WordsRaw
}

// fields is s's keys as they are written: seg; the run only where it names
// one (WordSlot.Run); kind, layout and skeleton where set.
func (s WordSlot) fields() []wordsField {
	out := []wordsField{{key: "seg", val: s.Seg}}
	if s.Kind == WordSlotRun || s.Run != 0 {
		out = append(out, wordsField{key: "run", val: s.Run})
	}
	if s.Kind != "" {
		out = append(out, wordsField{key: "kind", val: s.Kind})
	}
	if s.Layout != "" {
		out = append(out, wordsField{key: "layout", val: s.Layout})
	}
	if s.Skeleton != "" {
		out = append(out, wordsField{key: "skeleton", val: s.Skeleton})
	}
	return out
}

// fields is e's keys as they are written. Of an entry of this version: v,
// digest, layout and slots always, and seen where carried. Of any other: v
// where set, and the known fields only where set (a decoded one carries them
// in Unknown).
func (e WordsEntry) fields() []wordsField {
	var out []wordsField
	this := e.V == WordsVersion
	if e.V != 0 {
		out = append(out, wordsField{key: "v", val: e.V})
	}
	if this || e.Digest != "" {
		out = append(out, wordsField{key: "digest", val: e.Digest})
	}
	if this || e.Layout != "" {
		out = append(out, wordsField{key: "layout", val: e.Layout})
	}
	if this || e.Slots != nil {
		out = append(out, wordsField{key: "slots", val: e.Slots})
	}
	if e.Seen != "" {
		out = append(out, wordsField{key: "seen", raw: e.Seen})
	}
	return out
}

// MarshalYAML writes the slot's keys (fields), then those it carries.
func (s WordSlot) MarshalYAML() (interface{}, error) { return wordsYAML(s.fields(), s.Unknown) }

// MarshalJSON writes the slot as MarshalYAML does.
func (s WordSlot) MarshalJSON() ([]byte, error) { return wordsJSON(s.fields(), s.Unknown) }

// MarshalYAML writes the entry's keys (fields), then those it carries.
func (e WordsEntry) MarshalYAML() (interface{}, error) { return wordsYAML(e.fields(), e.Unknown) }

// MarshalJSON writes the entry as MarshalYAML does.
func (e WordsEntry) MarshalJSON() ([]byte, error) { return wordsJSON(e.fields(), e.Unknown) }

// UnmarshalYAML reads a slot: its known keys into their fields, every other
// key into Unknown.
func (s *WordSlot) UnmarshalYAML(n *yaml.Node) error {
	pairs, err := yamlPairs(n, "slot")
	if err != nil {
		return err
	}
	var out WordSlot
	unknown := map[string]WordsRaw{}
	for _, p := range pairs {
		var err error
		switch p.key {
		case "seg":
			err = p.val.Decode(&out.Seg)
		case "run":
			err = p.val.Decode(&out.Run)
		case "kind":
			err = p.val.Decode(&out.Kind)
		case "layout":
			err = p.val.Decode(&out.Layout)
		case "skeleton":
			err = p.val.Decode(&out.Skeleton)
		default:
			unknown[p.key], err = rawOfYAML(p.val)
		}
		if err != nil {
			return fmt.Errorf("learned words: slot key %q: %w", p.key, err)
		}
	}
	if out.Unknown, err = rawObject(unknown); err != nil {
		return err
	}
	*s = out
	return nil
}

// UnmarshalJSON reads a slot as UnmarshalYAML does.
func (s *WordSlot) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return nil
	}
	pairs, err := jsonPairs(data, "slot")
	if err != nil {
		return err
	}
	var out WordSlot
	unknown := map[string]WordsRaw{}
	for key, raw := range pairs {
		var err error
		switch key {
		case "seg":
			err = json.Unmarshal(raw, &out.Seg)
		case "run":
			err = json.Unmarshal(raw, &out.Run)
		case "kind":
			err = json.Unmarshal(raw, &out.Kind)
		case "layout":
			err = json.Unmarshal(raw, &out.Layout)
		case "skeleton":
			err = json.Unmarshal(raw, &out.Skeleton)
		default:
			unknown[key], err = rawOfJSON(raw)
		}
		if err != nil {
			return fmt.Errorf("learned words: slot key %q: %w", key, err)
		}
	}
	if out.Unknown, err = rawObject(unknown); err != nil {
		return err
	}
	*s = out
	return nil
}

// UnmarshalYAML reads an entry: of this version, its known keys into their
// fields, seen into Seen and every other key into Unknown; of any other, its
// v, and every other key into Unknown.
func (e *WordsEntry) UnmarshalYAML(n *yaml.Node) error {
	pairs, err := yamlPairs(n, "entry")
	if err != nil {
		return err
	}
	var out WordsEntry
	readV := false
	for _, p := range pairs {
		var v int
		if p.key == "v" && p.val.Kind == yaml.ScalarNode && p.val.ShortTag() == "!!int" && p.val.Decode(&v) == nil {
			out.V, readV = v, true
		}
	}
	unknown := map[string]WordsRaw{}
	for _, p := range pairs {
		if p.key == "v" && readV {
			continue
		}
		var err error
		known := out.V == WordsVersion
		switch {
		case known && p.key == "digest":
			err = p.val.Decode(&out.Digest)
		case known && p.key == "layout":
			err = p.val.Decode(&out.Layout)
		case known && p.key == "slots":
			err = p.val.Decode(&out.Slots)
		case known && p.key == "seen":
			out.Seen, err = rawOfYAML(p.val)
		default:
			unknown[p.key], err = rawOfYAML(p.val)
		}
		if err != nil {
			return fmt.Errorf("learned words: entry key %q: %w", p.key, err)
		}
	}
	if out.Unknown, err = rawObject(unknown); err != nil {
		return err
	}
	*e = out
	return nil
}

// UnmarshalJSON reads an entry as UnmarshalYAML does.
func (e *WordsEntry) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return nil
	}
	pairs, err := jsonPairs(data, "entry")
	if err != nil {
		return err
	}
	var out WordsEntry
	var v int
	if raw, ok := pairs["v"]; ok && isJSONInteger(raw) && json.Unmarshal(raw, &v) == nil {
		out.V = v
		delete(pairs, "v")
	}
	unknown := map[string]WordsRaw{}
	for key, raw := range pairs {
		var err error
		known := out.V == WordsVersion
		switch {
		case known && key == "digest":
			err = json.Unmarshal(raw, &out.Digest)
		case known && key == "layout":
			err = json.Unmarshal(raw, &out.Layout)
		case known && key == "slots":
			err = json.Unmarshal(raw, &out.Slots)
		case known && key == "seen":
			out.Seen, err = rawOfJSON(raw)
		default:
			unknown[key], err = rawOfJSON(raw)
		}
		if err != nil {
			return fmt.Errorf("learned words: entry key %q: %w", key, err)
		}
	}
	if out.Unknown, err = rawObject(unknown); err != nil {
		return err
	}
	*e = out
	return nil
}

// has reports whether r, an object of carried keys, holds key.
func (r WordsRaw) has(key string) bool {
	if r == "" {
		return false
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal([]byte(r), &keys) != nil {
		return false
	}
	_, ok := keys[key]
	return ok
}

// wordsYAML is the mapping of fields, then of the keys unknown carries (an
// object of them), sorted, each key written once.
func wordsYAML(fields []wordsField, unknown WordsRaw) (*yaml.Node, error) {
	extra, err := carriedFields(unknown)
	if err != nil {
		return nil, err
	}
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	written := map[string]bool{}
	for _, f := range append(fields, extra...) {
		if written[f.key] {
			continue
		}
		written[f.key] = true
		v := &yaml.Node{}
		if f.raw != "" {
			v, err = yamlOfRaw(f.raw)
		} else {
			err = v.Encode(f.val)
		}
		if err != nil {
			return nil, fmt.Errorf("learned words: write key %q: %w", f.key, err)
		}
		out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.key}, v)
	}
	return out, nil
}

// wordsJSON is wordsYAML's object in JSON.
func wordsJSON(fields []wordsField, unknown WordsRaw) ([]byte, error) {
	extra, err := carriedFields(unknown)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteByte('{')
	written := map[string]bool{}
	for _, f := range append(fields, extra...) {
		if written[f.key] {
			continue
		}
		written[f.key] = true
		v := []byte(f.raw)
		if f.raw == "" {
			if v, err = json.Marshal(f.val); err != nil {
				return nil, fmt.Errorf("learned words: write key %q: %w", f.key, err)
			}
		}
		k, err := json.Marshal(f.key)
		if err != nil {
			return nil, err
		}
		if len(written) > 1 {
			b.WriteByte(',')
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// carriedFields is the keys of unknown, an object of carried keys, sorted.
func carriedFields(unknown WordsRaw) ([]wordsField, error) {
	if unknown == "" {
		return nil, nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(unknown), &keys); err != nil {
		return nil, fmt.Errorf("learned words: the carried keys are not a JSON object: %w", err)
	}
	out := make([]wordsField, 0, len(keys))
	for k, v := range keys {
		out = append(out, wordsField{key: k, raw: WordsRaw(v)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

// yamlPair is one key of a YAML mapping and its value.
type yamlPair struct {
	key string
	val *yaml.Node
}

// yamlPairs is the keys of n, a mapping, in order: an error when n is not a
// mapping, or holds a key twice or a key that is not a string. what names n
// in the error.
func yamlPairs(n *yaml.Node, what string) ([]yamlPair, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("learned words: a %s is a mapping", what)
	}
	out := make([]yamlPair, 0, len(n.Content)/2)
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if k.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("learned words: a %s holds a key that is not a string", what)
		}
		if seen[k.Value] {
			return nil, fmt.Errorf("learned words: a %s holds the key %q twice", what, k.Value)
		}
		seen[k.Value] = true
		out = append(out, yamlPair{key: k.Value, val: n.Content[i+1]})
	}
	return out, nil
}

// jsonPairs is the keys of data, an object: an error when it is not one.
func jsonPairs(data []byte, what string) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil || out == nil {
		return nil, fmt.Errorf("learned words: a %s is an object", what)
	}
	return out, nil
}

// rawObject is keys as one carried object; "" for none.
func rawObject(keys map[string]WordsRaw) (WordsRaw, error) {
	if len(keys) == 0 {
		return "", nil
	}
	obj := make(map[string]json.RawMessage, len(keys))
	for k, v := range keys {
		obj[k] = json.RawMessage(v)
	}
	return compactJSON(obj)
}

// rawOfJSON is raw, a JSON value, as it is carried: compact.
func rawOfJSON(raw json.RawMessage) (WordsRaw, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return "", err
	}
	return WordsRaw(b.String()), nil
}

// rawOfYAML is n, a YAML value, as it is carried: as JSON, each scalar of the
// type YAML resolves it to (a number JSON cannot write, such as .inf, as its
// text).
func rawOfYAML(n *yaml.Node) (WordsRaw, error) {
	budget := maxCarriedNodes
	v, err := plainOfYAML(n, &budget)
	if err != nil {
		return "", err
	}
	return compactJSON(v)
}

// compactJSON is v as compact JSON, with no HTML escaping.
func compactJSON(v interface{}) (WordsRaw, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return WordsRaw(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), nil
}

// errCarriedTooLarge is why a carried value that expands past
// maxCarriedNodes fails its decode.
var errCarriedTooLarge = errors.New("learned words: a carried value is too large")

// plainOfYAML is n as a value JSON writes, each node counted against budget.
func plainOfYAML(n *yaml.Node, budget *int) (interface{}, error) {
	if *budget--; *budget < 0 {
		return nil, errCarriedTooLarge
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, errors.New("learned words: a document holds one value")
		}
		return plainOfYAML(n.Content[0], budget)
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, errors.New("learned words: an alias to nothing")
		}
		return plainOfYAML(n.Alias, budget)
	case yaml.SequenceNode:
		out := make([]interface{}, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := plainOfYAML(c, budget)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		pairs, err := yamlPairs(n, "carried value")
		if err != nil {
			return nil, err
		}
		out := make(map[string]interface{}, len(pairs))
		for _, p := range pairs {
			v, err := plainOfYAML(p.val, budget)
			if err != nil {
				return nil, err
			}
			out[p.key] = v
		}
		return out, nil
	case yaml.ScalarNode:
		return plainScalar(n), nil
	}
	return nil, fmt.Errorf("learned words: a YAML node of kind %d", n.Kind)
}

// plainScalar is n, a scalar, as the type YAML resolves it to: null, a
// bool, a number (as its JSON literal), or else its text.
func plainScalar(n *yaml.Node) interface{} {
	switch n.ShortTag() {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
	case "!!int":
		if isJSONNumber(n.Value) {
			return json.Number(n.Value)
		}
		var i int64
		if n.Decode(&i) == nil {
			return json.Number(strconv.FormatInt(i, 10))
		}
		var u uint64
		if n.Decode(&u) == nil {
			return json.Number(strconv.FormatUint(u, 10))
		}
	case "!!float":
		if isJSONNumber(n.Value) {
			return json.Number(n.Value)
		}
		var f float64
		if n.Decode(&f) == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
		}
	}
	return n.Value
}

// yamlOfRaw is raw, a carried JSON value, as a YAML node, written as keploy
// writes YAML: mappings and sequences in block style, and a string quoted
// only where YAML would read it otherwise.
func yamlOfRaw(raw WordsRaw) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("learned words: a carried value is one value")
	}
	n := doc.Content[0]
	blockStyle(n)
	return n, nil
}

// blockStyle writes n's mappings and sequences in block style, and lets the
// encoder quote a string (tagged a string, so it stays one) only where it
// must.
func blockStyle(n *yaml.Node) {
	switch {
	case n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode:
		n.Style &^= yaml.FlowStyle
	case n.Kind == yaml.ScalarNode && n.ShortTag() == "!!str":
		n.Tag = "!!str"
		n.Style &^= yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle
	}
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// isJSONNull reports whether data is JSON's null.
func isJSONNull(data []byte) bool { return string(bytes.TrimSpace(data)) == "null" }

// isJSONInteger reports whether raw is a JSON number with no fraction and no
// exponent.
func isJSONInteger(raw json.RawMessage) bool {
	s := string(bytes.TrimSpace(raw))
	return isJSONNumber(s) && !bytes.ContainsAny([]byte(s), ".eE")
}

// isJSONNumber reports whether s is a number as JSON writes it:
// -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
func isJSONNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(s)
}
