package pkg

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"net/url"

	"github.com/araddon/dateparse"
	"go.keploy.io/server/pkg/models"
	proto "go.keploy.io/server/grpc/regression"
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
func FilterFields(r interface{}, filter []string) interface{} { //This filters the headers that the user does not want to record
	for _, v := range filter {
		fieldType := strings.Split(v, ".")[0]  //req, resp, all
		fieldValue := strings.Split(v, ".")[1] //header, body
		fieldName := strings.Split(v, ".")[2]  //name of the header or body
		switch r.(type) {
		case models.TestCase: //This is for the case when the user wants to filter the headers of the testcases
			i := r.(models.TestCase)
			if fieldType == "req" || fieldType == "all" {
				fieldRegex := regexp.MustCompile(fieldName)
				switch fieldValue {
				case "header":
					for k := range i.HttpReq.Header { //If the regex matches the header name, delete it
						if fieldRegex.MatchString(k) == true {
							delete(i.HttpReq.Header, k)
						}
					}
				}
			}
			if fieldType == "resp" || fieldType == "all" {
				fieldRegex := regexp.MustCompile(fieldName)
				switch fieldValue {
				case "header":
					for k, _ := range i.HttpResp.Header {
						if fieldRegex.MatchString(k) == true {
							delete(i.HttpResp.Header, k)
						}
					}
				}
			}
		case *proto.Mock_SpecSchema: //This is for the case when the user wants to filter the headers of the mocks
			i := r.(*proto.Mock_SpecSchema)
			if fieldType == "req" || fieldType == "all" {
				fieldRegex := regexp.MustCompile(fieldName)
				switch fieldValue {
				case "header":
					for k := range i.Req.Header {
						if fieldRegex.MatchString(k) == true {
							delete(i.Req.Header, k)
						}
					}
				}
			}
			if fieldType == "resp" || fieldType == "all" {
				fieldRegex := regexp.MustCompile(fieldName)
				switch fieldValue {
				case "header":
					for k := range i.Res.Header {
						if fieldRegex.MatchString(k) == true {
							delete(i.Res.Header, k)
						}
					}
				}
			}
		}
	}
	return r
}
func ReplaceFields(r models.TestCase, replace map[string]string) models.TestCase { //For replacing the values of fields in the testcase.
	for k, v := range replace {
		fieldType := strings.Split(k, ".")[0] //header, domain, method, proto_major, proto_minor
			switch fieldType {
			case "header":
				newHeader := strings.Split(v, "|") //The value of the header is a string of the form "value1|value2"
				if len(strings.Split(k, ".")) > 1 {
					r.HttpReq.Header[strings.Split(k, ".")[1]] = newHeader
				}else{
					fmt.Println("No header name provided")
				}
			case "domain":
				replaceUrl, err := url.Parse(r.HttpReq.URL)
				if err != nil {
					fmt.Println("Error while parsing url", err)
				}
				replaceUrl.Host = v
				r.HttpReq.URL = replaceUrl.String()
			case "method":
				r.HttpReq.Method = models.Method(v)
			case "proto_major":
				protomajor, err := strconv.Atoi(v)
				if err != nil {
					fmt.Println("Error while converting proto_major to int", err)
				}
				r.HttpReq.ProtoMajor = protomajor
			case "proto_minor":
				protominor, err := strconv.Atoi(v)
				if err != nil {
					fmt.Println("Error while converting proto_minor to int", err)
				}
				r.HttpReq.ProtoMinor = protominor
			}
		}
	return r
}
