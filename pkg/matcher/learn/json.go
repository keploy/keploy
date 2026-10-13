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

// JSONLeaves adds the leaves of a decoded JSON value to out, under root (the
// leaf name of the value itself, "body"). See Leaf for the two path
// dialects.
func JSONLeaves(root string, v interface{}, out map[string]Leaf) {
	jsonLeaves(root, "", root, false, v, out)
}

// jsonLeaves walks v. path is its exact path, risk its path in the request
// noise dialect relative to the root (the request matcher's JSON walker,
// matcher.ChangedJSONFieldPaths: keys dot-joined, an array "[]"), and
// unnamed whether some key on the way cannot be named.
func jsonLeaves(path, risk, root string, unnamed bool, v interface{}, out map[string]Leaf) {
	noise := root
	if risk != "" {
		noise = root + "." + risk
	}
	switch t := v.(type) {
	case map[string]interface{}:
		if len(t) == 0 {
			out[path] = Leaf{Path: path, Noise: noise, Type: EmptyObj, Unnamed: unnamed}
			return
		}
		for k, child := range t {
			childRisk := k
			if risk != "" {
				childRisk = risk + "." + k
			}
			jsonLeaves(path+"."+k, childRisk, root, unnamed || strings.ContainsAny(k, ".[]"), child, out)
		}
	case []interface{}:
		if len(t) == 0 {
			out[path] = Leaf{Path: path, Noise: noise, Type: EmptyArray, Unnamed: unnamed}
			return
		}
		childRisk := "[]"
		if risk != "" {
			childRisk = risk + "[]"
		}
		for i, child := range t {
			jsonLeaves(path+"["+strconv.Itoa(i)+"]", childRisk, root, unnamed, child, out)
		}
	default:
		typ, val := scalar(t)
		out[path] = Leaf{Path: path, Noise: noise, Type: typ, Value: val, Unnamed: unnamed}
	}
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
			childRisk := k
			if risk != "" {
				childRisk = risk + "." + k
			}
			walkReorder(path+"."+k, childRisk, rv, lv, known, out)
		}
	case []interface{}:
		l, ok := live.([]interface{})
		if !ok || len(l) != len(r) {
			return
		}
		childRisk := "[]"
		if risk != "" {
			childRisk = risk + "[]"
		}
		if len(r) >= 2 {
			re, le := make([]interface{}, len(r)), make([]interface{}, len(l))
			for i := range r {
				re[i] = prune(childRisk, r[i], known)
				le[i] = prune(childRisk, l[i], known)
			}
			if matcher.ArrayReordered(re, le) {
				*out = append(*out, path)
				return
			}
		}
		for i := range r {
			walkReorder(path+"["+strconv.Itoa(i)+"]", childRisk, r[i], l[i], known, out)
		}
	}
}

// prune is v without the nodes known excuses. risk is v's path in the
// request noise dialect.
func prune(risk string, v interface{}, known Known) interface{} {
	if excusedNode(risk, v, known) {
		return excusedMark
	}
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, child := range t {
			childRisk := k
			if risk != "" {
				childRisk = risk + "." + k
			}
			if excusedNode(childRisk, child, known) {
				continue
			}
			out[k] = prune(childRisk, child, known)
		}
		return out
	case []interface{}:
		childRisk := "[]"
		if risk != "" {
			childRisk = risk + "[]"
		}
		out := make([]interface{}, len(t))
		for i, child := range t {
			out[i] = prune(childRisk, child, known)
		}
		return out
	}
	return v
}

func excusedNode(risk string, v interface{}, known Known) bool {
	noise := "body"
	if risk != "" {
		noise = "body." + risk
	}
	leaf := Leaf{Noise: noise}
	switch v.(type) {
	case map[string]interface{}, []interface{}:
	default:
		leaf.Type, leaf.Value = scalar(v)
	}
	excused, _ := known.Excused(leaf)
	return excused
}
