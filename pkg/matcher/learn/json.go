package learn

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher"
)

// DecodeJSON decodes a JSON document with its numbers as json.Number, so a
// large integer (a snowflake ID) keeps every digit. ok is false when b is not
// one JSON value.
func DecodeJSON(b []byte) (v interface{}, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// KeysLeaf is the keys of every JSON object in v (a decoded body under
// root, "body"), one per line in order, as a leaf of type Keys that no
// variable binds at (Unnamed); false when v has no key.
func KeysLeaf(root string, v interface{}) (Leaf, bool) {
	keys := map[string]bool{}
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, child := range t {
				keys[k] = true
				walk(child)
			}
		case []interface{}:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	if len(keys) == 0 {
		return Leaf{}, false
	}
	list := make([]string, 0, len(keys))
	for k := range keys {
		list = append(list, k)
	}
	sort.Strings(list)
	return Leaf{Path: root + "{keys}", Type: Keys, Value: strings.Join(list, "\n"), Unnamed: true}, true
}

// JSONLeaves adds the leaves of a decoded JSON value to out, under root (the
// leaf name of the value itself, "body"). See Leaf for the two path
// dialects.
func JSONLeaves(root string, v interface{}, out map[string]Leaf) {
	jsonLeaves(root, "", root, v, out)
}

// jsonLeaves walks v. path is its exact path, and risk its path in the
// request noise dialect relative to the root (the request matcher's JSON
// walker, matcher.ChangedJSONFieldPaths: keys dot-joined, an array "[]").
func jsonLeaves(path, risk, root string, v interface{}, out map[string]Leaf) {
	noise := root
	if risk != "" {
		noise = root + "." + risk
	}
	switch t := v.(type) {
	case map[string]interface{}:
		if len(t) == 0 {
			out[path] = Leaf{Path: path, Noise: noise, Type: EmptyObj}
			return
		}
		for k, child := range t {
			childRisk := k
			if risk != "" {
				childRisk = risk + "." + k
			}
			jsonLeaves(path+KeyStep(k), childRisk, root, child, out)
		}
	case []interface{}:
		if len(t) == 0 {
			out[path] = Leaf{Path: path, Noise: noise, Type: EmptyArray}
			return
		}
		childRisk := "[]"
		if risk != "" {
			childRisk = risk + "[]"
		}
		for i, child := range t {
			jsonLeaves(path+"["+strconv.Itoa(i)+"]", childRisk, root, child, out)
		}
	default:
		typ, val := scalar(t)
		out[path] = Leaf{Path: path, Noise: noise, Type: typ, Value: val}
	}
}

// KeyStep is the step an object key adds to an exact path (Leaf.Path): .key,
// or ["key"], the key as a JSON string as the variable grammar writes it
// (variables.Parse), for a key holding '.', '[' or ']', which a dot-joined
// path cannot name alone ("testsuite._id" in a Mongo filter).
func KeyStep(k string) string {
	if !strings.ContainsAny(k, ".[]") {
		return "." + k
	}
	q, _ := json.Marshal(k)
	return "[" + string(q) + "]"
}

// scalar is a decoded JSON scalar's type and text.
func scalar(v interface{}) (Type, string) {
	switch t := v.(type) {
	case nil:
		return Null, ""
	case string:
		return String, t
	case bool:
		return Bool, strconv.FormatBool(t)
	case json.Number:
		return Number, t.String()
	case float64:
		return Number, strconv.FormatFloat(t, 'g', -1, 64)
	}
	return String, ""
}

// excusedMark stands in for an array element the comparison excuses whole:
// it keeps the positions aligned, and every such element shares it, so it
// votes for no index.
const excusedMark = "\x00keploy-excused"

// reorderedArrays applies the order rule (matcher.ArrayReordered) to every
// array the two bodies hold at the same place with the same length, and
// returns the paths of those it refuses, sorted. A node the comparison
// excuses casts no vote: it is left out of its element first. The rule is
// not applied inside an array it refuses: that array's elements are not
// paired.
func reorderedArrays(rec, live interface{}, known Known) []string {
	if rec == nil || live == nil {
		return nil
	}
	var out []string
	walkReorder("body", "", rec, live, known, &out)
	sort.Strings(out)
	return out
}

// walkReorder walks the two bodies at path (the exact path) and risk (the
// request noise dialect's, relative to the body).
func walkReorder(path, risk string, rec, live interface{}, known Known, out *[]string) {
	switch r := rec.(type) {
	case map[string]interface{}:
		l, ok := live.(map[string]interface{})
		if !ok {
			return
		}
		for k, rv := range r {
			lv, ok := l[k]
			if !ok {
				continue
			}
			walkReorder(path+KeyStep(k), childRisk(risk, k), rv, lv, known, out)
		}
	case []interface{}:
		l, ok := live.([]interface{})
		if !ok || len(l) != len(r) {
			return
		}
		elemRisk := arrayRisk(risk)
		if len(r) >= 2 {
			re, le := make([]interface{}, len(r)), make([]interface{}, len(l))
			for i := range r {
				// Each side's element at its own position: the recorded
				// one's excused nodes are the recorded positions', and the
				// live one's the live positions'.
				ep := path + "[" + strconv.Itoa(i) + "]"
				re[i] = prune(ep, elemRisk, r[i], known)
				le[i] = prune(ep, elemRisk, l[i], known)
			}
			if matcher.ArrayReordered(re, le) {
				*out = append(*out, path)
				return
			}
		}
		for i := range r {
			walkReorder(path+"["+strconv.Itoa(i)+"]", elemRisk, r[i], l[i], known, out)
		}
	}
}

func childRisk(risk, k string) string {
	if risk == "" {
		return k
	}
	return risk + "." + k
}

func arrayRisk(risk string) string {
	if risk == "" {
		return "[]"
	}
	return risk + "[]"
}

// prune is v (at the exact path, and at risk in the request noise dialect)
// without the nodes known excuses.
func prune(path, risk string, v interface{}, known Known) interface{} {
	if excusedNode(path, risk, v, known) {
		return excusedMark
	}
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, child := range t {
			if excusedNode(path+KeyStep(k), childRisk(risk, k), child, known) {
				continue
			}
			out[k] = prune(path+KeyStep(k), childRisk(risk, k), child, known)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, child := range t {
			out[i] = prune(path+"["+strconv.Itoa(i)+"]", arrayRisk(risk), child, known)
		}
		return out
	}
	return v
}

func excusedNode(path, risk string, v interface{}, known Known) bool {
	noise := "body"
	if risk != "" {
		noise = "body." + risk
	}
	leaf := Leaf{Path: path, Noise: noise}
	switch v.(type) {
	case map[string]interface{}, []interface{}:
	default:
		leaf.Type, leaf.Value = scalar(v)
	}
	excused, _ := known.Excused(leaf)
	return excused
}
