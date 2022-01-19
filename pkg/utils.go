package pkg

import (
	"net/http"

	"go.keploy.io/server/pkg/service/run"
)

func CompareHeaders(h1 http.Header, h2 http.Header, res *[]run.HeaderResult) bool {
	match := true
	for k, v := range h1 {
		// Ignore go http router default headers
		if k == "Date" || k == "Content-Length" {
			continue
		}
		val, ok := h2[k]
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
		if k == "Date" || k == "Content-Length" {
			continue
		}
		_, ok := h1[k]
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
