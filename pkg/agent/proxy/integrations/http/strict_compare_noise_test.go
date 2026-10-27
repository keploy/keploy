package http

import "testing"

// keploy.yml's request-body noise names whole fields (P093): noise "id"
// excuses a body's id, never its paid amount. As a substring of the field's
// path it did, so a call whose amount changed was served the recording of
// another amount.
func TestStrictDeclaredBodyNoiseNamesWholeFields(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/v1/charge", ct, `{"id":"a-1","paid":100,"user":{"id":7}}`)
	noise := map[string][]string{"id": {}}
	if got, diag := strictMatch(t, sent("/v1/charge", ct, `{"id":"a-2","paid":100,"user":{"id":8}}`), nil, noise, nil, rec); got == nil {
		t.Fatalf("the ids the noise names were compared: %v", differs(diag))
	}
	if got, diag := strictMatch(t, sent("/v1/charge", ct, `{"id":"a-1","paid":0,"user":{"id":7}}`), nil, noise, nil, rec); got != nil || !hasField(differs(diag), "body.paid") {
		t.Fatalf("noise \"id\" excused paid: served %v, differs %v", got != nil, differs(diag))
	}
}

// A form field's name is all of it: noise "user" excuses the field user, never
// user[amount], whose bracket opens no array.
func TestStrictDeclaredBodyNoiseNamesWholeFormFields(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	rec := recorded("m", "/v1/charge", ct, `user=alice&user[amount]=100`)
	noise := map[string][]string{"user": {}}
	if got, diag := strictMatch(t, sent("/v1/charge", ct, `user=bob&user[amount]=100`), nil, noise, nil, rec); got == nil {
		t.Fatalf("the field the noise names was compared: %v", differs(diag))
	}
	got, diag := strictMatch(t, sent("/v1/charge", ct, `user=alice&user[amount]=1`), nil, noise, nil, rec)
	if got != nil || diag == nil || len(diag.differs) != 1 || diag.differs[0].diff.Path != "body.user[amount]" {
		t.Fatalf("noise \"user\" excused user[amount]: served %v, differs %v", got != nil, differs(diag))
	}
}
