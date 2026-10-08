package mocknoise

import "go.keploy.io/server/v3/pkg/models"

// EachRequestText visits every text of a recorded HTTP request an id can sit
// in: its URL, body, and header, query and form values.
func EachRequestText(req *models.HTTPReq, visit func(string)) {
	visit(req.URL)
	visit(req.Body)
	for _, v := range req.Header {
		visit(v)
	}
	for _, v := range req.URLParams {
		visit(v)
	}
	for _, f := range req.Form {
		for _, v := range f.Values {
			visit(v)
		}
	}
}

// EachResponseText visits every text of a recorded HTTP response an id can
// sit in: its body and header values.
func EachResponseText(resp *models.HTTPResp, visit func(string)) {
	visit(resp.Body)
	for _, v := range resp.Header {
		visit(v)
	}
}

// ReplaceResponseWords returns resp with every word lookup knows replaced
// (ReplaceWords) in its body and header values, and whether that changed
// anything. The header map resp came with is never written: a response that
// changes gets one of its own.
func ReplaceResponseWords(resp models.HTTPResp, lookup func(word string) (string, bool)) (models.HTTPResp, bool) {
	body := ReplaceWords(resp.Body, lookup)
	changed := body != resp.Body
	resp.Body = body
	if len(resp.Header) > 0 {
		header := make(map[string]string, len(resp.Header))
		for k, v := range resp.Header {
			header[k] = ReplaceWords(v, lookup)
			changed = changed || header[k] != v
		}
		resp.Header = header
	}
	return resp, changed
}
