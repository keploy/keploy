package pkg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/araddon/dateparse"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/yudai/gojsondiff"
	"github.com/yudai/gojsondiff/formatter"
	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/grpc/utils"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func IsTime(stringDate string) bool {
	s := strings.TrimSpace(stringDate)
	_, err := dateparse.ParseAny(s)
	return err == nil
}

func AddHttpBodyToMap(body string, m map[string][]string) error {
	// add body
	if json.Valid([]byte(body)) {
		var result interface{}

		err := json.Unmarshal([]byte(body), &result)
		if err != nil {
			return err
		}
		j := Flatten(result)
		for k, v := range j {
			nk := "body"
			if k != "" {
				nk = nk + "." + k
			}
			m[nk] = v
		}
	} else {
		// add it as raw text
		m["body"] = []string{body}
	}
	return nil
}

func FlattenHttpResponse(h http.Header, body string) (map[string][]string, error) {
	m := map[string][]string{}
	for k, v := range h {
		m["header."+k] = []string{strings.Join(v, "")}
	}
	err := AddHttpBodyToMap(body, m)
	if err != nil {
		return m, err
	}
	return m, nil
}

func FindNoisyFields(m map[string][]string, comparator func(string, []string) bool) []string {
	var noise []string
	for k, v := range m {
		if comparator(k, v) {
			noise = append(noise, k)
		}
	}
	return noise
}

// Flatten takes a map and returns a new one where nested maps are replaced
// by dot-delimited keys.
// examples of valid jsons - https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/JSON/parse#examples
func Flatten(j interface{}) map[string][]string {
	if j == nil {
		return map[string][]string{"": {""}}
	}
	o := make(map[string][]string)
	x := reflect.ValueOf(j)
	switch x.Kind() {
	case reflect.Map:
		m, ok := j.(map[string]interface{})
		if !ok {
			return map[string][]string{}
		}
		for k, v := range m {
			nm := Flatten(v)
			for nk, nv := range nm {
				fk := k
				if nk != "" {
					fk = fk + "." + nk
				}
				o[fk] = nv
			}
		}
	case reflect.Bool:
		o[""] = []string{strconv.FormatBool(x.Bool())}
	case reflect.Float64:
		o[""] = []string{strconv.FormatFloat(x.Float(), 'E', -1, 64)}
	case reflect.String:
		o[""] = []string{x.String()}
	case reflect.Slice:
		child, ok := j.([]interface{})
		if !ok {
			return map[string][]string{}
		}
		for _, av := range child {
			nm := Flatten(av)
			for nk, nv := range nm {
				if ov, exists := o[nk]; exists {
					o[nk] = append(ov, nv...)
				} else {
					o[nk] = nv
				}
			}
		}
	default:
		fmt.Println("found invalid value in json", j, x.Kind())
	}
	return o
}

func IsValidPath(s string) bool {
	return !strings.HasPrefix(s, "/etc/passwd") && !strings.Contains(s, "../")
}

// sanitiseInput sanitises user input strings before logging them for safety, removing newlines
// and escaping HTML tags. This is to prevent log injection, including forgery of log records.
// Reference: https://www.owasp.org/index.php/Log_Injection
func SanitiseInput(s string) string {
	re := regexp.MustCompile(`(\n|\n\r|\r\n|\r)`)
	return html.EscapeString(string(re.ReplaceAll([]byte(s), []byte(""))))
}

func CompareHeaders(h1 http.Header, h2 http.Header, res *[]models.HeaderResult, noise map[string]string) bool {
	if res == nil {
		return false
	}
	match := true
	_, isHeaderNoisy := noise["header"]
	for k, v := range h1 {
		// Ignore go http router default headers
		// if k == "Date" || k == "Content-Length" || k == "date" || k == "connection" {
		// 	continue
		// }
		_, isNoisy := noise[k]
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h2[k]
		if !isNoisy {
			if !ok {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: nil,
						},
					})
				}

				match = false
				continue
			}
			if len(v) != len(val) {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: val,
						},
					})
				}
				match = false
				continue
			}
			for i, e := range v {
				if val[i] != e {
					if checkKey(res, k) {
						*res = append(*res, models.HeaderResult{
							Normal: false,
							Expected: models.Header{
								Key:   k,
								Value: v,
							},
							Actual: models.Header{
								Key:   k,
								Value: val,
							},
						})
					}
					match = false
					continue
				}
			}
		}
		if checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: v,
				},
				Actual: models.Header{
					Key:   k,
					Value: val,
				},
			})
		}
	}
	for k, v := range h2 {
		// Ignore go http router default headers
		// if k == "Date" || k == "Content-Length" || k == "date" || k == "connection" {
		// 	continue
		// }
		_, isNoisy := noise[k]
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h1[k]
		if isNoisy && checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: val,
				},
				Actual: models.Header{
					Key:   k,
					Value: v,
				},
			})
			continue
		}
		if !ok {
			if checkKey(res, k) {
				*res = append(*res, models.HeaderResult{
					Normal: false,
					Expected: models.Header{
						Key:   k,
						Value: nil,
					},
					Actual: models.Header{
						Key:   k,
						Value: v,
					},
				})
			}

			match = false
		}
	}
	return match
}

func checkKey(res *[]models.HeaderResult, key string) bool {
	for _, v := range *res {
		if key == v.Expected.Key {
			return false
		}
	}
	return true
}

func Contains(elems []string, v string) bool {
	for _, s := range elems {
		if v == s {
			return true
		}
	}
	return false
}
func FilterFields(r interface{}, filter []string, logger *zap.Logger) interface{} { //This filters the headers that the user does not want to record
	for _, v := range filter {
		fields := strings.Split(v, ".")
		if len(fields) < 3 {
			logger.Error(fmt.Sprintf("failed to filter a tcs field `%v` due to invalid format. Format should be `<req_OR_resp_OR_all>.<header_OR_body>.<FIELD_NAME>`", v))
			continue
		}
		fieldType := fields[0]  //req, resp, all
		fieldValue := fields[1] //header, body
		fieldName := fields[2]  //name of the header or body

		switch i := r.(type) {
		case models.TestCase: //This is for the case when the user wants to filter the headers of the testcases
			// i := r.(models.TestCase)
			if fieldType == "req" || fieldType == "all" {
				fieldRegex := regexp.MustCompile(fieldName)
				switch fieldValue {
				case "header": // pair with matching key is filtered from request headers
					for k := range i.HttpReq.Header { //If the regex matches the header name, delete it
						if fieldRegex.MatchString(k) {
							delete(i.HttpReq.Header, k)
						}
					}
					// TODO: Filter for request body
				}
			}
			if fieldType == "resp" || fieldType == "all" {
				fieldRegex := regexp.MustCompile(fieldName)
				switch fieldValue {
				case "header": // filters pair with matching key from the response headers
					for k := range i.HttpResp.Header {
						if fieldRegex.MatchString(k) {
							delete(i.HttpResp.Header, k)
						}
					}
					// TODO: Filter for response body
				}
			}
		case *proto.Mock_SpecSchema: //This is for the case when the user wants to filter the headers of the mocks
			// i := r.(*proto.Mock_SpecSchema)
			if fieldType == "req" || fieldType == "all" {
				fieldRegex := regexp.MustCompile(fieldName)
				switch fieldValue {
				case "header": // pair with matching key is filtered from request headers
					for k := range i.Req.Header {
						if fieldRegex.MatchString(k) {
							delete(i.Req.Header, k)
						}
					}
					// TODO: Filter for response body
				}
			}
			if fieldType == "resp" || fieldType == "all" {
				fieldRegex := regexp.MustCompile(fieldName)
				switch fieldValue {
				case "header": // filters pair with matching key from the response headers
					for k := range i.Res.Header {
						if fieldRegex.MatchString(k) {
							delete(i.Res.Header, k)
						}
					}
				}
			}
		}
	}
	return r
}

// replaceUrlDomain changes the Domain of the full urlStr to domain
func replaceUrlDomain(urlStr string, domain string, logger *zap.Logger) (*url.URL, error) {
	replaceUrl, err := url.Parse(urlStr)
	if err != nil {
		logger.Error("failed to replace http.Request domain field due to error while parsing url", zap.Error(err))
		return replaceUrl, err
	}
	replaceUrl.Host = domain // changes the Domain of parsed url
	return replaceUrl, nil
}

// ReplaceFields replaces the http test-case Request fields to values from the "replace" map.
func ReplaceFields(r interface{}, replace map[string]string, logger *zap.Logger) interface{} {
	for k, v := range replace {
		fields := strings.Split(k, ".")
		fieldType := fields[0] //header, domain, method, proto_major, proto_minor

		switch fieldType {
		case "header": // FORMAT should be "header.key":"val1 | val2 | val3"
			newHeader := strings.Split(v, " | ") //The value of the header is a string of the form "value1 | value2"
			if len(fields) > 1 {
				switch i := r.(type) {
				case models.TestCase:
					i.HttpReq.Header[fields[1]] = newHeader
				case *proto.Mock_SpecSchema:
					i.Req.Header[fields[1]] = utils.ToStrArr(newHeader)
				}
			} else {
				logger.Error("failed to replace http.Request header field due to no header key provided. The format should be `map[string]string{'header.Accept': 'val1 | val2 | val3'}`")
			}
		case "domain":
			switch i := r.(type) {
			case models.TestCase:
				if replacedUrl, err := replaceUrlDomain(i.HttpReq.URL, v, logger); err == nil {
					i.HttpReq.URL = replacedUrl.String()
				}
			case *proto.Mock_SpecSchema:
				if replacedUrl, err := replaceUrlDomain(i.Req.URL, v, logger); err == nil {
					i.Req.URL = replacedUrl.String()

				}
			}
		case "method":
			switch i := r.(type) {
			case models.TestCase:
				i.HttpReq.Method = models.Method(v)
			case *proto.Mock_SpecSchema:
				i.Req.Method = v
				i.Metadata["operation"] = v
			}
		case "proto_major":
			protomajor, err := strconv.Atoi(v)
			if err != nil {
				logger.Error("failed to replace http.Request proto_major field", zap.Error(err))
			}
			switch i := r.(type) {
			case models.TestCase:
				i.HttpReq.ProtoMajor = protomajor
			case *proto.Mock_SpecSchema:
				i.Req.ProtoMajor = int64(protomajor)
			}
		case "proto_minor":
			protominor, err := strconv.Atoi(v)
			if err != nil {
				logger.Error("failed to replace http.Request proto_minor field", zap.Error(err))
			}
			switch i := r.(type) {
			case models.TestCase:
				i.HttpReq.ProtoMinor = protominor
			case *proto.Mock_SpecSchema:
				i.Req.ProtoMinor = int64(protominor)
			}
		default:
			logger.Error("Invalid format for replace map keys. Possible values for keys are `header, domain, method, proto_major, proto_minor`")
		}
	}
	return r
}

/*
 * Prints a nice diff table where the left is the expect and the right is the
 * actual. Its called generic because it works with whatever string. For
 * JSON-based diffs use JSONDiff
 */
func GenericDiff(expect, actual string) string {

	// Offset will be where the string start to unmatch
	offset, _ := diffStr(expect, actual)

	// Color of the unmatch, can be changed
	c := color.FgMagenta

	exp := breakLineWithColor(expect, &c, offset)
	act := breakLineWithColor(actual, &c, offset)

	return expectActualTable(exp, act)
}

/* This will return the json diffs in a beautifull way. It will in fact
 * create a colorized table-based expect-response string and print it.
 * on the left-side there'll be the expect and on the right the actual
 * response. Its important to mention the inputs must to be a json. If
 * the body isnt in the rest-api formats (what means it is not json-based)
 * its better to use a generic diff output as the DiffOutput I've made.
 */
func JSONDiff(json1 []byte, json2 []byte) string {
	diffString := calculateDiffs(json1, json2)
	expect, actual := separateAndColorize(diffString)
	result := expectActualTable(expect, actual)
	return result
}

// Find the diff between two strings returning index where
// the difference begin
func diffStr(s1, s2 string) (int, bool) {
	diff := false
	i := -1

	// Check if one string is smaller than another, if so theres a diff
	if len(s1) < len(s2) {
		i = len(s1)
		diff = true
	} else if len(s2) < len(s1) {
		diff = true
		i = len(s2)
	}

	// Check for unmatched characters
	for i := 0; i < len(s1) && i < len(s2); i++ {
		if s1[i] != s2[i] {
			return i, true
		}
	}

	return i, diff
}

// Declarated here so at each new test we dont need to recreate it again
var diff = gojsondiff.New()

// Will perform the calculation of the diffs, returning a string that
// containes the lines that does not match represented by either a
// minus or add symbol followed by the respective line. OBS: For jsons
func calculateDiffs(json1 []byte, json2 []byte) string {
	dObj, _ := diff.Compare(json1, json2)

	var jsonObject map[string]interface{}
	json.Unmarshal([]byte(json1), &jsonObject)

	diffString, _ := formatter.NewAsciiFormatter(jsonObject, formatter.AsciiFormatterConfig{
		ShowArrayIndex: true,
		Coloring:       false,
	}).Format(dObj)

	return diffString
}

// Will receive a string that has the differences represented
// by a plus or a minus sign and separate it. Just works with json
func separateAndColorize(diffStr string) (string, string) {
	expect, actual := "", ""

	diffLines := strings.Split(diffStr, "\n")

	for _, line := range diffLines {
		if len(line) > 0 {
			if line[0] == '-' {
				c := color.FgRed
				expect += breakLineWithColor(line, &c, 0)
			} else if line[0] == '+' {
				c := color.FgGreen
				actual += breakLineWithColor(line, &c, 0)
			} else {
				expect += breakLineWithColor(line, nil, 0)
				actual += breakLineWithColor(line, nil, 0)
			}
		}
	}
	return expect, actual
}

// Will colorize the line and do the job of break it if it pass maxLength,
// always respecting the reset of ascii colors before the break line to dont
func breakLineWithColor(input string, c *color.Attribute, offset int) string {
	var output []string
	var paint func(a ...interface{}) string
	colorize := false
	maxLength := 55

	if c != nil {
		colorize = true
		paint = color.New(*c).SprintFunc()
	}

	for i := 0; i < len(input); i += maxLength {
		end := i + maxLength

		if end > len(input) {
			end = len(input)
		}

		if colorize && i+maxLength > offset {
			paintedStart := i
			if paintedStart < offset {
				paintedStart = offset
			}
			prePaint := input[i:paintedStart]
			postPaint := paint(input[paintedStart:end])
			substr := prePaint + postPaint + "\n"
			output = append(output, substr)
		} else {
			substr := input[i:end] + "\n"
			output = append(output, substr)
		}
	}
	return strings.Join(output, "")
}

// Will return a string in a two columns table where the left
// side is the expected string and the right is the actual
func expectActualTable(exp string, act string) string {
	buf := &bytes.Buffer{}
	table := tablewriter.NewWriter(buf)

	table.SetHeader([]string{"Expect", "Actual"})
	table.SetAutoWrapText(false)
	table.Append([]string{exp, act})
	table.Render()
	return buf.String()
}
