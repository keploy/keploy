package templates

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// Run is the template state of one replay of one test set: each key's value
// as recorded (config.yaml) and as this run has it now. Nothing else holds
// template state; the matcher never sees it.
type Run struct {
	logger   *zap.Logger
	recorded map[string]string
	values   map[string]string
	secrets  map[string]string
	// unusable names each config.yaml or secret.yaml key whose value is a map
	// or a list, and why: a placeholder stands for one value, so Check
	// refuses every placeholder that names such a key.
	unusable map[string]string

	mu     sync.Mutex
	warned map[string]bool
}

// NewRun reads a test set's config.yaml template map and secret.yaml values.
// Each value is kept as its exact text (1234567, never 1.234567e+06). A map or
// a list is not a value a placeholder can stand for: such a key is kept as
// unusable, and only the test cases whose placeholders name it are refused
// (Check), so it never stops a run.
func NewRun(logger *zap.Logger, template, secret map[string]interface{}) *Run {
	if logger == nil {
		logger = zap.NewNop()
	}
	r := &Run{
		logger:   logger,
		recorded: make(map[string]string, len(template)),
		secrets:  make(map[string]string, len(secret)),
		unusable: map[string]string{},
		warned:   map[string]bool{},
	}
	for k, v := range template {
		text, err := ValueText(v)
		if err != nil {
			r.unusable[k] = fmt.Sprintf("config.yaml's template gives %q a value that %s", k, err)
			continue
		}
		r.recorded[k] = text
	}
	for k, v := range secret {
		text, err := ValueText(v)
		if err != nil {
			r.unusable[secretPrefix+k] = fmt.Sprintf("secret.yaml gives %q a value that %s", k, err)
			continue
		}
		r.secrets[k] = text
	}
	r.Reset()
	return r
}

// Reset puts every key back at its recorded value: the start of a pass over
// the test set.
func (r *Run) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = make(map[string]string, len(r.recorded))
	for k, v := range r.recorded {
		r.values[k] = v
	}
}

// Empty reports whether the test set defines no template and no secret.
func (r *Run) Empty() bool {
	return len(r.recorded) == 0 && len(r.secrets) == 0 && len(r.unusable) == 0
}

// Defined reports whether the test set defines a placeholder's key: a
// config.yaml template key, or a secret.yaml key under ".secret.". A key
// defined with a map or a list is defined, and unusable (Unusable).
func (r *Run) Defined(p Placeholder) bool {
	if _, bad := r.unusable[p.Key]; bad {
		return true
	}
	if name, ok := p.Secret(); ok {
		_, has := r.secrets[name]
		return has
	}
	_, has := r.recorded[p.Key]
	return has
}

// Unusable says why a placeholder's key cannot be used, when the test set
// defines it with a map or a list.
func (r *Run) Unusable(p Placeholder) (string, bool) {
	why, bad := r.unusable[p.Key]
	return why, bad
}

// Apply gives each captured key the value it captured, as the test case that
// captured it would on a pass where it runs. A replay that does not re-run a
// test case in a pass applies what that test case last captured at its place
// in the set, so every later test case sees the value a whole pass gives it.
func (r *Run) Apply(caps []Capture) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range caps {
		if _, ok := r.recorded[c.Key]; ok {
			r.values[c.Key] = c.Value
		}
	}
}

// fork is a copy of r to resolve a response against without moving r: the
// same recorded values, secrets and unusable keys, each key at r's value
// now. It warns of nothing, r having warned of the same text already.
func (r *Run) fork() *Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := &Run{
		logger:   zap.NewNop(),
		recorded: copyStringMap(r.recorded),
		values:   copyStringMap(r.values),
		secrets:  copyStringMap(r.secrets),
		unusable: copyStringMap(r.unusable),
		warned:   map[string]bool{},
	}
	return f
}

// Values is a copy of each key's current value.
func (r *Run) Values() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out
}

// current is a placeholder's value now; false when its key is undefined.
func (r *Run) current(p Placeholder) (string, bool) {
	if name, ok := p.Secret(); ok {
		v, has := r.secrets[name]
		return v, has
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, has := r.values[p.Key]
	return v, has
}

// recordedValue is a placeholder's config.yaml value.
func (r *Run) recordedValue(p Placeholder) (string, bool) {
	if name, ok := p.Secret(); ok {
		v, has := r.secrets[name]
		return v, has
	}
	v, has := r.recorded[p.Key]
	return v, has
}

func (r *Run) set(key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
}

// warnOnce logs a warning once per run for each distinct message, fields and
// value. The value is a placeholder's, the application's or a secret's, so
// the line carries it only as logcontent allows; the warning is said once per
// value either way.
func (r *Run) warnOnce(msg, value string, fields ...zap.Field) {
	key := msg + "\x00value=" + value
	for _, f := range fields {
		key += "\x00" + f.Key + "=" + f.String
	}
	r.mu.Lock()
	seen := r.warned[key]
	r.warned[key] = true
	r.mu.Unlock()
	if !seen {
		r.logger.Warn(msg, append(fields, logcontent.String("value", value))...)
	}
}

// ValueText is a config.yaml template value as the exact text a placeholder
// stands for: a number as its plain digits (1234567, never 1.234567e+06). A
// map or a list is no such value, and an error.
func ValueText(v interface{}) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int8, int16, int32, int64:
		return fmt.Sprintf("%d", t), nil
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", t), nil
	case float32:
		return formatFloat(float64(t)), nil
	case float64:
		return formatFloat(t), nil
	case json.Number:
		return t.String(), nil
	}
	return "", fmt.Errorf("is a %s, not a string, a number or a boolean", shapeOf(v))
}

// shapeOf names what a config.yaml value is when it is not a scalar.
func shapeOf(v interface{}) string {
	switch reflect.ValueOf(v).Kind() {
	case reflect.Map:
		return "map"
	case reflect.Slice, reflect.Array:
		return "list"
	}
	return fmt.Sprintf("%T", v)
}

// formatFloat writes a float as plain decimal digits: 1234567 stays 1234567.
func formatFloat(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

var intRe = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)

// typed is value as the JSON a typed placeholder writes outside a string:
// a number for int and float, which must be one. false when value is not a
// number of that type.
func typed(typ, value string) (string, bool) {
	switch typ {
	case TypeInt:
		return value, intRe.MatchString(value)
	case TypeFloat:
		if m := numberRe.FindString(value); m == value && value != "" {
			return value, true
		}
		return value, false
	}
	return `"` + jsonEscape(value) + `"`, true
}

// fitsType reports whether text captured from an answer is of a capture's
// type: anything non-empty for string, an integer for int, a number for float.
func fitsType(typ, text string) bool {
	if text == "" {
		return false
	}
	switch typ {
	case TypeInt:
		return intRe.MatchString(text)
	case TypeFloat:
		return numberRe.FindString(text) == text
	}
	return true
}

// Recorded is key's config.yaml (recorded) value.
func (r *Run) Recorded(key string) (string, bool) {
	v, ok := r.recorded[key]
	return v, ok
}

// RequestVariable is a template variable a test case's request sends: its
// key, its value in the run now, and its config.yaml (recorded) value.
type RequestVariable struct {
	Key, Value, Recorded string
}

// RequestVariables are the variables req sends, by key: each key a placeholder
// of its URL, query, headers, body or form names, that config.yaml defines and
// that is usable (a secret is the literal it redacts, never a variable).
func (r *Run) RequestVariables(req models.HTTPReq) []RequestVariable {
	texts := []string{req.URL, req.Body}
	for _, v := range req.URLParams {
		texts = append(texts, v)
	}
	for _, v := range req.Header {
		texts = append(texts, v)
	}
	for _, f := range req.Form {
		texts = append(texts, f.Values...)
	}
	seen := map[string]bool{}
	var out []RequestVariable
	for _, t := range texts {
		for _, p := range Find(t) {
			if _, secret := p.Secret(); secret || seen[p.Key] {
				continue
			}
			if _, bad := r.Unusable(p); bad {
				continue
			}
			now, ok := r.current(p)
			if !ok {
				continue
			}
			rec, _ := r.recordedValue(p)
			seen[p.Key] = true
			out = append(out, RequestVariable{Key: p.Key, Value: now, Recorded: rec})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
