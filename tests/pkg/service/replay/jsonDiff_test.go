package replay

import (
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v2/pkg/service/replay"
)

func escapeSpecialChars(input string) string {
	// Replace common control characters with visible escape codes
	input = strings.ReplaceAll(input, "\n", "\\n")
	input = strings.ReplaceAll(input, "\t", "\\t")
	input = strings.ReplaceAll(input, "\r", "\\r")
	input = strings.ReplaceAll(input, " ", "")

	// Escape ANSI color codes by replacing the ESC character with a visible representation
	input = strings.ReplaceAll(input, "\x1b", "\\x1b")

	return input
}

func TestSprintJSONDiff(t *testing.T) {
	tests := []struct {
		name     string
		json1    string
		json2    string
		expected string
	}{
		{
			name:     "different JSONs",
			json1:    `[{"name":"Cat","id":3},{"name":"Dog","id":1},{"name":"Elephant","id":2},{"name":"Bird","id":4}]`,
			json2:    `[{"name":"Dog","id":1},{"name":"Cat","id":2},{"name":"Elephant","id":3},{"name":"Bird","id":4}]`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{|{\n"0":{|"0":{\n"name":\x1b[31m"Cat"\x1b[0m,\x1b[0m|"name":\x1b[32m"Dog"\x1b[0m,\x1b[0m\n\x1b[0m"id":\x1b[31m3\x1b[0m,\x1b[0m|\x1b[0m"id":\x1b[32m1\x1b[0m,\x1b[0m\n\x1b[0m}\x1b[0m|\x1b[0m}\x1b[0m\n\x1b[0m}\x1b[0m|\x1b[0m}\x1b[0m\n\x1b[0m{\x1b[0m|\x1b[0m{\x1b[0m\n\x1b[0m"1":{\x1b[0m|\x1b[0m"1":{\x1b[0m\n\x1b[0m"name":\x1b[31m"Dog"\x1b[0m,\x1b[0m|\x1b[0m"name":\x1b[32m"Cat"\x1b[0m,\x1b[0m\n\x1b[0m"id":\x1b[31m1\x1b[0m,\x1b[0m|\x1b[0m"id":\x1b[32m2\x1b[0m,\x1b[0m\n\x1b[0m}\x1b[0m|\x1b[0m}\x1b[0m\n\x1b[0m}\x1b[0m|\x1b[0m}\x1b[0m\n\x1b[0m{\x1b[0m|\x1b[0m{\x1b[0m\n\x1b[0m"2":{\x1b[0m|\x1b[0m"2":{\x1b[0m\n\x1b[0m"name":"Elephant",\x1b[0m|\x1b[0m"name":"Elephant",\x1b[0m\n\x1b[0m"id":\x1b[31m2\x1b[0m,\x1b[0m|\x1b[0m"id":\x1b[32m3\x1b[0m,\x1b[0m\n\x1b[0m}\x1b[0m|\x1b[0m}\x1b[0m\n\x1b[0m}\x1b[0m|\x1b[0m}\x1b[0m\n|\n`,
		},
		{
			name:     "nested JSONs with different structures",
			json1:    `{"level1":{"level2":{"name":"Cat","id":3}}}`,
			json2:    `{"level1":{"level2":{"name":"Dog","id":3}}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"level1":{"level2":{"name":\x1b[31m"Cat"\x1b[0m|\x1b[32m"Dog"\x1b[0m}}}\n`,
		},
		{
			name:     "nested JSONs with array length differences",
			json1:    `{"animals":[{"name":"Cat"},{"name":"Dog"},{"name":"Elephant"}]}`,
			json2:    `{"animals":[{"name":"Dog"},{"name":"Cat"},{"apple":"lusiancs"},{"name":"Elephant"}]}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animals":[{"name":\x1b[31m"Cat"\x1b[0m|\x1b[32m"Dog"\x1b[0m},{"name":"Dog"|\x1b[31m"Cat"\x1b[0m}]}\n`,
		},
		{
			name:     "nested JSONs with array differences",
			json1:    `{"animals":[{"name":"Cat"},{"name":"Dog"},{"name":"Elephant"}]}`,
			json2:    `{"animals":[{"name":"Dog"},{"name":"Cat"},{"apple":"lusiancs"}]}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animals":[{"name":\x1b[31m"Cat"\x1b[0m|\x1b[32m"Dog"\x1b[0m},{"name":"Dog"|\x1b[31m"Cat"\x1b[0m}]}\n`,
		},
		{
			name:     "different key-value pairs in nested JSON",
			json1:    `{"animal":{"name":"Cat","attributes":{"color":"black","age":5}}}`,
			json2:    `{"animal":{"name":"Cat","attributes":{"color":"white","age":5}}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animal":{"attributes":{"color":\x1b[31m"black"\x1b[0m|\x1b[32m"white"\x1b[0m}}}\n`,
		},
		{
			name:     "arrays with different elements",
			json1:    `["Cat","Dog","Elephant"]`,
			json2:    `["Dog","Cat","Elephant"]`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n["\x1b[31mCat\x1b[0m"|\x1b[32mDog\x1b[0m]}\n`,
		},
		{
			name:     "nested arrays within objects",
			json1:    `{"animals":{"domestic":["Cat","Dog"],"wild":["Elephant","Lion"]}}`,
			json2:    `{"animals":{"domestic":["Dog","Cat"],"wild":["Lion","Elephant"]}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animals":{"domestic":["\x1b[31mCat\x1b[0m"|\x1b[32mDog\x1b[0m],"wild":["\x1b[31mElephant\x1b[0m"|\x1b[32mLion\x1b[0m]}}\n`,
		},
		{
			name:     "three layered nested maps",
			json1:    `{"level1":{"level2":{"level3":{"name":"Cat","id":3}}}}`,
			json2:    `{"level1":{"level2":{"level3":{"name":"Dog","id":3}}}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"level1":{"level2":{"level3":{"name":\x1b[31m"Cat"\x1b[0m|\x1b[32m"Dog"\x1b[0m}}}}\n`,
		},
		{
			name:     "nested objects with different keys",
			json1:    `{"animal":{"name":"Cat","features":{"furly":"short","tail":"long"}}}`,
			json2:    `{"animal":{"name":"Cat","features":{"fur":"long","tail":"long"}}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animal":{"features":{"fur":\x1b[31m"short"\x1b[0m|\x1b[32m"long"\x1b[0m}}}\n`,
		},
		{
			name:     "deeply nested mixed structures",
			json1:    `{"zoo":{"animals":[{"type":"mammal","name":"Elephant","age":10},{"type":"bird","name":"Parrot","age":2}]}}`,
			json2:    `{"zoo":{"animals":[{"type":"mammal","name":"Elephant","age":10},{"type":"bird","name":"Parrot","age":3}]}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"zoo":{"animals":[{"type":"bird","name":"Parrot","age":\x1b[31m2\x1b[0m|\x1b[32m3\x1b[0m}]}}\n`,
		},
		{
			name:     "complex nested objects and arrays",
			json1:    `{"family":{"parents":[{"name":"Alice","age":40},{"name":"Bob","age":42}],"children":[{"name":"Charlie","age":10},{"name":"Daisy","age":8}]}}`,
			json2:    `{"family":{"parents":[{"name":"Bob","age":42},{"name":"Alice","age":40}],"children":[{"name":"Daisy","age":8},{"name":"Charlie","age":10}]}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"family":{"parents":[{"name":\x1b[31m"Alice"\x1b[0m|\x1b[32m"Bob"\x1b[0m}],"children":[{"name":\x1b[31m"Charlie"\x1b[0m|\x1b[32m"Daisy"\x1b[0m}]}}\n`,
		},
		{
			name:     "different arrays with nested objects",
			json1:    `{"books":[{"title":"Book A","author":{"name":"Author 1"}},{"title":"Book B","author":{"name":"Author 2"}}]}`,
			json2:    `{"books":[{"title":"Book B","author":{"name":"Author 2"}},{"title":"Book A","author":{"name":"Author 1"}}]}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"books":[{"title":\x1b[31m"BookA"\x1b[0m|\x1b[32m"BookB"\x1b[0m},{"title":"BookB","author":{"name":"Author2"}}]}\n`,
		},
		{
			name:     "simple string difference",
			json1:    `"Hello World"`,
			json2:    `"Hello Universe"`,
			expected: `"Hello\x1b[31mWorld\x1b[0m"|\x1b[32mUniverse\x1b[0m"`,
		},
		{
			name:     "array of arrays",
			json1:    `[[1, 2], [3, 4]]`,
			json2:    `[[1, 2], [4, 3]]`,
			expected: `[[1,2],[\x1b[31m3\x1b[0m|\x1b[32m4\x1b[0m,\x1b[31m4\x1b[0m|\x1b[32m3\x1b[0m]]`,
		},
		{
			name:     "map containing array and string",
			json1:    `{"key1": ["a", "b", "c"], "key2": "value1"}`,
			json2:    `{"key1": ["a", "b", "c"], "key2": "value2"}`,
			expected: `{"key1":["a","b","c"],"key2":"\x1b[31mvalue1\x1b[0m"|\x1b[32mvalue2\x1b[0m"}`,
		},
		{
			name:     "array of maps with string differences",
			json1:    `[{"name": "Alice"}, {"name": "Bob"}]`,
			json2:    `[{"name": "Alice"}, {"name": "Charlie"}]`,
			expected: `[{"name":"Alice"},{"name":"\x1b[31mBob\x1b[0m"|\x1b[32mCharlie\x1b[0m"}]`,
		},
		{
			name:     "nested array of strings",
			json1:    `[[["a", "b"], ["c", "d"]], [["e", "f"], ["g", "h"]]]`,
			json2:    `[[["a", "b"], ["d", "c"]], [["e", "f"], ["h", "g"]]]`,
			expected: `[[["a","b"],["\x1b[31mc\x1b[0m|\x1b[32md\x1b[0m","\x1b[31md\x1b[0m|\x1b[32mc\x1b[0m"]],[["e","f"],["\x1b[31mg\x1b[0m|\x1b[32mh\x1b[0m","\x1b[31mh\x1b[0m|\x1b[32mg\x1b[0m"]]]`,
		},
		{
			name:     "complex nested structures with maps and arrays",
			json1:    `{"outer": {"inner": [{"key": "value1"}, {"key": "value2"}], "array": [1, 2, 3]}}`,
			json2:    `{"outer": {"inner": [{"key": "value1"}, {"key": "value3"}], "array": [1, 3, 2]}}`,
			expected: `{"outer":{"inner":[{"key":"value1"},{"key":"\x1b[31mvalue2\x1b[0m"|\x1b[32mvalue3\x1b[0m"}],"array":[1,\x1b[31m2\x1b[0m|\x1b[32m3\x1b[0m,\x1b[31m3\x1b[0m|\x1b[32m2\x1b[0m]}}`,
		},
		{
			name:     "deeply nested arrays and strings",
			json1:    `[[["string1", ["string2", ["string3"]]]]]`,
			json2:    `[[["string1", ["string4", ["string3"]]]]]`,
			expected: `[[["string1",["\x1b[31mstring2\x1b[0m"|\x1b[32mstring4\x1b[0m",["string3"]]]]]`,
		},
		{
			name:     "nested maps with number differences",
			json1:    `{"level1": {"level2": {"value": 10}}}`,
			json2:    `{"level1": {"level2": {"value": 20}}}`,
			expected: `{"level1":{"level2":{"value":\x1b[31m10\x1b[0m|\x1b[32m20\x1b[0m}}}`,
		},
		{
			name:     "array with mixed types",
			json1:    `["string", 123, {"key": "value"}, [1, 2, 3]]`,
			json2:    `["string", 123, {"key": "different"}, [1, 3, 2]]`,
			expected: `["string",123,{"key":"\x1b[31mvalue\x1b[0m"|\x1b[32mdifferent\x1b[0m"},[1,\x1b[31m2\x1b[0m|\x1b[32m3\x1b[0m,\x1b[31m3\x1b[0m|\x1b[32m2\x1b[0m]]`,
		},
		{
			name:     "complex multi-type nested structures",
			json1:    `{"a":[{"b":[{"c":"d"},2,3,{"e":"f"}]},["g","h"]]}`,
			json2:    `{"a":[{"b":[{"c":"d"},3,2,{"e":"f"}]},["h","g"]]}`,
			expected: `{"a":[{"b":[{"c":"d"},\x1b[31m2\x1b[0m|\x1b[32m3\x1b[0m,\x1b[31m3\x1b[0m|\x1b[32m2\x1b[0m,{"e":"f"}]},["\x1b[31mg\x1b[0m"|\x1b[32mh\x1b[0m","\x1b[31mh\x1b[0m"|\x1b[32mg\x1b[0m]]`,
		},
		{
			name:     "empty array to array of maps",
			json1:    `{"nested":{"key":[]}}`,
			json2:    `{"nested":{"key":[{"mapKey1":"value1"},{"mapKey2":"value2"}]}}`,
			expected: `{"nested":{"key":[]|[{"mapKey1":"value1"},{"mapKey2":"value2"}]}}`,
		},
		{
			name:     "array to complex array of maps",
			json1:    `{"nested":{"key":[{"mapKey3":{"innerKey":"innerValue"}},{"mapKey4":"value2", "mapKey7":[3, 4, {"subKey2":"subValue2"}], "mapKey6":{"innerKey2":"innerValue2"}}]}}`,
			json2:    `{"nested":{"key":[{"mapKey1":"value1", "mapKey2":[1, 2, {"subKey":"subValue"}], "mapKey3":{"innerKey":"innerValue"}},{"mapKey4":"value2", "mapKey5":[3, 4, {"subKey2":"subValue2"}], "mapKey6":{"innerKey2":"innerValue2"}}]}}`,
			expected: `{"nested":{"key":[]|[{"mapKey1":"value1","mapKey2":[1,2,{"subKey":"subValue"}],"mapKey3":{"innerKey":"innerValue"}},{"mapKey4":"value2","mapKey5":[3,4,{"subKey2":"subValue2"}],"mapKey6":{"innerKey2":"innerValue2"}}]}}`,
		},
		{
			name:     "complex multi-type nested structures",
			json1:    `{"a":[{"b":[{"c":"d"},2,3,{"e":"f"}]},["g","h"]]}`,
			json2:    `{"a":[{"b":[{"c":"d"},3,2,{"e":"f"}]},["h","g"]]}`,
			expected: `{"a":[{"b":[{"c":"d"},\x1b[31m2\x1b[0m|\x1b[32m3\x1b[0m,\x1b[31m3\x1b[0m|\x1b[32m2\x1b[0m,{"e":"f"}]},["\x1b[31mg\x1b[0m"|\x1b[32mh\x1b[0m","\x1b[31mh\x1b[0m"|\x1b[32mg\x1b[0m]]`,
		},
		{
			name:     "different JSONs with changed keys",
			json1:    `[{"name":"Cat","id":3},{"name":"Dog","id":1},{"name":"Elephant","id":2},{"name":"Bird","id":4}]`,
			json2:    `[{"name":"Dog","id":1},{"animal":"Cat","id":2},{"name":"Elephant","id":3},{"name":"Bird","id":4}]`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"name":"Cat","id":3}|{"animal":"Cat","id":2}`,
		},
		{
			name:     "nested JSONs with array length differences",
			json1:    `{"animals":[{"name":"Cat"},{"name":"Dog"},{"name":"Elephant"}]}`,
			json2:    `{"animals":[{"name":"Dog"},{"name":"Cat"},{"apple":"lusiancs"},{"name":"Elephant"}]}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animals":[{"name":"Cat"}|{"apple":"lusiancs"}]}`,
		},
		{
			name:     "nested JSONs with array differences",
			json1:    `{"animals":[{"name":"Cat"},{"name":"Dog"},{"name":"Elephant"}]}`,
			json2:    `{"animals":[{"name":"Dog"},{"name":"Cat"},{"apple":"lusiancs"}]}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animals":[{"name":"Cat"}|{"apple":"lusiancs"}]}`,
		},
		{
			name:     "different key-value pairs in nested JSON",
			json1:    `{"animal":{"name":"Cat","attributes":{"color":"black","age":5}}}`,
			json2:    `{"animal":{"name":"Cat","attributes":{"color":"white","age":5}}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animal":{"attributes":{"color":\x1b[31m"black"\x1b[0m|\x1b[32m"white"\x1b[0m}}}`,
		},
		{
			name:     "arrays with different elements",
			json1:    `["Cat","Dog","Elephant"]`,
			json2:    `["Dog","Cat","Elephant"]`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n["\x1b[31mCat\x1b[0m"|\x1b[32mDog\x1b[0m]`,
		},
		{
			name:     "nested arrays within objects",
			json1:    `{"animals":{"domestic":["Cat","Dog"],"wild":["Elephant","Lion"]}}`,
			json2:    `{"animals":{"domestic":["Dog","Cat"],"wild":["Lion","Elephant"]}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animals":{"domestic":["\x1b[31mCat\x1b[0m"|\x1b[32mDog\x1b[0m],"wild":["\x1b[31mElephant\x1b[0m"|\x1b[32mLion\x1b[0m]}`,
		},
		{
			name:     "three layered nested maps",
			json1:    `{"level1":{"level2":{"level3":{"name":"Cat","id":3}}}}`,
			json2:    `{"level1":{"level2":{"level3":{"name":"Dog","id":3}}}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"level1":{"level2":{"level3":{"name":\x1b[31m"Cat"\x1b[0m|\x1b[32m"Dog"\x1b[0m}}}}`,
		},
		{
			name:     "nested objects with different keys",
			json1:    `{"animal":{"name":"Cat","features":{"furly":"short","tail":"long"}}}`,
			json2:    `{"animal":{"name":"Cat","features":{"fur":"long","tail":"long"}}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"animal":{"features":{"fur":\x1b[31m"short"\x1b[0m|\x1b[32m"long"\x1b[0m}}}`,
		},
		{
			name:     "deeply nested mixed structures",
			json1:    `{"zoo":{"animals":[{"type":"mammal","name":"Elephant","age":10},{"type":"bird","name":"Parrot","age":2}]}}`,
			json2:    `{"zoo":{"animals":[{"type":"mammal","name":"Elephant","age":10},{"type":"bird","name":"Parrot","age":3}]}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"zoo":{"animals":[{"type":"bird","name":"Parrot","age":\x1b[31m2\x1b[0m|\x1b[32m3\x1b[0m}]}}`,
		},
		{
			name:     "complex nested objects and arrays",
			json1:    `{"family":{"parents":[{"name":"Alice","age":40},{"name":"Bob","age":42}],"children":[{"name":"Charlie","age":10},{"name":"Daisy","age":8}]}}`,
			json2:    `{"family":{"parents":[{"name":"Bob","age":42},{"name":"Alice","age":40}],"children":[{"name":"Daisy","age":8},{"name":"Charlie","age":10}]}}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"family":{"parents":[{"name":\x1b[31m"Alice"\x1b[0m|\x1b[32m"Bob"\x1b[0m}],"children":[{"name":\x1b[31m"Charlie"\x1b[0m|\x1b[32m"Daisy"\x1b[0m}]}}`,
		},
		{
			name:     "different arrays with nested objects",
			json1:    `{"books":[{"title":"Book A","author":{"name":"Author 1"}},{"title":"Book B","author":{"name":"Author 2"}}]}`,
			json2:    `{"books":[{"title":"Book B","author":{"name":"Author 2"}},{"title":"Book A","author":{"name":"Author 1"}}]}`,
			expected: `EXPECTBODY|ACTUALBODY\n-----------------------------------------------------+-----------------------------------------------------\n{"books":[{"title":\x1b[31m"BookA"\x1b[0m|\x1b[32m"BookB"\x1b[0m},{"title":"BookB","author":{"name":"Author2"}}]}`,
		},
		{
			name:     "simple string difference",
			json1:    `"Hello World"`,
			json2:    `"Hello Universe"`,
			expected: `"Hello\x1b[31mWorld\x1b[0m"|\x1b[32mUniverse\x1b[0m"`,
		},
		{
			name:     "array of arrays",
			json1:    `[[1, 2], [3, 4]]`,
			json2:    `[[1, 2], [4, 3]]`,
			expected: `[[1,2],[\x1b[31m3\x1b[0m|\x1b[32m4\x1b[0m,\x1b[31m4\x1b[0m|\x1b[32m3\x1b[0m]]`,
		},
		{
			name:     "map containing array and string",
			json1:    `{"key1": ["a", "b", "c"], "key2": "value1"}`,
			json2:    `{"key1": ["a", "b", "c"], "key2": "value2"}`,
			expected: `{"key1":["a","b","c"],"key2":"\x1b[31mvalue1\x1b[0m"|\x1b[32mvalue2\x1b[0m"}`,
		},
		{
			name:     "array of maps with string differences",
			json1:    `[{"name": "Alice"}, {"name": "Bob"}]`,
			json2:    `[{"name": "Alice"}, {"name": "Charlie"}]`,
			expected: `[{"name":"Alice"},{"name":"\x1b[31mBob\x1b[0m"|\x1b[32mCharlie\x1b[0m"}]`,
		},
		{
			name:     "nested array of strings",
			json1:    `[[["a", "b"], ["c", "d"]], [["e", "f"], ["g", "h"]]]`,
			json2:    `[[["a", "b"], ["d", "c"]], [["e", "f"], ["h", "g"]]]`,
			expected: `[[["a","b"],["\x1b[31mc\x1b[0m|\x1b[32md\x1b[0m","\x1b[31md\x1b[0m|\x1b[32mc\x1b[0m"]],[["e","f"],["\x1b[31mg\x1b[0m|\x1b[32mh\x1b[0m","\x1b[31mh\x1b[0m|\x1b[32mg\x1b[0m"]]]`,
		},
		{
			name:     "complex nested structures with maps and arrays",
			json1:    `{"outer": {"inner": [{"key": "value1"}, {"key": "value2"}], "array": [1, 2, 3]}}`,
			json2:    `{"outer": {"inner": [{"key": "value1"}, {"key": "value3"}], "array": [1, 3, 2]}}`,
			expected: `{"outer":{"inner":[{"key":"value1"},{"key":"\x1b[31mvalue2\x1b[0m"|\x1b[32mvalue3\x1b[0m"}],"array":[1,\x1b[31m2\x1b[0m|\x1b[32m3\x1b[0m,\x1b[31m3\x1b[0m|\x1b[32m2\x1b[0m]}}`,
		},
		{
			name:     "deeply nested arrays and strings",
			json1:    `[[["string1", ["string2", ["string3"]]]]]`,
			json2:    `[[["string1", ["string4", ["string3"]]]]]`,
			expected: `[[["string1",["\x1b[31mstring2\x1b[0m"|\x1b[32mstring4\x1b[0m",["string3"]]]]]`,
		},
		{
			name:     "nested maps with number differences",
			json1:    `{"level1": {"level2": {"value": 10}}}`,
			json2:    `{"level1": {"level2": {"value": 20}}}`,
			expected: `{"level1":{"level2":{"value":\x1b[31m10\x1b[0m|\x1b[32m20\x1b[0m}}}`,
		},
		{
			name:     "array with mixed types",
			json1:    `["string", 123, {"key": "value"}, [1, 2, 3]]`,
			json2:    `["string", 123, {"key": "different"}, [1, 3, 2]]`,
			expected: `["string",123,{"key":"\x1b[31mvalue\x1b[0m"|\x1b[32mdifferent\x1b[0m"},[1,\x1b[31m2\x1b[0m|\x1b[32m3\x1b[0m,\x1b[31m3\x1b[0m|\x1b[32m2\x1b[0m]]`,
		},
		{
			name:     "empty array to array of maps",
			json1:    `{"nested":{"key":[]}}`,
			json2:    `{"nested":{"key":[{"mapKey1":"value1"},{"mapKey2":"value2"}]}}`,
			expected: `{"nested":{"key":[]|[{"mapKey1":"value1"},{"mapKey2":"value2"}]}}`,
		},
		{
			name:     "empty array to complex array of maps",
			json1:    `{"nested":{"key":[]}}`,
			json2:    `{"nested":{"key":[{"mapKey1":"value1", "mapKey2":[1, 2, {"subKey":"subValue"}], "mapKey3":{"innerKey":"innerValue"}},{"mapKey4":"value2", "mapKey5":[3, 4, {"subKey2":"subValue2"}], "mapKey6":{"innerKey2":"innerValue2"}}]}}`,
			expected: `{"nested":{"key":[]|[{"mapKey1":"value1","mapKey2":[1,2,{"subKey":"subValue"}],"mapKey3":{"innerKey":"innerValue"}},{"mapKey4":"value2","mapKey5":[3,4,{"subKey2":"subValue2"}],"mapKey6":{"innerKey2":"innerValue2"}}]}}`,
		},
		{
			name:     "long values and large JSON",
			json1:    `{"longKey":"` + strings.Repeat("a", 1000) + `","nested":{"key1":{"subkey1":"value1"},"key2":{"subkey2":"value2"}}}`,
			json2:    `{"longKey":"` + strings.Repeat("b", 1000) + `","nested":{"key1":{"subkey1":"value1"},"key2":{"subkey2":"value3"}}}`,
			expected: `{"longKey":"` + "\x1b[31m" + strings.Repeat("a", 1000) + "\x1b[0m|\x1b[32m" + strings.Repeat("b", 1000) + "\x1b[0m" + `","nested":{"key2":{"subkey2":"\x1b[31mvalue2\x1b[0m|\x1b[32mvalue3\x1b[0m"}}}`,
		},
		{
			name:     "nested maps with changed array structures",
			json1:    `{"level1":{"level2":{"key1":[]}}}`,
			json2:    `{"level1":{"level2":{"key1":[{"subKey1":"value1"}, "string", 123]}}}`,
			expected: `{"level1":{"level2":{"key1":[]|[{"subKey1":"value1"},"string",123]}}}`,
		},
		{
			name:     "long keys with subtle changes",
			json1:    `{"longKeyWithSimilarTextButSlightlyDifferentEndingA":"value1"}`,
			json2:    `{"longKeyWithSimilarTextButSlightlyDifferentEndingB":"value1"}`,
			expected: `{"longKeyWithSimilarTextButSlightlyDifferentEnding\x1b[31mA\x1b[0m|\x1b[32mB\x1b[0m":"value1"}`,
		},
		{
			name:     "long paragraphs with a random word change",
			json1:    `{"paragraph":"This is a long paragraph with many words. The quick brown fox jumps over the lazy dog. A random word will change in the middle of this sentence."}`,
			json2:    `{"paragraph":"This is a long paragraph with many words. The quick brown fox jumps over the lazy dog. A random word will change in the middle of this phrase."}`,
			expected: `{"paragraph":"...middle of this \x1b[31msentence\x1b[0m|\x1b[32mphrase\x1b[0m."}`,
		},
		{
			name:     "empty array to complex array of maps with subtle changes",
			json1:    `{"nested":{"key":[]}}`,
			json2:    `{"nested":{"key":[{"mapKey1":"value1", "mapKey2":[1, 2, {"subKey":"subValue"}], "mapKey3":{"innerKey":"innerValue"}},{"mapKey4":"value2", "mapKey5":[3, 4, {"subKey2":"subValue3"}], "mapKey6":{"innerKey2":"innerValue2"}}]}}`,
			expected: `{"nested":{"key":[]|[{"mapKey1":"value1","mapKey2":[1,2,{"subKey":"subValue"}],"mapKey3":{"innerKey":"innerValue"}},{"mapKey4":"value2","mapKey5":[3,4,{"subKey2":"\x1b[31msubValue2\x1b[0m|\x1b[32msubValue3\x1b[0m"}],"mapKey6":{"innerKey2":"innerValue2"}}]}}`,
		},
		{
			name:     "long values with subtle changes and long paragraphs",
			json1:    `{"longKey":"This is a long key with many words and a subtle change at the end of this sentence."}`,
			json2:    `{"longKey":"This is a long key with many words and a subtle change at the end of this phrase."}`,
			expected: `{"longKey":"...end of this \x1b[31msentence\x1b[0m|\x1b[32mphrase\x1b[0m."}`,
		},
		{
			name:     "nested maps with changed array structures",
			json1:    `{"level1":{"level2":{"key1":[]}}}`,
			json2:    `{"level1":{"level2":{"key1":[{"subKey1":"value1"}, "string", 123]}}}`,
			expected: `{"level1":{"level2":{"key1":[]|[{"subKey1":"value1"},"string",123]}}}`,
		},
		{
			name:     "complex nested maps with arrays and long values",
			json1:    `{"level1":{"level2":{"level3":{"longKey":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ1c2VyIjp7ImlkIjoxLCJmaXJzdE5hbWUiOiJTdGVybGluZyIsImxhc3ROYW1lIjoiU2F1ZXIiLCJlbWFpbCI6Ik1hc29uLkdvbGRuZXI0OUBob3RtYWlsLmNvbSIsInBhc3N3b3JkIjoiZGFhOTMyMGY1YzU4NDRiODRiMjhlMDE2YjRiOGM0MGIiLCJjcmVhdGVkQXQiOiIyMDIzLTEyLTA4VDE4OjE2OjQxLjYzOFoiLCJ1cGRhdGVkQXQiOm51bGwsImRlbGV0ZWRBdCI6bnVsbH0sImlhdCI6MTcxOTM0MzYzOCwiZXhwIjoxNzE5NDMwMDM4fQ.Kgm3Lmbg97M_QQP5Gn9q4suRYEF7_n4ITqehV4i7t_s is a very long value with many descriptive words and phrases to make it lengthy."}}}}`,
			json2:    `{"level1":{"level2":{"level3":{"longKey":"This is a very long value with many descriptive words and phrases to make it extensive."}}}}`,
			expected: `{"level1":{"level2":{"level3":{"longKey":"...to make it \x1b[31mlengthy\x1b[0m|\x1b[32mextensive\x1b[0m."}}}}`,
		},
		{
			name:     "empty array to array of maps with complex nested structures",
			json1:    `{"nested":{"key":[]}}`,
			json2:    `{"nested":{"key":[{"mapKey1":"value1", "mapKey2":[{"subKey1":"value2"}, "string", 123], "mapKey3":{"innerKey":"innerValue"}},{"mapKey4":"value3", "mapKey5":[{"subKey2":"value4"}, "anotherString", 456], "mapKey6":{"innerKey2":"innerValue2"}}]}}`,
			expected: `{"nested":{"key":[]|[{"mapKey1":"value1","mapKey2":[{"subKey1":"value2"},"string",123],"mapKey3":{"innerKey":"innerValue"}},{"mapKey4":"value3","mapKey5":[{"subKey2":"value4"},"anotherString",456],"mapKey6":{"innerKey2":"innerValue2"}}]}}`,
		},
		{
			name:     "long nested structures with slight key changes",
			json1:    `{"level1":{"level2":{"level3":{"longKeyWithMinorChangeA":"This is a very long value that remains mostly the same."}}}}`,
			json2:    `{"level1":{"level2":{"level3":{"longKeyWithMinorChangeB":"This is a very long value that remains mostly the same."}}}}`,
			expected: `{"level1":{"level2":{"level3":{"longKeyWithMinorChange\x1b[31mA\x1b[0m|\x1b[32mB\x1b[0m":"This is a very long value that remains mostly the same."}}}}`,
		},
		{
			name:     "long nested structures with slight key changes",
			json1:    `{"level1":{"level2":{"level3":{"longKeyWithMinorChangeA":"This is a very long value that remains mostly the same."}}}}`,
			json2:    `{"level1":{"level2":{"level3":{"longKeyWithMinorChangeA":"This is a very long value that remains mostly the same."}}}}`,
			expected: `{"level1":{"level2":{"level3":{"longKeyWithMinorChange\x1b[31mA\x1b[0m|\x1b[32mB\x1b[0m":"This is a very long value that remains mostly the same."}}}}`,
		},
		{
			name:     "long paragraphs with nested arrays and maps",
			json1:    `{"nested":{"longParagraph":"This is a long paragraph. It contains multiple sentences. Each sentence has many words. One sentence will be different in the second JSON."}}`,
			json2:    `{"nested":{"longParagraph":"This is a long paragraph. It contains multiple sentences. Each sentence has many words. One phrase will be different in the second JSON."}}`,
			expected: `{"nested":{"longParagraph":"...One \x1b[31msentence\x1b[0m|\x1b[32mphrase\x1b[0m will be different..."}}`,
		},
		{
			name:     "complex nested structures with arrays and subtle changes",
			json1:    `{"level1":{"level2":{"key1":[{"subKey1":"value1"}, {"subKey2":"value2"}, "string", 123]}}}`,
			json2:    `{"level1":{"level2":{"key1":[{"subKey1":"value1"}, {"subKey2":"value3"}, "string", 123]}}}`,
			expected: `{"level1":{"level2":{"key1":[{"subKey2":"\x1b[31mvalue2\x1b[0m|\x1b[32mvalue3\x1b[0m"}]}}}`,
		},
		{
			name:     "random key change in nested JSON",
			json1:    `{"level1":{"level2":{"name":"Cat","id":3}}}`,
			json2:    `{"level1":{"level2":{"animal":"Cat","id":3}}}`,
			expected: `{"level1":{"level2":{"\x1b[31mname\x1b[0m|\x1b[32manimal\x1b[0m":"Cat"}}}`,
		},
		{
			name:     "random key change in array of objects",
			json1:    `[{"name":"Cat","id":3},{"name":"Dog","id":1},{"name":"Elephant","id":2}]`,
			json2:    `[{"animal":"Cat","id":3},{"name":"Dog","id":1},{"name":"Elephant","id":2}]`,
			expected: `[{\x1b[31mname\x1b[0m|\x1b[32manimal\x1b[0m:"Cat"}]`,
		},
		{
			name:     "nested JSON with random key change",
			json1:    `{"animals":[{"name":"Cat"},{"name":"Dog"},{"name":"Elephant"}]}`,
			json2:    `{"animals":[{"type":"Cat"},{"name":"Dog"},{"name":"Elephant"}]}`,
			expected: `{"animals":[{\x1b[31mname\x1b[0m|\x1b[32mtype\x1b[0m:"Cat"}]}`,
		},
		{
			name:     "deeply nested JSON with random key change",
			json1:    `{"level1":{"level2":{"level3":{"name":"Cat","id":3}}}}`,
			json2:    `{"level1":{"level2":{"level3":{"species":"Cat","id":3}}}}`,
			expected: `{"level1":{"level2":{"level3":{"\x1b[31mname\x1b[0m|\x1b[32mspecies\x1b[0m":"Cat"}}}}`,
		},
		{
			name:     "random key change in map containing array",
			json1:    `{"key1": ["a", "b", "c"], "key2": "value1"}`,
			json2:    `{"key1": ["a", "b", "c"], "keyX": "value1"}`,
			expected: `{"key1":["a","b","c"],\x1b[31mkey2\x1b[0m|\x1b[32mkeyX\x1b[0m:"value1"}`,
		},
		{
			name:     "random key change in array of maps",
			json1:    `[{"name": "Alice"}, {"name": "Bob"}]`,
			json2:    `[{"name": "Alice"}, {"person": "Bob"}]`,
			expected: `[{"name":"Alice"},{"\x1b[31mname\x1b[0m|\x1b[32mperson\x1b[0m:"Bob"}]`,
		},
		{
			name:     "random key change in nested objects",
			json1:    `{"animal":{"name":"Cat","attributes":{"color":"black","age":5}}}`,
			json2:    `{"animal":{"name":"Cat","characteristics":{"color":"black","age":5}}}`,
			expected: `{"animal":{"\x1b[31mattributes\x1b[0m|\x1b[32mcharacteristics\x1b[0m":{"color":"black","age":5}}}`,
		},
		{
			name:     "nested maps with random key change",
			json1:    `{"level1":{"level2":{"key1":{"subKey":"value"}}}}`,
			json2:    `{"level1":{"level2":{"key1":{"attribute":"value"}}}}`,
			expected: `{"level1":{"level2":{"key1":{"\x1b[31msubKey\x1b[0m|\x1b[32mattribute\x1b[0m":"value"}}}}}`,
		},
		{
			name:     "random key change in complex nested structures",
			json1:    `{"outer": []`,
			json2:    `{"outer": [Vary]`,
			expected: `{"outer":{"inner":[{\x1b[31mkey\x1b[0m|\x1b[32mattribute\x1b[0m:"value1"}],"array":[1,2,3]}}`,
		},
		{
			name:     "random key change in deeply nested mixed structures",
			json1:    `{"zoo":{"animals":[{"type":"mammal","name":"Elephant","age":10},{"type":"bird","name":"Parrot","age":2}]}}`,
			json2:    `{"zoo":{"animals":[{"species":"mammal","name":"Elephant","age":10},{"type":"bird","name":"Parrot","age":2}]}}`,
			expected: `{"zoo":{"animals":[{\x1b[31mtype\x1b[0m|\x1b[32mspecies\x1b[0m:"mammal"}]}}`,
		},
		{
			name:     "random key change in deeply nested mixed structures",
			json1:    `{"Etag":[W/"1c0-4VkjzPwyKEH0Xy9lGO28f/cyPk4"],"Vary":[]}`,
			json2:    `{"Etag":[W/"1c0-8j/k9MOCbWGtKgVesjFGmY6dEAs"],"Vary":["Origin"]}`,
			expected: `{"zoo":{"animals":[{\x1b[31mtype\x1b[0m|\x1b[32mspecies\x1b[0m:"mammal"}]}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := replay.SprintJSONDiff([]byte(tt.json1), []byte(tt.json2), "body", map[string][]string{})
			if err != nil {
				t.Errorf("sprintJSONDiff(%q, %q) returned an error: %v", tt.json1, tt.json2, err)
			}
			if escapeSpecialChars(result) != tt.expected {
				fmt.Println(tt.name)
				fmt.Println((result)) // Using Print to avoid adding a newline at the end
			}
		})
	}
}
