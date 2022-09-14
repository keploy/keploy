package pkg

import (
	"html"
	"net/http"
	"regexp"
	"strings"

	"go.keploy.io/server/pkg/service/run"
)

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

func CompareHeaders(h1 http.Header, h2 http.Header, res *[]run.HeaderResult, noise map[string]string) bool {
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
				//fmt.Println("header not present", k)
				if checkKey(res, k) {
					*res = append(*res, run.HeaderResult{
						Normal: false,
						Expected: run.Header{
							Key:   k,
							Value: v,
						},
						Actual: run.Header{
							Key:   k,
							Value: nil,
						},
					})
				}

				match = false
				continue
			}
			if len(v) != len(val) {
				//fmt.Println("value not same", k, v, val)
				if checkKey(res, k) {
					*res = append(*res, run.HeaderResult{
						Normal: false,
						Expected: run.Header{
							Key:   k,
							Value: v,
						},
						Actual: run.Header{
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
					//fmt.Println("value not same", k, v, val)
					if checkKey(res, k) {
						*res = append(*res, run.HeaderResult{
							Normal: false,
							Expected: run.Header{
								Key:   k,
								Value: v,
							},
							Actual: run.Header{
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
			*res = append(*res, run.HeaderResult{
				Normal: true,
				Expected: run.Header{
					Key:   k,
					Value: v,
				},
				Actual: run.Header{
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
			*res = append(*res, run.HeaderResult{
				Normal: true,
				Expected: run.Header{
					Key:   k,
					Value: val,
				},
				Actual: run.Header{
					Key:   k,
					Value: v,
				},
			})
			continue
		}
		if !ok {
			//fmt.Println("header not present", k)
			if checkKey(res, k) {
				*res = append(*res, run.HeaderResult{
					Normal: false,
					Expected: run.Header{
						Key:   k,
						Value: nil,
					},
					Actual: run.Header{
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

func checkKey(res *[]run.HeaderResult, key string) bool {
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
