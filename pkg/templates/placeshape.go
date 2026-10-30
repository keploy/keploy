package templates

import (
	"regexp"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// placeLead is the words keploy writes before a place's field path
// (Failure.Where, Capture.Where, Refusal.Where, Undefined.Where, Place.Where):
// which side ("request", "response"), what is read ("url", "url_params",
// "header", "header name", "pseudo header", "trailer", "form", "form field
// name", "body") and, in a streamed answer, which frame and which of its
// fields ("body frame 3", "body frame 3 data"), then " key" for a place in an
// object key. Each is keploy's own, never the app's: a frame's number is
// keploy's count of the frames, so it stays whole.
var placeLead = regexp.MustCompile(`^(?:request |response )?(?:url(?:_params)?|form(?: field name)?|(?:pseudo )?header(?: name)?|trailer(?: name)?|body(?: frame (?:0|[1-9][0-9]{0,8})(?: [A-Za-z]+)?)?)(?: key)?`)

// PlaceShape is a template place as a log line names it: keploy's leading
// words kept (placeLead), each placeholder in it kept (the test case's own
// text: body key roles.{{string .id}}), and every other stretch of the field
// path in its shape (models.FieldShape). A place under an object key is
// named by the key, which may be a value the app sent: a templated key's
// live value (body key roles.u_LIVE2), a user's name or e-mail address as a
// recorded key (body.users.Ada Lovelace.id). A key that holds a space is one
// part, so it is {}: a space is in no name.
func PlaceShape(where string) string {
	lead := placeLead.FindString(where)
	rest := where[len(lead):]
	// The path follows the lead after one space ("header Set-Cookie.sid",
	// "body key roles.x"), one dot ("body.users.x") or nothing ("body[4]").
	if rest != "" && (rest[0] == ' ' || rest[0] == '.') {
		lead, rest = lead+rest[:1], rest[1:]
	}
	if rest == "" {
		return lead
	}
	var b strings.Builder
	b.WriteString(lead)
	last := 0
	for _, p := range Find(rest) {
		b.WriteString(pathStretchShape(rest[last:p.Start]))
		b.WriteString(rest[p.Start:p.End])
		last = p.End
	}
	b.WriteString(pathStretchShape(rest[last:]))
	return b.String()
}

// pathStretchShape is a stretch of a field path between placeholders in its
// shape (models.FieldShape), the dots that join it to them kept.
func pathStretchShape(s string) string {
	core := strings.Trim(s, ".")
	if core == "" {
		return s
	}
	i := strings.Index(s, core)
	return s[:i] + models.FieldShape(core) + s[i+len(core):]
}
