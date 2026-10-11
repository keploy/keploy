package matcher

import (
	"fmt"
	"strconv"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// ResponseIgnoreOrdering is the ignoreOrdering a response comparison uses: the
// configured value (test.ignoreOrdering, on by default), except under the
// proven switch, where response arrays are compared in order whatever the
// configuration says. A reorder is then a difference, and judging tells an
// unstable order apart from a drifting value with ArrayReordered instead of
// forgiving it. Every response matcher calls this at its entry, so no caller
// can compare as a multiset under the switch.
func ResponseIgnoreOrdering(configured bool) bool {
	return configured && !provenswitch.On()
}

// ArrayReordered is the order rule: the ONE definition of "this array came back
// in another order". It is meant for every reader that has to tell a reordered
// array from a drifting one — the response noise walker (BodyNoiseFromJSONDiff),
// the request learner, the reason text and judging — so they cannot disagree
// about the same pair of arrays.
//
// For a recorded and a live array of the same length, it counts, for each live
// element, the leaves it shares with each recorded element: a leaf is a scalar
// (or an empty object or array) at a path inside the element, and two leaves
// are shared when both the path and the value are equal. If some live element
// shares more leaves with a recorded element at ANOTHER index than with the one
// at its own index, the array is reordered. Otherwise the elements pair by
// index, and whatever differs between a pair is a drift of that element.
//
// A scalar element is a single leaf, so for an array of scalars the rule reads:
// some live value equals a recorded value at another index and not the one at
// its own. Nested arrays are leaves by position inside their element, and get
// the rule themselves when the caller walks into the paired elements.
//
// Why "prefers another index" and not "is a permutation": the second misses the
// shape that matters, a reorder in which every element also drifts one leaf (a
// map-ordered list whose `expires_in` moved between record and replay). No
// element then equals any other, so a permutation test sees no reorder, the
// drifting leaves are learned as noise at every position, and the reorder
// itself goes green. Any pairing that beats the index pairing makes at least
// one element prefer another index, so the rule catches every reorder such a
// pairing would expose. It can also refuse an array the index pairing would
// explain (a drifted value that happens to equal a recorded neighbour); that
// keeps a test red with a reason, never green.
//
// Arrays of different lengths, and arrays of fewer than two elements, are never
// reordered: there is no other index to prefer.
func ArrayReordered(recorded, live []interface{}) bool {
	n := len(recorded)
	if n != len(live) || n < 2 {
		return false
	}
	// leaf -> the recorded indices whose element holds it. Counting through
	// this index costs one visit per shared leaf rather than one comparison
	// per pair of elements.
	holders := make(map[string][]int)
	for j, el := range recorded {
		for leaf := range elementLeaves(el) {
			holders[leaf] = append(holders[leaf], j)
		}
	}
	shared := make([]int, n)
	var touched []int
	for i, el := range live {
		for _, j := range touched {
			shared[j] = 0
		}
		touched = touched[:0]
		for leaf := range elementLeaves(el) {
			for _, j := range holders[leaf] {
				if shared[j] == 0 {
					touched = append(touched, j)
				}
				shared[j]++
			}
		}
		own := shared[i]
		for _, j := range touched {
			if j != i && shared[j] > own {
				return true
			}
		}
	}
	return false
}

// elementLeaves is the set of leaves of one array element, each a path inside
// the element joined with its value, in a form where two leaves are equal
// exactly when their paths and values are.
func elementLeaves(v interface{}) map[string]struct{} {
	out := make(map[string]struct{})
	collectLeaves(out, "", v)
	return out
}

// collectLeaves adds v's leaves under path. An object key is quoted and an
// array position bracketed, so no key can pose as a position or as a deeper
// path; the value carries its JSON type, so "1" and 1 are different leaves, as
// they are different values to the matcher.
func collectLeaves(out map[string]struct{}, path string, v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		if len(t) == 0 {
			out[path+"\x02{}"] = struct{}{}
			return
		}
		for k, child := range t {
			collectLeaves(out, path+"\x00"+strconv.Quote(k), child)
		}
	case []interface{}:
		if len(t) == 0 {
			out[path+"\x02[]"] = struct{}{}
			return
		}
		for i, child := range t {
			collectLeaves(out, path+"\x00["+strconv.Itoa(i)+"]", child)
		}
	default:
		out[path+"\x01"+leafValue(t)] = struct{}{}
	}
}

// leafValue renders a decoded JSON scalar with its type.
func leafValue(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return "s" + t
	case bool:
		return "b" + strconv.FormatBool(t)
	case float64:
		return "n" + strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprintf("%T:%v", t, t)
	}
}
