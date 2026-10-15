package http

import (
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// d26Mask is the mask the learn step learns for a value that drifted only by
// F's clock (founder decision 26).
func d26Mask(t *testing.T, recorded, live string) []string {
	t.Helper()
	m := learn.ClockMask(recorded, live)
	if m == "" {
		t.Fatalf("no mask for %q -> %q", recorded, live)
	}
	return []string{m}
}

// The strict matcher honours a learned mask in every part of a request: a
// value whose clock read moved is excused, one that changed what did not
// drift is compared, and reported.
func TestStrictHonoursALearnedMask(t *testing.T) {
	note := "order 1234 placed at 2026-10-14T10:00:41Z"
	noteMask := d26Mask(t, note, "order 1234 placed at 2026-10-14T10:01:42Z")
	tsMask := d26Mask(t, "2026-10-14T10:00:41Z", "2026-10-14T10:01:42Z")
	sentMask := d26Mask(t, "sent Wed, 14 Oct 2026 10:00:41 +0000", "sent Wed, 14 Oct 2026 10:01:42 +0000")

	t.Run("a JSON body field", func(t *testing.T) {
		ct := map[string]string{"Content-Type": "application/json"}
		rec := recorded("m", "/v1/orders", ct, `{"note":"`+note+`","items":[{"at":"2026-10-14T10:00:41Z"}]}`)
		rec.Spec.ReqBodyNoise = map[string][]string{"body.note": noteMask, "body.items[0].at": tsMask}
		if got, diag := strictMatch(t, sent("/v1/orders", ct, `{"note":"order 1234 placed at 2026-11-02T08:15:00Z","items":[{"at":"2026-11-02T08:15:00.5Z"}]}`), nil, nil, nil, rec); got == nil {
			t.Fatalf("clock reads that moved were not excused: %v", differs(diag))
		}
		if got, diag := strictMatch(t, sent("/v1/orders", ct, `{"note":"order 9999 placed at 2026-11-02T08:15:00Z","items":[{"at":"2026-11-02T08:15:00Z"}]}`), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body.note") {
			t.Fatalf("a note changed outside its clock read: served %v, differs %v", got != nil, differs(diag))
		}
		if got, diag := strictMatch(t, sent("/v1/orders", ct, `{"note":"`+note+`","items":[{"at":"soon"}]}`), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body.items[].at") {
			t.Fatalf("a masked element that is no clock read: served %v, differs %v", got != nil, differs(diag))
		}
	})

	t.Run("a header", func(t *testing.T) {
		rec := recorded("m", "/v1/orders", map[string]string{"X-Sent": "sent Wed, 14 Oct 2026 10:00:41 +0000"}, "")
		rec.Spec.ReqBodyNoise = map[string][]string{"header.x-sent": sentMask}
		if got, diag := strictMatch(t, sent("/v1/orders", map[string]string{"X-Sent": "sent Mon, 02 Nov 2026 13:45:00 +0530"}, ""), nil, nil, nil, rec); got == nil {
			t.Fatalf("a header whose clock read moved was not excused: %v", differs(diag))
		}
		if got, diag := strictMatch(t, sent("/v1/orders", map[string]string{"X-Sent": "held Mon, 02 Nov 2026 08:15:00 +0000"}, ""), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "header.x-sent") {
			t.Fatalf("a header changed outside its clock read: served %v, differs %v", got != nil, differs(diag))
		}
	})

	t.Run("a query value and a path segment", func(t *testing.T) {
		rec := recorded("m", "/v1/day/2026-10-14T10:00:41Z/orders?since="+url.QueryEscape("2026-10-14T10:00:41Z"), nil, "")
		rec.Spec.ReqBodyNoise = map[string][]string{"url.path.2": tsMask, "url.query.since": tsMask}
		if got, diag := strictMatch(t, sent("/v1/day/2026-11-02T08:15:00Z/orders?since="+url.QueryEscape("2026-11-02T08:15:00Z"), nil, ""), nil, nil, nil, rec); got == nil {
			t.Fatalf("a path segment and a query value whose clock read moved were not excused: %v", differs(diag))
		}
		if got, diag := strictMatch(t, sent("/v1/day/2026-11-02T08:15:00Z/orders?since=latest", nil, ""), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "query.since") {
			t.Fatalf("a query value that is no clock read: served %v, differs %v", got != nil, differs(diag))
		}
		if got, _ := strictMatch(t, sent("/v1/day/today/orders?since="+url.QueryEscape("2026-11-02T08:15:00Z"), nil, ""), nil, nil, nil, rec); got != nil {
			t.Fatal("a path segment that is no clock read was served")
		}
	})

	t.Run("a form field and a multipart field", func(t *testing.T) {
		ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		rec := recorded("m", "/oauth/token", ct, "grant_type=client_credentials&note="+url.QueryEscape(note))
		rec.Spec.ReqBodyNoise = map[string][]string{"body.note": noteMask}
		if got, diag := strictMatch(t, sent("/oauth/token", ct, "grant_type=client_credentials&note="+url.QueryEscape("order 1234 placed at 2026-11-02T08:15:00Z")), nil, nil, nil, rec); got == nil {
			t.Fatalf("a form field whose clock read moved was not excused: %v", differs(diag))
		}
		if got, diag := strictMatch(t, sent("/oauth/token", ct, "grant_type=client_credentials&note="+url.QueryEscape("order 9 placed at 2026-11-02T08:15:00Z")), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body.note") {
			t.Fatalf("a form field changed outside its clock read: served %v, differs %v", got != nil, differs(diag))
		}

		body, mct := multipartBody(t, "rec-boundary", map[string]string{"name": note, "kind": "x"}, "hello")
		mrec := recorded("m", "/upload", map[string]string{"Content-Type": mct}, body)
		mrec.Spec.ReqBodyNoise = map[string][]string{"body.name": noteMask}
		moved, movedCT := multipartBody(t, "b1", map[string]string{"name": "order 1234 placed at 2026-11-02T08:15:00Z", "kind": "x"}, "hello")
		if got, diag := strictMatch(t, sent("/upload", map[string]string{"Content-Type": movedCT}, moved), nil, nil, nil, mrec); got == nil {
			t.Fatalf("a multipart field whose clock read moved was not excused: %v", differs(diag))
		}
		changed, changedCT := multipartBody(t, "b2", map[string]string{"name": "order 9 placed at 2026-11-02T08:15:00Z", "kind": "x"}, "hello")
		if got, diag := strictMatch(t, sent("/upload", map[string]string{"Content-Type": changedCT}, changed), nil, nil, nil, mrec); got != nil || !hasField(differs(diag), "body.name") {
			t.Fatalf("a multipart field changed outside its clock read: served %v, differs %v", got != nil, differs(diag))
		}
	})

	t.Run("learned whole, as before", func(t *testing.T) {
		ct := map[string]string{"Content-Type": "application/json"}
		rec := recorded("m", "/v1/orders", ct, `{"note":"`+note+`"}`)
		rec.Spec.ReqBodyNoise = map[string][]string{"body.note": {}}
		if got, diag := strictMatch(t, sent("/v1/orders", ct, `{"note":"anything"}`), nil, nil, nil, rec); got == nil {
			t.Fatalf("noise learned whole did not excuse a value: %v", differs(diag))
		}
	})
}
