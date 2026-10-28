package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A replay hands the app a stand-in for its AWS secret key and session token
// (replay-isolation: the capture and the cloud replay's synthesis scramble
// them), so a SigV4 presigned URL the app returns is signed with another key
// than the recording's: everything in it but X-Amz-Signature, and the
// X-Amz-Security-Token that is itself scrambled, is the same. The response is
// compared by the presigned URL's grammar, never the signature or the token;
// every other part of it is still compared as written.

// The AWS documentation's example key id, and two secrets and session tokens
// of the same shape: the recording's and the replay's stand-in.
const (
	psKeyID    = "AKIAIOSFODNN7EXAMPLE"
	psSecret   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	psStandIn  = "Qk3rT8vWm1ZpL0sXyN5cHd/a2EfGhJ7uYbOiR9tK"
	psToken    = "IQoJb3JpZ2luX2VjEXAMPLEtokenONE+/AbCdEfGh0123456789=="
	psTokenAlt = "IQoJb3JpZ2luX2VjSTANDINtokenTWO+/ZyXwVuTs9876543210=="
)

var psAt = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

type presignArgs struct {
	secret, token, keyID, host, path, region, signedHeaders, extra string
	expires                                                        int
	at                                                             time.Time
}

func psDefaults(secret, token string) presignArgs {
	return presignArgs{
		secret: secret, token: token, keyID: psKeyID,
		host: "uploads.s3.us-east-1.amazonaws.com", path: "/reports/2026/q3.pdf",
		region: "us-east-1", signedHeaders: "host", expires: 900, at: psAt,
	}
}

func psSigningKey(secret string, at time.Time, region string) []byte {
	mac := func(k []byte, s string) []byte {
		h := hmac.New(sha256.New, k)
		h.Write([]byte(s))
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+secret), at.UTC().Format("20060102"))
	k = mac(k, region)
	k = mac(k, "s3")
	return mac(k, "aws4_request")
}

// presign builds a GET presigned URL as the AWS SDKs do (SigV4 query form,
// UNSIGNED-PAYLOAD), with a real signature over it.
func presign(a presignArgs) string {
	date := a.at.UTC().Format("20060102")
	amzDate := a.at.UTC().Format("20060102T150405Z")
	scope := date + "/" + a.region + "/s3/aws4_request"
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", a.keyID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(a.expires))
	if a.token != "" {
		q.Set("X-Amz-Security-Token", a.token)
	}
	q.Set("X-Amz-SignedHeaders", a.signedHeaders)
	if a.extra != "" {
		k, v, _ := strings.Cut(a.extra, "=")
		q.Set(k, v)
	}
	canonicalQuery := strings.ReplaceAll(q.Encode(), "+", "%20")
	canonical := strings.Join([]string{"GET", a.path, canonicalQuery, "host:" + a.host + "\n", a.signedHeaders, "UNSIGNED-PAYLOAD"}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	sts := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(sum[:])}, "\n")
	h := hmac.New(sha256.New, psSigningKey(a.secret, a.at, a.region))
	h.Write([]byte(sts))
	return "https://" + a.host + a.path + "?" + canonicalQuery + "&X-Amz-Signature=" + hex.EncodeToString(h.Sum(nil))
}

// presignPost builds a presigned POST's url and fields as the AWS SDKs do: a
// base64 policy whose conditions name every field, the session token
// included, and a signature over the policy. lower spells the field names as
// boto3 does; otherwise as the JS and Go SDKs do.
func presignPost(t *testing.T, a presignArgs, key string, lower bool) map[string]interface{} {
	t.Helper()
	cred := a.keyID + "/" + a.at.UTC().Format("20060102") + "/" + a.region + "/s3/aws4_request"
	amzDate := a.at.UTC().Format("20060102T150405Z")
	conditions := []interface{}{
		map[string]string{"bucket": "uploads"},
		[]interface{}{"starts-with", "$key", "user-7/"},
		[]interface{}{"content-length-range", 0, 1048576},
		map[string]string{"x-amz-algorithm": "AWS4-HMAC-SHA256"},
		map[string]string{"x-amz-credential": cred},
		map[string]string{"x-amz-date": amzDate},
	}
	if a.token != "" {
		conditions = append(conditions, map[string]string{"x-amz-security-token": a.token})
	}
	pj, err := json.Marshal(map[string]interface{}{
		"expiration": a.at.Add(time.Duration(a.expires) * time.Second).Format("2006-01-02T15:04:05.000Z"),
		"conditions": conditions,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := base64.StdEncoding.EncodeToString(pj)
	h := hmac.New(sha256.New, psSigningKey(a.secret, a.at, a.region))
	h.Write([]byte(policy))
	name := func(s string) string {
		if lower {
			return strings.ToLower(s)
		}
		return s
	}
	fields := map[string]interface{}{
		"key":                    key,
		name("X-Amz-Algorithm"):  "AWS4-HMAC-SHA256",
		name("X-Amz-Credential"): cred,
		name("X-Amz-Date"):       amzDate,
		name("Policy"):           policy,
		name("X-Amz-Signature"):  hex.EncodeToString(h.Sum(nil)),
	}
	if a.token != "" {
		fields[name("X-Amz-Security-Token")] = a.token
	}
	return map[string]interface{}{"url": "https://" + a.host + "/", "fields": fields}
}

// jsonBody marshals v as an app written in Go answers it: encoding/json
// escapes each & of a URL as &.
func jsonBody(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func psCase(recBody, liveBody string, recHeader, liveHeader map[string]string, status int) (*models.TestCase, *models.HTTPResp) {
	if recHeader == nil {
		recHeader = map[string]string{"Content-Type": "application/json"}
	}
	if liveHeader == nil {
		liveHeader = map[string]string{"Content-Type": "application/json"}
	}
	tc := &models.TestCase{
		Name:     "get-download-url",
		HTTPResp: models.HTTPResp{StatusCode: status, Header: recHeader, Body: recBody},
	}
	return tc, &models.HTTPResp{StatusCode: status, Header: liveHeader, Body: liveBody}
}

// eachSwitch runs a row with the proven switch off and on: the grammar is the
// same under both.
func eachSwitch(t *testing.T, row func(t *testing.T)) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			row(t)
		})
	}
}

func psMatch(tc *models.TestCase, live *models.HTTPResp) bool {
	ok, _ := Match(tc, live, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true))
	return ok
}

// THE DEFECT (replay-isolation review B2): the same presigned URL, signed with
// the replay's stand-in for the secret key and session token, failed the test
// case on its signature.
func TestMatch_PresignedURLSignedWithTheStandInSecretMatches(t *testing.T) {
	rec := psDefaults(psSecret, psToken)
	live := psDefaults(psStandIn, psTokenAlt)
	recURL, liveURL := presign(rec), presign(live)
	if recURL == liveURL {
		t.Fatal("the row's two URLs are the same: the stand-in secret signed nothing new")
	}
	noToken := func(a presignArgs) presignArgs { a.token = ""; return a }

	rows := []struct {
		name string
		tc   func(t *testing.T) (*models.TestCase, *models.HTTPResp)
	}{
		{"a JSON body's url, as a Go app writes it", func(t *testing.T) (*models.TestCase, *models.HTTPResp) {
			return psCase(jsonBody(t, map[string]string{"url": recURL}), jsonBody(t, map[string]string{"url": liveURL}), nil, nil, 200)
		}},
		{"a JSON body's url with no session token", func(t *testing.T) (*models.TestCase, *models.HTTPResp) {
			return psCase(jsonBody(t, map[string]string{"url": presign(noToken(rec))}), jsonBody(t, map[string]string{"url": presign(noToken(live))}), nil, nil, 200)
		}},
		{"an array of urls", func(t *testing.T) (*models.TestCase, *models.HTTPResp) {
			r2, l2 := rec, live
			r2.path, l2.path = "/reports/2026/q4.pdf", "/reports/2026/q4.pdf"
			return psCase(jsonBody(t, []string{recURL, presign(r2)}), jsonBody(t, []string{liveURL, presign(l2)}), nil, nil, 200)
		}},
		{"a redirect's Location header", func(t *testing.T) (*models.TestCase, *models.HTTPResp) {
			return psCase("", "", map[string]string{"Location": recURL}, map[string]string{"Location": liveURL}, 302)
		}},
		{"a plain-text body that is the url and a newline", func(t *testing.T) (*models.TestCase, *models.HTTPResp) {
			return psCase(recURL+"\n", liveURL+"\n", map[string]string{"Content-Type": "text/plain"}, map[string]string{"Content-Type": "text/plain"}, 200)
		}},
		{"a presigned POST, as the JS and Go SDKs spell it", func(t *testing.T) (*models.TestCase, *models.HTTPResp) {
			return psCase(jsonBody(t, presignPost(t, rec, "user-7/avatar.png", false)), jsonBody(t, presignPost(t, live, "user-7/avatar.png", false)), nil, nil, 200)
		}},
		{"a presigned POST, as boto3 spells it", func(t *testing.T) (*models.TestCase, *models.HTTPResp) {
			return psCase(jsonBody(t, presignPost(t, rec, "user-7/avatar.png", true)), jsonBody(t, presignPost(t, live, "user-7/avatar.png", true)), nil, nil, 200)
		}},
		{"a presigned POST whose policy names the token in an eq condition", func(t *testing.T) (*models.TestCase, *models.HTTPResp) {
			eqForm := func(a presignArgs) map[string]interface{} {
				form := presignPost(t, a, "user-7/avatar.png", false)
				fields := form["fields"].(map[string]interface{})
				pj, _ := base64.StdEncoding.DecodeString(fields["Policy"].(string))
				b, _ := json.Marshal(a.token)
				eq := strings.Replace(string(pj), `{"x-amz-security-token":`+string(b)+`}`, `["eq","$x-amz-security-token",`+string(b)+`]`, 1)
				if eq == string(pj) {
					t.Fatal("the policy names no session token condition to rewrite")
				}
				fields["Policy"] = base64.StdEncoding.EncodeToString([]byte(eq))
				return form
			}
			return psCase(jsonBody(t, eqForm(rec)), jsonBody(t, eqForm(live)), nil, nil, 200)
		}},
		{"a presigned POST with no session token", func(t *testing.T) (*models.TestCase, *models.HTTPResp) {
			return psCase(jsonBody(t, presignPost(t, noToken(rec), "user-7/avatar.png", false)), jsonBody(t, presignPost(t, noToken(live), "user-7/avatar.png", false)), nil, nil, 200)
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				tc, actual := r.tc(t)
				if tc.HTTPResp.Body == actual.Body && tc.HTTPResp.Header["Location"] == actual.Header["Location"] {
					t.Fatal("the row compares nothing that differs")
				}
				if !psMatch(tc, actual) {
					t.Fatalf("a presigned URL that differs only in its signature and session token failed\nrecorded: %s %v\nreplayed: %s %v",
						tc.HTTPResp.Body, tc.HTTPResp.Header, actual.Body, actual.Header)
				}
			})
		})
	}
}

// No false green: everything in a presigned URL but its signature and session
// token is still compared, as written.
func TestMatch_PresignedURLStillComparesEverythingButTheSignature(t *testing.T) {
	rec := psDefaults(psSecret, psToken)
	recURL := presign(rec)
	standIn := func(mod func(a *presignArgs)) string {
		a := psDefaults(psStandIn, psTokenAlt)
		mod(&a)
		return presign(a)
	}
	rows := map[string]string{
		"another key id":                         standIn(func(a *presignArgs) { a.keyID = "AKIAI44QH8DHBEXAMPLE" }),
		"another path":                           standIn(func(a *presignArgs) { a.path = "/reports/2026/q4.pdf" }),
		"another host (bucket)":                  standIn(func(a *presignArgs) { a.host = "backups.s3.us-east-1.amazonaws.com" }),
		"another region":                         standIn(func(a *presignArgs) { a.region = "eu-west-1" }),
		"another expiry":                         standIn(func(a *presignArgs) { a.expires = 3600 }),
		"another signing time":                   standIn(func(a *presignArgs) { a.at = psAt.Add(time.Second) }),
		"other signed headers":                   standIn(func(a *presignArgs) { a.signedHeaders = "host;x-amz-meta-owner" }),
		"another parameter":                      standIn(func(a *presignArgs) { a.extra = "response-content-disposition=attachment" }),
		"no session token":                       standIn(func(a *presignArgs) { a.token = "" }),
		"http, not https":                        strings.Replace(standIn(func(*presignArgs) {}), "https://", "http://", 1),
		"a signature that is no SigV4 signature": recURL[:strings.LastIndex(recURL, "=")+1] + "not-a-signature",
		"a fragment added":                       standIn(func(*presignArgs) {}) + "#page=2",
	}
	for name, liveURL := range rows {
		t.Run(name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				tc, actual := psCase(jsonBody(t, map[string]string{"url": recURL}), jsonBody(t, map[string]string{"url": liveURL}), nil, nil, 200)
				if psMatch(tc, actual) {
					t.Fatalf("a presigned URL with %s matched the recording's", name)
				}
				tc, actual = psCase("", "", map[string]string{"Location": recURL}, map[string]string{"Location": liveURL}, 302)
				if psMatch(tc, actual) {
					t.Fatalf("a Location header's presigned URL with %s matched the recording's", name)
				}
			})
		})
	}
}

// Another algorithm (SigV4A) is no SigV4 presigned URL: compared whole.
func TestMatch_ASigV4APresignedURLIsComparedWhole(t *testing.T) {
	sigV4A := func(u string) string {
		return strings.Replace(u, "X-Amz-Algorithm=AWS4-HMAC-SHA256", "X-Amz-Algorithm=AWS4-ECDSA-P256-SHA256", 1)
	}
	eachSwitch(t, func(t *testing.T) {
		tc, actual := psCase(jsonBody(t, map[string]string{"url": sigV4A(presign(psDefaults(psSecret, psToken)))}),
			jsonBody(t, map[string]string{"url": sigV4A(presign(psDefaults(psStandIn, psTokenAlt)))}), nil, nil, 200)
		if psMatch(tc, actual) {
			t.Fatal("a SigV4A URL with another signature and token matched")
		}
	})
}

// No false green for a presigned POST: its fields and its policy's conditions
// are compared, but for the signature and the session token.
func TestMatch_PresignedPOSTStillComparesItsPolicyAndFields(t *testing.T) {
	rec := presignPost(t, psDefaults(psSecret, psToken), "user-7/avatar.png", false)
	standIn := func(key string, mod func(a *presignArgs)) map[string]interface{} {
		a := psDefaults(psStandIn, psTokenAlt)
		mod(&a)
		return presignPost(t, a, key, false)
	}
	same := func(*presignArgs) {}
	rows := map[string]map[string]interface{}{
		"another key field":    standIn("user-8/avatar.png", same),
		"another key id":       standIn("user-7/avatar.png", func(a *presignArgs) { a.keyID = "AKIAI44QH8DHBEXAMPLE" }),
		"another expiration":   standIn("user-7/avatar.png", func(a *presignArgs) { a.expires = 3600 }),
		"another signing time": standIn("user-7/avatar.png", func(a *presignArgs) { a.at = psAt.Add(time.Second) }),
		"another host":         standIn("user-7/avatar.png", func(a *presignArgs) { a.host = "backups.s3.us-east-1.amazonaws.com" }),
		"no session token":     standIn("user-7/avatar.png", func(a *presignArgs) { a.token = "" }),
	}
	// A policy whose conditions differ, with every field as the recording's:
	// another upload size limit.
	other := standIn("user-7/avatar.png", same)
	fields := other["fields"].(map[string]interface{})
	pj, _ := base64.StdEncoding.DecodeString(fields["Policy"].(string))
	fields["Policy"] = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(pj), "1048576", "2097152", 1)))
	rows["a policy with another size limit"] = other
	// One whose policy names another bucket, every field as the recording's.
	other = standIn("user-7/avatar.png", same)
	fields = other["fields"].(map[string]interface{})
	pj, _ = base64.StdEncoding.DecodeString(fields["Policy"].(string))
	fields["Policy"] = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(pj), `{"bucket":"uploads"}`, `{"bucket":"backups"}`, 1)))
	rows["a policy naming another bucket"] = other
	// One whose signature field is no SigV4 signature.
	other = standIn("user-7/avatar.png", same)
	other["fields"].(map[string]interface{})["X-Amz-Signature"] = "not-a-signature"
	rows["a signature field that is no SigV4 signature"] = other

	for name, live := range rows {
		t.Run(name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				tc, actual := psCase(jsonBody(t, rec), jsonBody(t, live), nil, nil, 200)
				if psMatch(tc, actual) {
					t.Fatalf("a presigned POST with %s matched the recording's", name)
				}
			})
		})
	}

	// An object that is no presigned POST's form, here one with no
	// X-Amz-Algorithm, is compared whole: its signature and token differ.
	t.Run("a form with no algorithm", func(t *testing.T) {
		eachSwitch(t, func(t *testing.T) {
			r := presignPost(t, psDefaults(psSecret, psToken), "user-7/avatar.png", false)
			l := presignPost(t, psDefaults(psStandIn, psTokenAlt), "user-7/avatar.png", false)
			delete(r["fields"].(map[string]interface{}), "X-Amz-Algorithm")
			delete(l["fields"].(map[string]interface{}), "X-Amz-Algorithm")
			tc, actual := psCase(jsonBody(t, r), jsonBody(t, l), nil, nil, 200)
			if psMatch(tc, actual) {
				t.Fatal("an object that is no presigned POST's form matched with another signature and token")
			}
		})
	})
}
