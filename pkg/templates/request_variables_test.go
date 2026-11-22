package templates

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The variables a request sends are the keys its placeholders name, wherever
// they stand, each once, at the run's value beside the recorded one; a secret
// is a literal, and a key config.yaml does not define is text.
func TestRun_RequestVariables(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"id": "u_rec", "n": 1234567, "tok": "t_rec", "unused": "x", "q": "q_rec", "f": "f_rec"}, map[string]interface{}{"key": "s"})
	r.Apply([]Capture{{Key: "id", Value: "u_live"}})
	got := r.RequestVariables(models.HTTPReq{
		URL:       "http://h/users/{{string .id}}?n={{int .n}}",
		URLParams: map[string]string{"n": "{{int .n}}", "only": "{{string .q}}"},
		Header:    map[string]string{"Authorization": "Bearer {{string .tok}}", "X-Key": "{{string .secret.key}}"},
		Body:      `{"id":"{{string .id}}","who":"{{string .nobody}}"}`,
		Form:      []models.FormData{{Key: "f", Values: []string{"{{string .tok}}"}}, {Key: "g", Values: []string{"{{string .f}}"}}},
	})
	want := []RequestVariable{
		{Key: "f", Value: "f_rec", Recorded: "f_rec"},
		{Key: "id", Value: "u_live", Recorded: "u_rec"},
		{Key: "n", Value: "1234567", Recorded: "1234567"},
		{Key: "q", Value: "q_rec", Recorded: "q_rec"},
		{Key: "tok", Value: "t_rec", Recorded: "t_rec"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RequestVariables = %+v, want %+v", got, want)
	}
	if got := r.RequestVariables(models.HTTPReq{URL: "http://h/plain"}); len(got) != 0 {
		t.Fatalf("a request without placeholders sends %+v", got)
	}
}
