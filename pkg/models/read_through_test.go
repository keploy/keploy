package models

import (
	"bytes"
	"encoding/gob"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

func mongoFindSectionMock(section string) *Mock {
	return mongoFindSectionWithResp(section, `{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"1"}],"id":{"$numberLong":"0"},"ns":"appdb.service_configs"},"ok":1} }`)
}

func mongoFindSectionWithResp(reqSection, respSection string) *Mock {
	return &Mock{
		Version: Version("api.keploy.io/v1beta2"),
		Name:    "mock-mongo-1",
		Kind:    Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"type": "mocks", "destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{
				{
					Header: &MongoHeader{Opcode: 2013},
					Message: &MongoOpMessage{
						Sections: []string{reqSection},
					},
				},
			},
			MongoResponses: []MongoResponse{
				{
					Header: &MongoHeader{Opcode: 2013},
					Message: &MongoOpMessage{
						Sections: []string{respSection},
					},
				},
			},
			ReqTimestampMock: time.Now(),
			ResTimestampMock: time.Now(),
		},
	}
}

func httpTestMock(method, rawURL string, headers map[string]string, status int) *Mock {
	return &Mock{
		Version: Version("api.keploy.io/v1beta2"),
		Name:    "mock-http-1",
		Kind:    HTTP,
		Spec: MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			HTTPReq: &HTTPReq{
				Method: Method(method),
				URL:    rawURL,
				Header: headers,
			},
			HTTPResp: &HTTPResp{
				StatusCode: status,
				Body:       `{"ok":true}`,
			},
		},
	}
}

func TestReadThroughKey_MongoReadOnlyAndExactShape(t *testing.T) {
	ownerRead := mongoFindSectionMock(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha","owner.id":{"$nin":[null,""]}},"sort":{"version":1},"projection":{"owner.id":1},"hint":{"serviceId":1,"branchId":1,"version":-1},"lsid":{"id":"session-A"},"$clusterTime":{"clusterTime":111},"$db":"appdb"} }`,
	)
	// Same query with different volatile driver metadata (lsid, $clusterTime, maxTimeMS, readConcern.afterClusterTime).
	ownerReadDiffMeta := mongoFindSectionMock(
		`{ SectionSingle msg: {"find":"service_configs","projection":{"owner.id":1},"filter":{"serviceId":"svc-alpha","owner.id":{"$nin":[null,""]}},"sort":{"version":1},"hint":{"serviceId":1,"branchId":1,"version":-1},"lsid":{"id":"session-B"},"$clusterTime":{"clusterTime":999},"maxTimeMS":5000,"readConcern":{"afterClusterTime":{"$timestamp":{"t":1,"i":1}}},"$db":"appdb"} }`,
	)
	// Different sort direction (version:-1 instead of 1) MUST produce a different key.
	diffSort := mongoFindSectionMock(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha","owner.id":{"$nin":[null,""]}},"sort":{"version":-1},"projection":{"owner.id":1},"hint":{"serviceId":1,"branchId":1,"version":-1},"$db":"appdb"} }`,
	)
	// Different projection MUST produce a different key.
	diffProj := mongoFindSectionMock(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha","owner.id":{"$nin":[null,""]}},"sort":{"version":1},"projection":{"owner.name":1},"hint":{"serviceId":1,"branchId":1,"version":-1},"$db":"appdb"} }`,
	)
	// Different filter value MUST produce a different key.
	diffFilter := mongoFindSectionMock(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-beta","owner.id":{"$nin":[null,""]}},"sort":{"version":1},"projection":{"owner.id":1},"hint":{"serviceId":1,"branchId":1,"version":-1},"$db":"appdb"} }`,
	)
	// Different embedded-document key order in filter MUST produce a different key.
	diffEmbedOrder := mongoFindSectionMock(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"owner":{"b":2,"a":1}},"$db":"appdb"} }`,
	)
	embedOrderA := mongoFindSectionMock(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"owner":{"a":1,"b":2}},"$db":"appdb"} }`,
	)
	// Different batchSize MUST produce a different key (changes response shape).
	diffBatchSize := mongoFindSectionMock(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha","owner.id":{"$nin":[null,""]}},"sort":{"version":1},"projection":{"owner.id":1},"hint":{"serviceId":1,"branchId":1,"version":-1},"batchSize":1,"$db":"appdb"} }`,
	)

	k1, ok := ReadThroughKey(ownerRead)
	if !ok || k1 == "" {
		t.Fatalf("expected ownerRead to be read-through eligible")
	}
	if name := ReadThroughMockName(k1); len(name) != len("mock-rt-")+16 {
		t.Fatalf("expected 16-hex-char read-through name, got %q", name)
	}
	k2, ok := ReadThroughKey(ownerReadDiffMeta)
	if !ok || k2 != k1 {
		t.Fatalf("expected identical key ignoring volatile metadata: k1=%q k2=%q ok=%v", k1, k2, ok)
	}
	for name, other := range map[string]*Mock{
		"diffSort":      diffSort,
		"diffProj":      diffProj,
		"diffFilter":    diffFilter,
		"diffBatchSize": diffBatchSize,
	} {
		ko, ok := ReadThroughKey(other)
		if !ok {
			t.Fatalf("%s unexpectedly ineligible", name)
		}
		if ko == k1 {
			t.Fatalf("%s collided with ownerRead key %q (must miss on exact match)", name, k1)
		}
	}
	ke1, ok1 := ReadThroughKey(embedOrderA)
	ke2, ok2 := ReadThroughKey(diffEmbedOrder)
	if !ok1 || !ok2 || ke1 == ke2 {
		t.Fatalf("embedded document key order must be preserved in Mongo key: ok=(%v,%v) k=(%q,%q)", ok1, ok2, ke1, ke2)
	}

	// Error response (ok:0) and open cursor (cursor.id != 0) MUST be rejected.
	if _, ok := ReadThroughKey(mongoFindSectionWithResp(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"$db":"appdb"} }`,
		`{ SectionSingle msg: {"ok":0,"errmsg":"transient error"} }`,
	)); ok {
		t.Fatal("Mongo ok:0 error response must NEVER enter read-through tier")
	}
	if _, ok := ReadThroughKey(mongoFindSectionWithResp(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"$db":"appdb"} }`,
		`{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"1"}],"id":{"$numberLong":"99999"},"ns":"appdb.service_configs"},"ok":1} }`,
	)); ok {
		t.Fatal("Mongo open multi-batch cursor (cursor.id != 0) must NEVER enter read-through tier")
	}

	// Read-only aggregate / count / distinct MUST be eligible.
	for _, tc := range []struct {
		name string
		sec  string
	}{
		{"aggregate-read", `{ SectionSingle msg: {"aggregate":"events","pipeline":[{"$match":{"type":"cfg"}},{"$sort":{"v":1}}],"cursor":{},"$db":"appdb"} }`},
		{"count", `{ SectionSingle msg: {"count":"users","query":{"active":true},"$db":"appdb"} }`},
		{"distinct", `{ SectionSingle msg: {"distinct":"users","key":"role","query":{"org":"acme"},"$db":"appdb"} }`},
	} {
		if _, ok := ReadThroughKey(mongoFindSectionMock(tc.sec)); !ok {
			t.Fatalf("expected Mongo %s to be read-through eligible", tc.name)
		}
	}

	// ALL write / mutating / cursor Mongo commands MUST be rejected (fail-closed).
	for _, tc := range []struct {
		name string
		sec  string
	}{
		{"insert", `{ SectionSingle msg: {"insert":"users","documents":[{"_id":1}],"$db":"appdb"} }`},
		{"update", `{ SectionSingle msg: {"update":"users","updates":[{"q":{"_id":1},"u":{"$set":{"a":2}}}],"$db":"appdb"} }`},
		{"delete", `{ SectionSingle msg: {"delete":"users","deletes":[{"q":{"_id":1},"limit":1}],"$db":"appdb"} }`},
		{"findAndModify", `{ SectionSingle msg: {"findAndModify":"jobs","query":{"status":"new"},"update":{"$set":{"status":"run"}},"$db":"appdb"} }`},
		{"aggregate-$out", `{ SectionSingle msg: {"aggregate":"events","pipeline":[{"$match":{"a":1}},{"$out":"archive"}],"cursor":{},"$db":"appdb"} }`},
		{"aggregate-$merge", `{ SectionSingle msg: {"aggregate":"events","pipeline":[{"$merge":{"into":"summary"}}],"cursor":{},"$db":"appdb"} }`},
		{"createIndexes", `{ SectionSingle msg: {"createIndexes":"events","indexes":[{"key":{"a":1},"name":"a_1"}],"$db":"appdb"} }`},
		{"getMore", `{ SectionSingle msg: {"getMore":12345,"collection":"events","$db":"appdb"} }`},
		{"txnNumber-find", `{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"txnNumber":{"$numberLong":"1"},"$db":"appdb"} }`},
		{"autocommit-false-find", `{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"autocommit":false,"$db":"appdb"} }`},
		{"startTransaction-find", `{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"startTransaction":true,"txnNumber":1,"autocommit":false,"$db":"appdb"} }`},
	} {
		if _, ok := ReadThroughKey(mongoFindSectionMock(tc.sec)); ok {
			t.Fatalf("Mongo write/mutating/transactional command %s MUST NOT be read-through eligible", tc.name)
		}
	}

	// Fail-closed Mongo response validation: spilled, nil message, empty
	// sections, unparseable section JSON, missing ok, value MongoOpReply with
	// open cursor or error flags, and unknown message types MUST be rejected.
	validFindReq := `{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"$db":"appdb"} }`
	spilled := mongoFindSectionMock(validFindReq)
	spilled.Spec.MongoResponses = nil
	spilled.responseHydrator = func() (*HTTPResp, []MongoResponse, error) { return nil, nil, nil }
	if !spilled.HasSpilledResponse() {
		t.Fatal("expected spilled fixture to report HasSpilledResponse() == true")
	}
	if _, ok := ReadThroughKey(spilled); ok {
		t.Fatal("spilled response mock must fail closed and NOT enter read-through tier")
	}
	for name, resp := range map[string][]MongoResponse{
		"nil-message":            {{Header: &MongoHeader{Opcode: 2013}, Message: nil}},
		"nil-opmsg-ptr":          {{Header: &MongoHeader{Opcode: 2013}, Message: (*MongoOpMessage)(nil)}},
		"empty-sections":         {{Header: &MongoHeader{Opcode: 2013}, Message: &MongoOpMessage{Sections: nil}}},
		"unparseable-section":    {{Header: &MongoHeader{Opcode: 2013}, Message: &MongoOpMessage{Sections: []string{"not-valid-section"}}}},
		"unparseable-json":       {{Header: &MongoHeader{Opcode: 2013}, Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {broken`}}}},
		"missing-ok-field":       {{Header: &MongoHeader{Opcode: 2013}, Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"cursor":{"firstBatch":[],"id":0}} }`}}}},
		"cursor-missing-id":      {{Header: &MongoHeader{Opcode: 2013}, Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"cursor":{"firstBatch":[]},"ok":1} }`}}}},
		"value-opreply-open-cur": {{Header: &MongoHeader{Opcode: 1}, Message: MongoOpReply{CursorID: 42}}},
		"ptr-opreply-nil":        {{Header: &MongoHeader{Opcode: 1}, Message: (*MongoOpReply)(nil)}},
		"value-opreply-errflag":  {{Header: &MongoHeader{Opcode: 1}, Message: MongoOpReply{ResponseFlags: 0x02}}},
		"value-opreply-errmsg":   {{Header: &MongoHeader{Opcode: 1}, Message: MongoOpReply{Documents: []string{`{"$err":"not master"}`}}}},
		"opreply-cmd-open-cur":   {{Header: &MongoHeader{Opcode: 1}, Message: MongoOpReply{CursorID: 0, Documents: []string{`{"cursor":{"firstBatch":[{"_id":"1"}],"id":{"$numberLong":"42"}},"ok":1}`}}}},
		"opreply-cmd-no-cur-id":  {{Header: &MongoHeader{Opcode: 1}, Message: MongoOpReply{CursorID: 0, Documents: []string{`{"cursor":{"firstBatch":[]},"ok":1}`}}}},
		"unknown-msg-type":       {{Header: &MongoHeader{Opcode: 2004}, Message: &MongoOpQuery{FullCollectionName: "appdb.x", Query: "{}"}}},
	} {
		m := mongoFindSectionMock(validFindReq)
		m.Spec.MongoResponses = resp
		if _, ok := ReadThroughKey(m); ok {
			t.Fatalf("Mongo fail-closed response case %s MUST be rejected", name)
		}
	}
	// Valid closed OP_REPLY value with clean document (including $cmd closed cursor) MUST succeed.
	validOpReply := mongoFindSectionMock(validFindReq)
	validOpReply.Spec.MongoResponses = []MongoResponse{{
		Header:  &MongoHeader{Opcode: 1},
		Message: MongoOpReply{CursorID: 0, ResponseFlags: 0, Documents: []string{`{"_id":"1","ok":1}`}},
	}}
	if _, ok := ReadThroughKey(validOpReply); !ok {
		t.Fatal("valid closed MongoOpReply with ok:1 document must be accepted")
	}
	validOpReplyCmdCur := mongoFindSectionMock(validFindReq)
	validOpReplyCmdCur.Spec.MongoResponses = []MongoResponse{{
		Header:  &MongoHeader{Opcode: 1},
		Message: MongoOpReply{CursorID: 0, ResponseFlags: 0, Documents: []string{`{"cursor":{"firstBatch":[{"_id":"1"}],"id":{"$numberLong":"0"}},"ok":1}`}},
	}}
	if _, ok := ReadThroughKey(validOpReplyCmdCur); !ok {
		t.Fatal("valid closed MongoOpReply $cmd with cursor.id=0 must be accepted")
	}

	// Legacy OP_QUERY must accept both pointer (*MongoOpQuery) and value
	// (MongoOpQuery) with identical canonical keys, while rejecting writes.
	opQueryVal := MongoOpQuery{
		FullCollectionName: "appdb.service_configs",
		NumberToSkip:       0,
		NumberToReturn:     1,
		Query:              `{"serviceId":"svc-alpha"}`,
	}
	opQueryPtrMock := validOpReply.DeepCopy()
	opQueryPtrMock.Spec.MongoRequests = []MongoRequest{{Header: &MongoHeader{Opcode: 2004}, Message: &opQueryVal}}
	opQueryValMock := validOpReply.DeepCopy()
	opQueryValMock.Spec.MongoRequests = []MongoRequest{{Header: &MongoHeader{Opcode: 2004}, Message: opQueryVal}}
	kpq, okpq := ReadThroughKey(opQueryPtrMock)
	kvq, okvq := ReadThroughKey(opQueryValMock)
	if !okpq || !okvq || kpq == "" || kpq != kvq {
		t.Fatalf("pointer and value MongoOpQuery must produce identical keys: ptr=(%q,%v) val=(%q,%v)", kpq, okpq, kvq, okvq)
	}
	opQueryCmdWrite := validOpReply.DeepCopy()
	opQueryCmdWrite.Spec.MongoRequests = []MongoRequest{{
		Header: &MongoHeader{Opcode: 2004},
		Message: MongoOpQuery{
			FullCollectionName: "appdb.$cmd",
			Query:              `{"insert":"users","documents":[{"_id":1}]}`,
		},
	}}
	if _, ok := ReadThroughKey(opQueryCmdWrite); ok {
		t.Fatal("value MongoOpQuery write on .$cmd must be rejected")
	}
}

func TestReadThroughKey_HTTPReadOnlyExactAndTenantIsolation(t *testing.T) {
	g1 := httpTestMock("GET", "http://auth.internal/v1/jwks?b=2&a=1", map[string]string{
		"Host": "auth.internal", "Authorization": "Bearer tok-A", "X-Request-Id": "req-1", "Accept-Encoding": "gzip",
	}, 200)
	g2 := httpTestMock("GET", "http://auth.internal/v1/jwks?a=1&b=2", map[string]string{
		"Host": "auth.internal", "Authorization": "Bearer tok-A", "X-Request-Id": "req-2", "Accept-Encoding": "gzip",
	}, 200)
	gDiffAuth := httpTestMock("GET", "http://auth.internal/v1/jwks?a=1&b=2", map[string]string{
		"Host": "auth.internal", "Authorization": "Bearer tok-B", "Accept-Encoding": "gzip",
	}, 200)
	gDiffEnc := httpTestMock("GET", "http://auth.internal/v1/jwks?a=1&b=2", map[string]string{
		"Host": "auth.internal", "Authorization": "Bearer tok-A", "Accept-Encoding": "identity",
	}, 200)
	gHTTPS := httpTestMock("GET", "https://auth.internal/v1/jwks?a=1&b=2", map[string]string{
		"Host": "auth.internal", "Authorization": "Bearer tok-A", "Accept-Encoding": "gzip",
	}, 200)
	gEscapedSlash := httpTestMock("GET", "http://auth.internal/v1/items/a%2Fb", map[string]string{"Host": "auth.internal"}, 200)
	gLiteralSlash := httpTestMock("GET", "http://auth.internal/v1/items/a/b", map[string]string{"Host": "auth.internal"}, 200)
	gRawSemi1 := httpTestMock("GET", "http://auth.internal/v1/x?id=1;v=2", map[string]string{"Host": "auth.internal"}, 200)
	gRawSemi2 := httpTestMock("GET", "http://auth.internal/v1/x?id=5;v=9", map[string]string{"Host": "auth.internal"}, 200)

	k1, ok := ReadThroughKey(g1)
	if !ok {
		t.Fatal("GET must be eligible")
	}
	// Origin-form URLs containing "://" in query params (e.g., OAuth redirect_uri)
	// must still apply meta["tls"]="true" so HTTPS and plaintext never collide.
	cbPlain := httpTestMock("GET", "/cb?redirect_uri=https://app/x", map[string]string{"Host": "auth.internal"}, 301)
	cbTLS := httpTestMock("GET", "/cb?redirect_uri=https://app/x", map[string]string{"Host": "auth.internal"}, 200)
	cbTLS.Spec.Metadata["tls"] = "true"
	cbAbsHTTPS := httpTestMock("GET", "https://auth.internal/cb?redirect_uri=https://app/x", map[string]string{"Host": "auth.internal"}, 200)
	kCBPlain, okP := ReadThroughKey(cbPlain)
	kCBTLS, okT := ReadThroughKey(cbTLS)
	kCBAbs, okA := ReadThroughKey(cbAbsHTTPS)
	if !okP || !okT || !okA {
		t.Fatalf("expected callback URLs to be eligible: plain=%v tls=%v abs=%v", okP, okT, okA)
	}
	if kCBPlain == kCBTLS {
		t.Fatalf("plaintext and TLS origin-form URLs with :// in query must NOT collide: %q", kCBTLS)
	}
	if kCBTLS != kCBAbs {
		t.Fatalf("TLS origin-form URL with :// in query must match absolute https:// key: %q != %q", kCBTLS, kCBAbs)
	}
	k2, ok := ReadThroughKey(g2)
	if !ok || k1 != k2 {
		t.Fatalf("GET with reordered query and different X-Request-Id must dedup: k1=%q k2=%q", k1, k2)
	}
	for name, other := range map[string]*Mock{
		"diffAuth": gDiffAuth,
		"diffEnc":  gDiffEnc,
		"https":    gHTTPS,
	} {
		ko, ok := ReadThroughKey(other)
		if !ok || ko == k1 {
			t.Fatalf("HTTP %s must produce a distinct key (ok=%v)", name, ok)
		}
	}
	ks1, ok1 := ReadThroughKey(gEscapedSlash)
	ks2, ok2 := ReadThroughKey(gLiteralSlash)
	if !ok1 || !ok2 || ks1 == ks2 {
		t.Fatal("escaped %2F and literal / must not collide")
	}
	kr1, ok1 := ReadThroughKey(gRawSemi1)
	kr2, ok2 := ReadThroughKey(gRawSemi2)
	if !ok1 || !ok2 || kr1 == kr2 {
		t.Fatal("unparseable semicolon query strings must not collide")
	}

	// Non-cacheable error status codes must be rejected.
	for _, code := range []int{401, 403, 429, 500, 503} {
		if _, ok := ReadThroughKey(httpTestMock("GET", "http://auth.internal/v1/jwks", nil, code)); ok {
			t.Fatalf("HTTP %d error response must NOT enter read-through tier", code)
		}
	}
	// Mutating methods must NEVER be eligible.
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		if _, ok := ReadThroughKey(httpTestMock(method, "http://auth.internal/v1/jwks", nil, 200)); ok {
			t.Fatalf("HTTP %s MUST NOT be read-through eligible", method)
		}
	}
}

func TestReadThroughKey_MySQLWriteExclusionDatabaseScopeAndUnservedKinds(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{"SELECT owner_id FROM service_configs WHERE service_id = 'svc-alpha'", true},
		{"/* comment */ SELECT id FROM tenants WHERE active = 1;", true},
		{"SELECT id FROM rows WHERE active = 1", true},
		{"SELECT id FROM tenants -- real comment\nWHERE active = 1", true},
		{"SELECT id FROM tenants # hash comment\nWHERE active = 1", true},
		{"SELECT * FROM tenants WHERE x=1--1 INTO OUTFILE '/tmp/a'", false},
		{"SELECT * FROM ROWS FROM (mutating_fn(1)) AS t", false},
		{"SELECT * FROM ROWS FROM(mutating_fn(1)) AS t", false},
		{"SELECT * FROM jobs WHERE id = 1 FOR UPDATE", false},
		{"SELECT * FROM jobs WHERE id = 1\nFOR UPDATE SKIP LOCKED", false},
		{"SELECT * FROM jobs WHERE id = 1\tFOR\tSHARE", false},
		{"SELECT * FROM jobs WHERE id = 1 FOR/**/UPDATE", false},
		{"SELECT * FROM jobs\nINTO @x", false},
		{"SELECT * FROM jobs INTO OUTFILE '/tmp/a'", false},
		{"SELECT /*!50000 1 */ FROM t", false},
		{"SELECT nextval('order_seq')", false},
		{"SELECT nextval ('order_seq') FROM dual", false},
		{"SELECT setval('order_seq', 42)", false},
		{"SELECT pg_advisory_lock(1)", false},
		{"SELECT GET_LOCK('k', 5)", false},
		{"SELECT LAST_INSERT_ID()", false},
		{"SELECT FOUND_ROWS()", false},
		{"SELECT my_mutating_proc(1)", false},
		{"SELECT * FROM my_mutating_tvf(1)", false},
		{"SELECT * FROM LATERAL my_mutating_tvf(1)", false},
		{"SELECT * FROM users JOIN LATERAL my_mutating_tvf(users.id) ON true", false},
		{"SELECT * FROM users, my_mutating_tvf(users.id)", false},
		{"SELECT * FROM users u, public.my_mutating_tvf (u.id) WHERE u.id = 1", false},
		{"SELECT * FROM (SELECT id FROM users) sub, my_mutating_tvf(sub.id)", false},
		{"SELECT u.id, o.total FROM users u, orders o WHERE u.id = o.user_id AND o.status IN (1, 2)", true},
		{"SELECT `id` FROM `tenants` WHERE `slug` = 'acme'", true},
		{"SELECT a FROM `t#x` INTO OUTFILE '/tmp/pwn'", false},
		{"SELECT a FROM `t--x` INTO OUTFILE '/tmp/pwn'", false},
		{"SELECT a FROM `t``#x` INTO OUTFILE '/tmp/pwn'", false},
		{"SELECT `id FROM `tenants`", false},
		{"SELECT * FROM users; DELETE FROM users", false},
		{"INSERT INTO users VALUES (1)", false},
		{"UPDATE users SET a = 1", false},
		{"DELETE FROM users WHERE id = 1", false},
	} {
		// Protocol-4.1 EOF terminator with SERVER_STATUS_AUTOCOMMIT (0x0002),
		// SERVER_STATUS_IN_TRANS clear.
		autocommitRS := &mysql.TextResultSet{
			FinalResponse: &mysql.GenericResponse{
				Data: []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00},
				Type: "EOF",
			},
		}
		m := &Mock{
			Kind: MySQL,
			Spec: MockSpec{
				Metadata:       map[string]string{"type": "mocks", "destAddr": "mysql:3306", "database": "appdb"},
				MySQLRequests:  []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: tc.sql}}}},
				MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: autocommitRS}}},
			},
		}
		_, got := ReadThroughKey(m)
		if got != tc.want {
			t.Fatalf("MySQL read-only classification for %q = %v, want %v", tc.sql, got, tc.want)
		}
	}

	// MySQL non-result-set responses (ERR, OK from INTO OUTFILE/DML, EOF) and
	// result sets missing a status-carrying terminator or with
	// SERVER_STATUS_IN_TRANS (0x0001) set MUST all be rejected fail-closed.
	eofAutocommit := []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00}
	eofInTrans := []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x03, 0x00}
	eofAutocommitOff := []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x00, 0x00}
	okReplacingEOFAutocommit := []byte{0x07, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
	okReplacingEOFInTrans := []byte{0x07, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00}
	for name, tc := range map[string]struct {
		resp any
		want bool
	}{
		"err-packet":                   {&mysql.ERRPacket{}, false},
		"ok-packet":                    {&mysql.OKPacket{StatusFlags: 0x0002}, false},
		"eof-packet":                   {&mysql.EOFPacket{StatusFlags: 0x0002}, false},
		"resultset-nil-terminator":     {&mysql.TextResultSet{}, false},
		"resultset-pre41-eof":          {&mysql.TextResultSet{FinalResponse: &mysql.GenericResponse{Data: []byte{0x01, 0x00, 0x00, 0x05, 0xfe}}}, false},
		"resultset-eof-in-tx":          {&mysql.TextResultSet{FinalResponse: &mysql.GenericResponse{Data: eofInTrans, Type: "EOF"}}, false},
		"resultset-eof-autocommit-off": {&mysql.TextResultSet{FinalResponse: &mysql.GenericResponse{Data: eofAutocommitOff, Type: "EOF"}}, false},
		"resultset-ok-eof-in-tx":       {&mysql.TextResultSet{FinalResponse: &mysql.GenericResponse{Data: okReplacingEOFInTrans, Type: "OK"}}, false},
		"resultset-eof-autocommit":     {&mysql.TextResultSet{FinalResponse: &mysql.GenericResponse{Data: eofAutocommit, Type: "EOF"}}, true},
		"resultset-ok-eof-autocommit":  {&mysql.TextResultSet{FinalResponse: &mysql.GenericResponse{Data: okReplacingEOFAutocommit, Type: "OK"}}, true},
	} {
		m := &Mock{
			Kind: MySQL,
			Spec: MockSpec{
				Metadata:       map[string]string{"type": "mocks", "destAddr": "mysql:3306", "database": "appdb"},
				MySQLRequests:  []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT id FROM tenants"}}}},
				MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: tc.resp}}},
			},
		}
		if _, got := ReadThroughKey(m); got != tc.want {
			t.Fatalf("MySQL response case %s = %v, want %v", name, got, tc.want)
		}
	}

	// MySQL ephemeral pod destAddr IP must NOT break cross-pod cold replay,
	// while distinct MySQL server ports stay isolated.
	mysqlRec := &Mock{
		Kind: MySQL,
		Spec: MockSpec{
			Metadata: map[string]string{
				"type":             "mocks",
				"requestOperation": "COM_QUERY",
				"destAddr":         "192.0.2.10:3306",
				"database":         "tenant_a",
			},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT id FROM tenants"}}}},
			MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: &mysql.TextResultSet{
				FinalResponse: &mysql.GenericResponse{Data: eofAutocommit, Type: "EOF"},
			}}}},
		},
	}
	mysqlLiveSamePort := &Mock{
		Kind: MySQL,
		Spec: MockSpec{
			Metadata:      map[string]string{"destPort": "3306", "database": "tenant_a"},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT id FROM tenants"}}}},
		},
	}
	mysqlLiveDiffPod := &Mock{
		Kind: MySQL,
		Spec: MockSpec{
			Metadata:      map[string]string{"destAddr": "192.0.2.20:3306", "database": "tenant_a"},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT id FROM tenants"}}}},
		},
	}
	mysqlLiveDiffPort := &Mock{
		Kind: MySQL,
		Spec: MockSpec{
			Metadata:      map[string]string{"destAddr": "192.0.2.10:3307", "database": "tenant_a"},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT id FROM tenants"}}}},
		},
	}
	kRec, okRec := ReadThroughKey(mysqlRec)
	kLive1, ok1 := ReadThroughRequestKey(mysqlLiveSamePort)
	kLive2, ok2 := ReadThroughRequestKey(mysqlLiveDiffPod)
	kDiffPort, okDP := ReadThroughRequestKey(mysqlLiveDiffPort)
	if !okRec || !ok1 || !ok2 || !okDP || kRec != kLive1 || kRec != kLive2 {
		t.Fatalf("MySQL cross-pod replay in same database+port must match: rec=(%q,%v) live1=(%q,%v) live2=(%q,%v)", kRec, okRec, kLive1, ok1, kLive2, ok2)
	}
	if kRec == kDiffPort {
		t.Fatalf("distinct MySQL server ports (3306 vs 3307) must produce distinct keys: %q", kRec)
	}

	// databaseUnknown="true" and inTx="true" MUST both fail closed.
	dbUnknown := mysqlRec.DeepCopy()
	dbUnknown.Spec.Metadata["databaseUnknown"] = "true"
	if _, ok := ReadThroughKey(dbUnknown); ok {
		t.Fatal("MySQL mock with databaseUnknown=true must fail closed")
	}
	inTxMeta := mysqlRec.DeepCopy()
	inTxMeta.Spec.Metadata["inTx"] = "true"
	if _, ok := ReadThroughKey(inTxMeta); ok {
		t.Fatal("MySQL mock with inTx=true metadata must fail closed")
	}

	// MySQL untyped nil and typed nil response messages must fail closed.
	for name, msg := range map[string]any{
		"untyped-nil":   nil,
		"typed-nil-ok":  (*mysql.OKPacket)(nil),
		"typed-nil-err": (*mysql.ERRPacket)(nil),
		"typed-nil-rs":  (*mysql.TextResultSet)(nil),
	} {
		mysqlNilResp := &Mock{
			Kind: MySQL,
			Spec: MockSpec{
				Metadata:       map[string]string{"type": "mocks"},
				MySQLRequests:  []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT id FROM tenants"}}}},
				MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: msg}}},
			},
		}
		if _, ok := ReadThroughKey(mysqlNilResp); ok {
			t.Fatalf("MySQL response %s must fail closed", name)
		}
	}

	// MySQL different database scope (tenant_a vs tenant_b vs empty) must NEVER collide.
	dbA := &Mock{
		Kind: MySQL,
		Spec: MockSpec{
			Metadata:      map[string]string{"type": "mocks", "destAddr": "mysql:3306", "database": "tenant_a"},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT * FROM settings LIMIT 1"}}}},
			MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: &mysql.TextResultSet{
				FinalResponse: &mysql.GenericResponse{Data: eofAutocommit, Type: "EOF"},
			}}}},
		},
	}
	dbB := dbA.DeepCopy()
	dbB.Spec.Metadata["database"] = "tenant_b"
	dbEmpty := dbA.DeepCopy()
	delete(dbEmpty.Spec.Metadata, "database")
	ka, oka := ReadThroughKey(dbA)
	kb, okb := ReadThroughKey(dbB)
	ke, oke := ReadThroughKey(dbEmpty)
	if !oka || !okb || !oke || ka == kb || ka == ke || kb == ke {
		t.Fatalf("different MySQL database metadata must produce distinct keys: a=%q b=%q empty=%q", ka, kb, ke)
	}

	// Unserved kinds (HTTP2, PostgresV3, Redis) MUST NOT enter the read-through tier.
	unserved := map[string]*Mock{
		"http2-get": {
			Kind: HTTP2,
			Spec: MockSpec{
				Metadata:  map[string]string{"type": "mocks"},
				HTTP2Req:  &HTTP2Req{Method: "GET", Authority: "auth.internal", URL: "/v1/jwks"},
				HTTP2Resp: &HTTP2Resp{StatusCode: 200, Body: `{"ok":true}`},
			},
		},
		"postgres-select": {
			Kind: PostgresV3,
			Spec: MockSpec{
				Metadata: map[string]string{"type": "mocks", "destAddr": "pg:5432"},
				PostgresV3: &PostgresV3Spec{
					Type: PostgresV3TypeQuery,
					Query: &PostgresV3QuerySpec{
						SQLNormalized: "SELECT id FROM users WHERE name = $1",
						Literals:      PostgresV3Cells{{Value: "alice"}},
						Response:      &PostgresV3Response{CommandComplete: "SELECT 1"},
					},
				},
			},
		},
		"redis-get": {
			Kind: REDIS,
			Spec: MockSpec{
				Metadata:         map[string]string{"type": "mocks", "destAddr": "redis:6379"},
				GenericRequests:  []Payload{{Message: []OutputBinary{{Type: "utf-8", Data: "*2\r\n$3\r\nGET\r\n$6\r\ncfg:v1\r\n"}}}},
				GenericResponses: []Payload{{Message: []OutputBinary{{Type: "utf-8", Data: "$2\r\nok\r\n"}}}},
			},
		},
	}
	for name, m := range unserved {
		if _, ok := ReadThroughKey(m); ok {
			t.Fatalf("unserved kind %s must NOT be eligible for ReadThroughKey", name)
		}
		if _, ok := ReadThroughRequestKey(m); ok {
			t.Fatalf("unserved kind %s must NOT be eligible for ReadThroughRequestKey", name)
		}
	}
}

func TestIsReadThrough_DefenseInDepthAndIngestCache(t *testing.T) {
	write := httpTestMock("POST", "http://auth.internal/v1/tokens", nil, 200)
	if MarkReadThrough(write) {
		t.Fatal("MarkReadThrough must return false for POST")
	}
	write.Spec.Metadata[MetadataReadThroughKey] = MetadataReadThroughValue
	if write.IsReadThrough() {
		t.Fatal("IsReadThrough must reject a hand-tagged POST write mock")
	}
	write.DeriveLifetime()
	if write.TestModeInfo.readThroughState != readThroughInvalid {
		t.Fatalf("DeriveLifetime should cache readThroughInvalid for hand-tagged write, got %d", write.TestModeInfo.readThroughState)
	}
	if write.IsReadThrough() {
		t.Fatal("cached IsReadThrough must reject a hand-tagged POST write mock")
	}

	read := httpTestMock("GET", "http://auth.internal/v1/jwks", nil, 200)
	if !MarkReadThrough(read) {
		t.Fatal("MarkReadThrough must accept GET")
	}
	if read.TestModeInfo.readThroughState != readThroughValid {
		t.Fatalf("MarkReadThrough should cache readThroughValid, got %d", read.TestModeInfo.readThroughState)
	}
	if !read.IsReadThrough() {
		t.Fatal("IsReadThrough must accept tagged GET")
	}
	// Simulating disk reload: zero TestModeInfo, then DeriveLifetime caches validation once.
	reloaded := read.DeepCopy()
	reloaded.TestModeInfo = TestModeInfo{}
	reloaded.DeriveLifetime()
	if reloaded.TestModeInfo.readThroughState != readThroughValid {
		t.Fatalf("DeriveLifetime should cache readThroughValid on reloaded mock, got %d", reloaded.TestModeInfo.readThroughState)
	}
	if !reloaded.IsReadThrough() {
		t.Fatal("reloaded mock IsReadThrough must return true")
	}
	// Gob stream round-trip (CLI -> agent): LifetimeDerived survives gob
	// encoding while unexported readThroughState starts at readThroughUnknown;
	// DeriveLifetime on the agent must still populate readThroughState.
	var gobBuf bytes.Buffer
	src := read.DeepCopy()
	src.TestModeInfo.LifetimeDerived = true
	if err := gob.NewEncoder(&gobBuf).Encode(src); err != nil {
		t.Fatalf("gob encode: %v", err)
	}
	var streamed Mock
	if err := gob.NewDecoder(&gobBuf).Decode(&streamed); err != nil {
		t.Fatalf("gob decode: %v", err)
	}
	if !streamed.TestModeInfo.LifetimeDerived || streamed.TestModeInfo.readThroughState != readThroughUnknown {
		t.Fatalf("precondition failed: gob decoded LifetimeDerived=%v readThroughState=%d", streamed.TestModeInfo.LifetimeDerived, streamed.TestModeInfo.readThroughState)
	}
	streamed.DeriveLifetime()
	if streamed.TestModeInfo.readThroughState != readThroughValid {
		t.Fatalf("DeriveLifetime after gob stream must populate readThroughValid even when LifetimeDerived=true, got %d", streamed.TestModeInfo.readThroughState)
	}
	if !streamed.IsReadThrough() {
		t.Fatal("gob-streamed read-through mock IsReadThrough must return true")
	}

	// Removing the metadata tag must immediately invalidate IsReadThrough.
	delete(reloaded.Spec.Metadata, MetadataReadThroughKey)
	if reloaded.IsReadThrough() {
		t.Fatal("removing readThrough metadata tag must make IsReadThrough return false")
	}
}

func TestReadThroughKey_MongoGoldenParityVectorAndPortScoping(t *testing.T) {
	// Shared golden canonical key parity suite: EVERY vector and SHA-256 digest
	// below MUST match github.com/keploy/integrations/pkg/mongo/v2
	// TestMongoReadThroughKey_CanonicalExactAndFailClosed byte-for-byte.
	goldenVectors := []struct {
		name string
		meta map[string]string
		req  MongoRequest
		want string
	}{
		{
			name: "opmsg_find_volatile_stripped",
			meta: map[string]string{"destAddr": "192.0.2.10:27017"},
			req: MongoRequest{
				Header: &MongoHeader{Opcode: 2013},
				Message: &MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"sort":{"version":1},"projection":{"owner":1},"lsid":{"id":"volatile-1"},"$clusterTime":{"clusterTime":123},"$db":"appdb"} }`,
				}},
			},
			want: "f5606b2623fffe8ce1dfcdb65ae49ff40be948f3d888035b9f2f909c2db2005d",
		},
		{
			name: "opmsg_find_destaddr_only_port",
			meta: map[string]string{"destAddr": "192.0.2.42:27018"},
			req: MongoRequest{
				Header: &MongoHeader{Opcode: 2013},
				Message: &MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"$db":"appdb"} }`,
				}},
			},
			want: "75be1a12c19d34bb5434c611be2e5924cf053a4a4e79a79335fc96ee6ee0e664",
		},
		{
			name: "opmsg_find_readconcern_level",
			meta: map[string]string{"destPort": "27017"},
			req: MongoRequest{
				Header: &MongoHeader{Opcode: 2013},
				Message: &MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"readConcern":{"level":"local","afterClusterTime":{"$timestamp":{"t":1,"i":1}}},"$db":"appdb"} }`,
				}},
			},
			want: "ca8da72d55c34c9100ea15fd38a3dd8e87ced04b7d2cbf61cebcb2596a14f1e0",
		},
		{
			name: "opmsg_count",
			meta: map[string]string{"destAddr": "10.0.0.5:27017"},
			req: MongoRequest{
				Header: &MongoHeader{Opcode: 2013},
				Message: &MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"count":"service_configs","query":{"active":true,"serviceId":"svc-alpha"},"maxTimeMS":2500,"$db":"appdb"} }`,
				}},
			},
			want: "966fa932a078afdcd3e706ecde5e826659e23c2db5c07351e2e40d2252930c40",
		},
		{
			name: "opmsg_distinct",
			meta: map[string]string{"destAddr": "10.0.0.5:27017"},
			req: MongoRequest{
				Header: &MongoHeader{Opcode: 2013},
				Message: &MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"distinct":"service_configs","key":"owner","query":{"org":"acme"},"$readPreference":{"mode":"primary"},"$db":"appdb"} }`,
				}},
			},
			want: "7a167823c5b525d918eff5a19575115d0a3754093c0c1adea4421675e4dfdd0b",
		},
		{
			name: "opmsg_aggregate_nested_order_ab",
			meta: map[string]string{"destAddr": "10.0.0.5:27017"},
			req: MongoRequest{
				Header: &MongoHeader{Opcode: 2013},
				Message: &MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"aggregate":"service_configs","pipeline":[{"$match":{"key":{"a":1,"b":2}}},{"$sort":{"version":1}},{"$project":{"owner":1}}],"cursor":{},"allowDiskUse":true,"$db":"appdb"} }`,
				}},
			},
			want: "a6f5a8edc805c3a0a8af9650c015f3a39f4895e1e0b22ac28f70985443102785",
		},
		{
			name: "opmsg_aggregate_nested_order_ba",
			meta: map[string]string{"destAddr": "10.0.0.5:27017"},
			req: MongoRequest{
				Header: &MongoHeader{Opcode: 2013},
				Message: &MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"aggregate":"service_configs","pipeline":[{"$match":{"key":{"b":2,"a":1}}},{"$sort":{"version":1}},{"$project":{"owner":1}}],"cursor":{},"allowDiskUse":true,"$db":"appdb"} }`,
				}},
			},
			want: "af1aca7419ab9ad15b6771bedf926f977fda2e7d4a92fede0bc2d91079d0b425",
		},
		{
			name: "opquery_plain_collection",
			meta: map[string]string{"destAddr": "10.0.0.5:27017"},
			req: MongoRequest{
				Header: &MongoHeader{Opcode: 2004},
				Message: &MongoOpQuery{
					FullCollectionName:   "appdb.service_configs",
					Flags:                4,
					NumberToSkip:         2,
					NumberToReturn:       10,
					Query:                `{"serviceId":"svc-alpha"}`,
					ReturnFieldsSelector: `{"owner":1}`,
				},
			},
			want: "f1b081d9afc71c5ce2ae00f8a2dd62490abd9f977447d2285983bce46e1a8bdd",
		},
		{
			name: "opquery_cmd_count",
			meta: map[string]string{"destAddr": "10.0.0.5:27017"},
			req: MongoRequest{
				Header: &MongoHeader{Opcode: 2004},
				Message: &MongoOpQuery{
					FullCollectionName: "appdb.$cmd",
					Flags:              0,
					Query:              `{"count":"service_configs","query":{"active":true},"lsid":{"id":"v1"},"maxTimeMS":500}`,
				},
			},
			want: "685c6defd130dc896078d0f0ee2d9e9f2c795b8812798fd84d92c91be249fd89",
		},
	}
	goldenByName := make(map[string]string, len(goldenVectors))
	for _, gv := range goldenVectors {
		goldenByName[gv.name] = gv.want
		t.Run(gv.name, func(t *testing.T) {
			got, ok := ReadThroughRequestKey(&Mock{
				Kind: Mongo,
				Spec: MockSpec{
					Metadata:      gv.meta,
					MongoRequests: []MongoRequest{gv.req},
				},
			})
			if !ok || got != gv.want {
				t.Fatalf("golden vector %s mismatch:\n got: (%q, %v)\nwant: (%q, true)", gv.name, got, ok, gv.want)
			}
		})
	}
	// Embedded-document equality in MongoDB depends on field order; ensure the
	// two aggregate pipeline vectors stay distinct.
	if goldenByName["opmsg_aggregate_nested_order_ab"] == goldenByName["opmsg_aggregate_nested_order_ba"] {
		t.Fatal("aggregate nested key-order vectors must produce distinct canonical keys")
	}

	wantGolden := goldenByName["opmsg_find_volatile_stripped"]
	rec := mongoFindSectionMock(
		`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"sort":{"version":1},"projection":{"owner":1},"lsid":{"id":"volatile-1"},"$clusterTime":{"clusterTime":123},"$db":"appdb"} }`,
	)
	rec.Spec.Metadata["destAddr"] = "192.0.2.10:27017"
	kRec, ok := ReadThroughKey(rec)
	if !ok || kRec != wantGolden {
		t.Fatalf("golden canonical digest mismatch:\n got: (%q, %v)\nwant: (%q, true)", kRec, ok, wantGolden)
	}

	// Ephemeral pod IP change / DNS fallback (127.0.0.1:27017) and explicit destPort
	// on the same MongoDB port MUST hash identically.
	liveSamePort := &Mock{
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destPort": "27017", "destAddr": "127.0.0.1:27017"},
			MongoRequests: []MongoRequest{{
				Header: &MongoHeader{Opcode: 2013},
				Message: &MongoOpMessage{
					Sections: []string{
						`{ SectionSingle msg: {"find":"service_configs","projection":{"owner":1},"$db":"appdb","sort":{"version":1},"filter":{"serviceId":"svc-alpha"},"lsid":{"id":"volatile-2"},"maxTimeMS":5000,"$readPreference":{"mode":"secondaryPreferred"},"$client":{"driver":{"name":"mongo-go"}},"readConcern":{"afterClusterTime":{"$timestamp":{"t":1,"i":1}}}} }`,
					},
				},
			}},
		},
	}
	if kLive, okLive := ReadThroughRequestKey(liveSamePort); !okLive || kLive != wantGolden {
		t.Fatalf("cross-pod replay key mismatch: got (%q, %v), want (%q, true)", kLive, okLive, wantGolden)
	}

	// Distinct MongoDB port MUST stay isolated.
	liveDiffPort := liveSamePort.DeepCopy()
	liveDiffPort.Spec.Metadata = map[string]string{"destPort": "27018", "destAddr": "127.0.0.1:27018"}
	if kDiff, okDiff := ReadThroughRequestKey(liveDiffPort); !okDiff || kDiff == wantGolden {
		t.Fatalf("different MongoDB port must not collide with port 27017: got (%q, %v)", kDiff, okDiff)
	}

	// Distinct MongoDB $db on the SAME port MUST stay isolated.
	liveDiffDB := &Mock{
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destPort": "27017", "destAddr": "127.0.0.1:27017"},
			MongoRequests: []MongoRequest{{
				Header: &MongoHeader{Opcode: 2013},
				Message: &MongoOpMessage{
					Sections: []string{
						`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"sort":{"version":1},"projection":{"owner":1},"$db":"tenant_b"} }`,
					},
				},
			}},
		},
	}
	if kDiffDB, okDiffDB := ReadThroughRequestKey(liveDiffDB); !okDiffDB || kDiffDB == wantGolden {
		t.Fatalf("different MongoDB $db on same port must not collide: got (%q, %v)", kDiffDB, okDiffDB)
	}

	// Application-chosen readConcern.level MUST stay distinct while afterClusterTime is stripped.
	rcLocal := mongoFindSectionMock(`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"readConcern":{"level":"local","afterClusterTime":{"$timestamp":{"t":1,"i":1}}},"$db":"appdb"} }`)
	rcLocal2 := mongoFindSectionMock(`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"readConcern":{"level":"local","afterClusterTime":{"$timestamp":{"t":9,"i":9}}},"$db":"appdb"} }`)
	rcMaj := mongoFindSectionMock(`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"svc-alpha"},"readConcern":{"level":"majority"},"$db":"appdb"} }`)
	kl1, ok1 := ReadThroughKey(rcLocal)
	kl2, ok2 := ReadThroughKey(rcLocal2)
	km, okm := ReadThroughKey(rcMaj)
	if !ok1 || !ok2 || !okm || kl1 != kl2 || kl1 == km {
		t.Fatalf("readConcern.afterClusterTime must be ignored while readConcern.level stays distinct: l1=%q l2=%q maj=%q", kl1, kl2, km)
	}

	// Streaming/tailable options, $changeStream, duplicate keys, and $cmd.* pseudo-collections MUST fail closed.
	for name, m := range map[string]*Mock{
		"tailable":         mongoFindSectionMock(`{ SectionSingle msg: {"find":"cfg","tailable":true,"$db":"appdb"} }`),
		"awaitData":        mongoFindSectionMock(`{ SectionSingle msg: {"find":"cfg","awaitData":true,"$db":"appdb"} }`),
		"exhaust":          mongoFindSectionMock(`{ SectionSingle msg: {"find":"cfg","exhaust":true,"$db":"appdb"} }`),
		"changeStream":     mongoFindSectionMock(`{ SectionSingle msg: {"aggregate":"cfg","pipeline":[{"$changeStream":{}}],"cursor":{},"$db":"appdb"} }`),
		"duplicate-keys":   mongoFindSectionMock(`{ SectionSingle msg: {"find":"cfg","filter":{"id":"1"},"filter":{"id":"2"},"$db":"appdb"} }`),
		"opquery-cmd-sys":  {Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{Header: &MongoHeader{Opcode: 2004}, Message: &MongoOpQuery{FullCollectionName: "admin.$cmd.sys.unlock", Query: `{"op":1}`}}}}},
		"opquery-tailable": {Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{Header: &MongoHeader{Opcode: 2004}, Message: &MongoOpQuery{FullCollectionName: "appdb.cfg", Flags: 0x02, Query: `{}`}}}}},
	} {
		if _, ok := ReadThroughRequestKey(m); ok {
			t.Fatalf("Mongo negative case %s MUST fail closed", name)
		}
	}

	t.Run("conflict_base_name", func(t *testing.T) {
		roundTripBase := ReadThroughMockName(wantGolden)
		roundTripConflict := ReadThroughConflictMockName(wantGolden)
		gotBase, ok := ReadThroughConflictBaseName(roundTripConflict)
		if !ok || gotBase != roundTripBase {
			t.Fatalf("ReadThroughConflictBaseName(%q) = (%q, %v), want (%q, true)", roundTripConflict, gotBase, ok, roundTripBase)
		}
		for _, tc := range []struct {
			in       string
			wantBase string
			wantOK   bool
		}{
			{"mock-rt-0123456789abcdef-conflict", "mock-rt-0123456789abcdef", true},
			{"mock-rt--conflict", "", false},
			{"mock-rt-abc", "", false},
			{"mock-x-conflict", "", false},
			{"mock-rt-0123456789abcdef-conflict-conflict", "", false},
		} {
			b, ok := ReadThroughConflictBaseName(tc.in)
			if ok != tc.wantOK || b != tc.wantBase {
				t.Fatalf("ReadThroughConflictBaseName(%q) = (%q, %v), want (%q, %v)", tc.in, b, ok, tc.wantBase, tc.wantOK)
			}
		}
	})
}

func TestReadThroughKey_SQLVerticalTabAndBacktickTVFParity(t *testing.T) {
	eofAutocommit := []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00}
	eofNoBackslash := []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x02} // 0x0202: AUTOCOMMIT | NO_BACKSLASH_ESCAPES

	// Positive controls: standard autocommit SELECT and ORM-style backtick-quoted
	// identifiers (including comma joins of quoted tables) must pass.
	for _, okSQL := range []string{
		"SELECT id FROM configs WHERE k = 'ok'",
		"SELECT * FROM `users` WHERE `users`.`id` = 1",
		"SELECT * FROM `users`, `roles` WHERE `users`.`role_id` = `roles`.`id`",
	} {
		ctrl := &Mock{
			Kind: MySQL,
			Spec: MockSpec{
				Metadata:      map[string]string{"type": "mocks", "destAddr": "mysql:3306", "database": "appdb"},
				MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: okSQL}}}},
				MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: &mysql.TextResultSet{
					FinalResponse: &mysql.GenericResponse{Data: eofAutocommit, Type: "EOF"},
				}}}},
			},
		}
		if k, ok := ReadThroughKey(ctrl); !ok || k == "" {
			t.Fatalf("positive control autocommit SELECT %q must pass ReadThroughKey", okSQL)
		}
	}
	ctrl := &Mock{
		Kind: MySQL,
		Spec: MockSpec{
			Metadata:      map[string]string{"type": "mocks", "destAddr": "mysql:3306", "database": "appdb"},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT id FROM configs WHERE k = 'ok'"}}}},
			MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: &mysql.TextResultSet{
				FinalResponse: &mysql.GenericResponse{Data: eofAutocommit, Type: "EOF"},
			}}}},
		},
	}
	// SERVER_STATUS_NO_BACKSLASH_ESCAPES (0x0200) MUST fail closed.
	noBS := ctrl.DeepCopy()
	noBS.Spec.MySQLResponses[0].Message = &mysql.TextResultSet{FinalResponse: &mysql.GenericResponse{Data: eofNoBackslash, Type: "EOF"}}
	if _, ok := ReadThroughKey(noBS); ok {
		t.Fatal("MySQL result set with SERVER_STATUS_NO_BACKSLASH_ESCAPES must fail closed")
	}

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"vtab-get-lock", "SELECT GET_LOCK\v('k',5) FROM dual"},
		{"vtab-for-update", "SELECT 1 FROM t FOR\vUPDATE"},
		{"backtick-where-comma-tvf", "SELECT * FROM `t WHERE `, JSON_TABLE('[]', '$[*]' COLUMNS(id INT PATH '$')) AS jt"},
		{"backtick-quoted-comma-tvf", "SELECT * FROM `t, JSON_TABLE('[]', '$[*]' COLUMNS(id INT PATH '$')) AS jt`"},
	} {
		m := &Mock{
			Kind: MySQL,
			Spec: MockSpec{
				Metadata:      map[string]string{"type": "mocks", "destAddr": "mysql:3306", "database": "appdb"},
				MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: tc.sql}}}},
				MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: &mysql.TextResultSet{
					FinalResponse: &mysql.GenericResponse{Data: eofAutocommit, Type: "EOF"},
				}}}},
			},
		}
		if _, ok := ReadThroughKey(m); ok {
			t.Fatalf("SQL bypass case %s (%q) MUST fail closed", tc.name, tc.sql)
		}
	}
}

func TestReadThroughResponseDigest_IgnoresVolatileMetadata(t *testing.T) {
	req := `{ SectionSingle msg: {"find":"cfg","filter":{"id":"1"},"$db":"appdb"} }`
	m1 := mongoFindSectionWithResp(req, `{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"1","v":"a"}],"id":{"$numberLong":"0"},"ns":"appdb.cfg"},"ok":1,"$clusterTime":{"clusterTime":111},"operationTime":111} }`)
	m1.Spec.MongoResponses[0].Header = &MongoHeader{Length: 100, RequestID: 10, ResponseTo: 1, Opcode: 2013}
	m1.Spec.MongoResponses[0].ReadDelay = 5
	m2 := mongoFindSectionWithResp(req, `{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"1","v":"a"}],"id":{"$numberLong":"0"},"ns":"appdb.cfg"},"ok":1,"$clusterTime":{"clusterTime":999},"operationTime":999} }`)
	m2.Spec.MongoResponses[0].Header = &MongoHeader{Length: 104, RequestID: 77, ResponseTo: 42, Opcode: 2013}
	m2.Spec.MongoResponses[0].ReadDelay = 250
	mDiff := mongoFindSectionWithResp(req, `{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"1","v":"b"}],"id":{"$numberLong":"0"},"ns":"appdb.cfg"},"ok":1} }`)
	if ReadThroughResponseDigest(m1) != ReadThroughResponseDigest(m2) {
		t.Fatal("Mongo responses differing only in Header/ReadDelay/$clusterTime/operationTime must share digest")
	}
	if ReadThroughResponseDigest(m1) == ReadThroughResponseDigest(mDiff) {
		t.Fatal("Mongo responses with different payload data must produce distinct digests")
	}

	h1 := httpTestMock("GET", "http://auth.internal/v1/jwks", nil, 200)
	h1.Spec.HTTPResp.Header = map[string]string{"Content-Type": "application/json", "Date": "Thu, 01 Jan 2026 00:00:00 GMT", "X-Request-Id": "r1"}
	h2 := httpTestMock("GET", "http://auth.internal/v1/jwks", nil, 200)
	h2.Spec.HTTPResp.Header = map[string]string{"Content-Type": "application/json", "Date": "Fri, 02 Jan 2026 00:00:00 GMT", "X-Request-Id": "r2"}
	hDiff := httpTestMock("GET", "http://auth.internal/v1/jwks", nil, 200)
	hDiff.Spec.HTTPResp.Body = `{"ok":false}`
	if ReadThroughResponseDigest(h1) != ReadThroughResponseDigest(h2) {
		t.Fatal("HTTP responses differing only in volatile Date/X-Request-Id headers must share digest")
	}
	if ReadThroughResponseDigest(h1) == ReadThroughResponseDigest(hDiff) {
		t.Fatal("HTTP responses with different bodies must produce distinct digests")
	}
}
