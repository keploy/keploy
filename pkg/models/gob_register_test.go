package models

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"reflect"
	"sort"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/models/postgres"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// goldenGobNames freezes the gob wire name of every concrete type that can be
// stored in a mock's interface{} fields. gob writes these names into every
// mocks.gob file and into the interface payloads streamed between the keploy
// binaries, so a name is an on-disk / on-wire format contract: change one and
// previously recorded mocks — and any peer process built from a different
// commit — fail to decode. The init() functions pin them with gob.RegisterName,
// so a type rename no longer moves the wire name on its own; this test is the
// backstop that a name is never changed by accident. If you deliberately change
// the format, bump gobMockMagic in pkg/platform/yaml/mockdb and update the
// literal here in the same change.
//
// The name strings are frozen literals, intentionally NOT derived from the Go
// identifiers, so relabelling a registration without updating this list fails.
var goldenGobNames = []struct {
	name  string
	value any
}{
	{"*mysql.Spec", &mysql.Spec{}},
	{"*mysql.RequestYaml", &mysql.RequestYaml{}},
	{"*mysql.ResponseYaml", &mysql.ResponseYaml{}},
	{"*mysql.PacketInfo", &mysql.PacketInfo{}},
	{"*mysql.Request", &mysql.Request{}},
	{"*mysql.Response", &mysql.Response{}},
	{"*mysql.PacketBundle", &mysql.PacketBundle{}},
	{"*mysql.Packet", &mysql.Packet{}},
	{"*mysql.Header", &mysql.Header{}},
	{"*mysql.QueryPacket", &mysql.QueryPacket{}},
	{"*mysql.LocalInFileRequestPacket", &mysql.LocalInFileRequestPacket{}},
	{"*mysql.TextResultSet", &mysql.TextResultSet{}},
	{"*mysql.BinaryProtocolResultSet", &mysql.BinaryProtocolResultSet{}},
	{"*mysql.GenericResponse", &mysql.GenericResponse{}},
	{"*mysql.ColumnCount", &mysql.ColumnCount{}},
	{"*mysql.ColumnDefinition41", &mysql.ColumnDefinition41{}},
	{"*mysql.TextRow", &mysql.TextRow{}},
	{"*mysql.BinaryRow", &mysql.BinaryRow{}},
	{"*mysql.ColumnEntry", &mysql.ColumnEntry{}},
	{"*mysql.StmtPreparePacket", &mysql.StmtPreparePacket{}},
	{"*mysql.StmtPrepareOkPacket", &mysql.StmtPrepareOkPacket{}},
	{"*mysql.StmtExecutePacket", &mysql.StmtExecutePacket{}},
	{"*mysql.Parameter", &mysql.Parameter{}},
	{"*mysql.StmtFetchPacket", &mysql.StmtFetchPacket{}},
	{"*mysql.StmtClosePacket", &mysql.StmtClosePacket{}},
	{"*mysql.StmtResetPacket", &mysql.StmtResetPacket{}},
	{"*mysql.StmtSendLongDataPacket", &mysql.StmtSendLongDataPacket{}},
	{"*mysql.QuitPacket", &mysql.QuitPacket{}},
	{"*mysql.InitDBPacket", &mysql.InitDBPacket{}},
	{"*mysql.StatisticsPacket", &mysql.StatisticsPacket{}},
	{"*mysql.DebugPacket", &mysql.DebugPacket{}},
	{"*mysql.PingPacket", &mysql.PingPacket{}},
	{"*mysql.ResetConnectionPacket", &mysql.ResetConnectionPacket{}},
	{"*mysql.SetOptionPacket", &mysql.SetOptionPacket{}},
	{"*mysql.ChangeUserPacket", &mysql.ChangeUserPacket{}},
	{"*mysql.OKPacket", &mysql.OKPacket{}},
	{"*mysql.ERRPacket", &mysql.ERRPacket{}},
	{"*mysql.EOFPacket", &mysql.EOFPacket{}},
	{"*mysql.HandshakeV10Packet", &mysql.HandshakeV10Packet{}},
	{"*mysql.HandshakeResponse41Packet", &mysql.HandshakeResponse41Packet{}},
	{"*mysql.SSLRequestPacket", &mysql.SSLRequestPacket{}},
	{"*mysql.AuthSwitchRequestPacket", &mysql.AuthSwitchRequestPacket{}},
	{"*mysql.AuthSwitchResponsePacket", &mysql.AuthSwitchResponsePacket{}},
	{"*mysql.AuthMoreDataPacket", &mysql.AuthMoreDataPacket{}},
	{"*mysql.AuthNextFactorPacket", &mysql.AuthNextFactorPacket{}},
	{"*postgres.Spec", &postgres.Spec{}},
	{"*postgres.RequestYaml", &postgres.RequestYaml{}},
	{"*postgres.ResponseYaml", &postgres.ResponseYaml{}},
	{"*postgres.PacketInfo", &postgres.PacketInfo{}},
	{"*postgres.Request", &postgres.Request{}},
	{"*postgres.Response", &postgres.Response{}},
	{"*postgres.PacketBundle", &postgres.PacketBundle{}},
	{"*postgres.Packet", &postgres.Packet{}},
	{"*postgres.Header", &postgres.Header{}},
	{"*models.MongoOpMessage", &MongoOpMessage{}},
	{"*models.MongoOpQuery", &MongoOpQuery{}},
	{"*models.MongoOpReply", &MongoOpReply{}},
	{"*models.MongoOpUnknown", &MongoOpUnknown{}},
	{"*models.MongoOpCompressed", &MongoOpCompressed{}},
	{"*models.MongoOpUpdate", &MongoOpUpdate{}},
	{"*models.MongoOpInsert", &MongoOpInsert{}},
	{"*models.MongoOpDelete", &MongoOpDelete{}},
	{"*models.MongoOpGetMore", &MongoOpGetMore{}},
	{"*models.MongoOpKillCursors", &MongoOpKillCursors{}},
	{"go.mongodb.org/mongo-driver/v2/bson.D", bson.D{}},
	{"go.mongodb.org/mongo-driver/v2/bson.E", bson.E{}},
	{"go.mongodb.org/mongo-driver/v2/bson.A", bson.A{}},
	{"go.mongodb.org/mongo-driver/v2/bson.Binary", bson.Binary{}},
	{"go.mongodb.org/mongo-driver/v2/bson.M", bson.M{}},
	{"go.mongodb.org/mongo-driver/v2/bson.ObjectID", bson.ObjectID{}},
}

// gobFramedName is the exact byte pattern gob writes for a concrete type name
// inside an interface value: the name length as a single gob uint byte (every
// name here is < 128 bytes, so encodeUint emits one byte) followed by the name.
// Framing by length avoids a substring false match — "*mysql.Request" must not
// be considered present merely because "*mysql.RequestYaml" is.
func gobFramedName(name string) []byte {
	return append([]byte{byte(len(name))}, name...)
}

func TestGobWireNamesAreFrozen(t *testing.T) {
	type box struct{ I any }
	for _, tc := range goldenGobNames {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(box{I: tc.value}); err != nil {
			t.Errorf("%T: not gob-registered (encode failed): %v", tc.value, err)
			continue
		}
		if !bytes.Contains(buf.Bytes(), gobFramedName(tc.name)) {
			t.Errorf("%T: gob wire name is no longer %q. This changes the mocks.gob "+
				"format and breaks decoding of existing files and of peers built from "+
				"another commit. If intentional, bump gobMockMagic and update goldenGobNames.",
				tc.value, tc.name)
		}
		var got box
		if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
			t.Errorf("%T: interface round-trip decode failed: %v", tc.value, err)
			continue
		}
		if reflect.TypeOf(got.I) != reflect.TypeOf(tc.value) {
			t.Errorf("round-trip for %q returned %T, want %T", tc.name, got.I, tc.value)
		}
	}
}

// TestNoDuplicateGoldenNames guards the list itself: a copy-paste that maps two
// types to one wire name would make gob decode the wrong concrete type.
func TestNoDuplicateGoldenNames(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range goldenGobNames {
		if prev, ok := seen[tc.name]; ok {
			t.Errorf("duplicate gob wire name %q (types %s and %T)", tc.name, prev, tc.value)
		}
		seen[tc.name] = reflect.TypeOf(tc.value).String()
	}
}

// goldenGobV1Base64 is a gob-encoded []any holding one value of every registered
// type, produced by the ORIGINAL gob.Register(&T{}) code (before the switch to
// gob.RegisterName). It is embedded rather than a testdata file because the repo
// .gitignore excludes testdata/. Regenerate (as _v2) only when gobMockMagic is
// bumped on purpose. See TestDecodesPreChangeGobBlob.
const goldenGobV1Base64 = "" +
	"C38CAQL/gAABEAAA/4b/gABGCypteXNxbC5TcGVj/4EDAQEEU3BlYwH/ggABBgEITWV0YWRhdGEB/4QAAQhSZXF1ZXN0cwH/kAAB" +
	"CFJlc3BvbnNlAf+UAAEJQ3JlYXRlZEF0AQQAARBSZXFUaW1lc3RhbXBNb2NrAf+WAAEQUmVzVGltZXN0YW1wTW9jawH/lgAAACH/" +
	"gwQBARFtYXBbc3RyaW5nXXN0cmluZwH/hAABDAEMAAAi/48CAQETW11teXNxbC5SZXF1ZXN0WWFtbAH/kAAB/4YAADz/hQMBAQtS" +
	"ZXF1ZXN0WWFtbAH/hgABAwEGSGVhZGVyAf+IAAEETWV0YQH/hAABB01lc3NhZ2UB/4wAAAAt/4cDAQEKUGFja2V0SW5mbwH/iAAB" +
	"AgEGSGVhZGVyAf+KAAEEVHlwZQEMAAAANf+JAwEBBkhlYWRlcgH/igABAgENUGF5bG9hZExlbmd0aAEGAAEKU2VxdWVuY2VJRAEG" +
	"AAAA/57/iwMBAQROb2RlAf+MAAEMAQRLaW5kAQYAAQVTdHlsZQEGAAEDVGFnAQwAAQVWYWx1ZQEMAAEGQW5jaG9yAQwAAQVBbGlh" +
	"cwH/jAABB0NvbnRlbnQB/44AAQtIZWFkQ29tbWVudAEMAAELTGluZUNvbW1lbnQBDAABC0Zvb3RDb21tZW50AQwAAQRMaW5lAQQA" +
	"AQZDb2x1bW4BBAAAABv/jQIBAQxbXSp5YW1sLk5vZGUB/44AAf+MAAAj/5MCAQEUW11teXNxbC5SZXNwb25zZVlhbWwB/5QAAf+S" +
	"AAA9/5EDAQEMUmVzcG9uc2VZYW1sAf+SAAEDAQZIZWFkZXIB/4gAAQRNZXRhAf+EAAEHTWVzc2FnZQH/jAAAABD/lQUBAQRUaW1l" +
	"Af+WAAAA/4P/ggEAEipteXNxbC5SZXF1ZXN0WWFtbP+GAwMAABMqbXlzcWwuUmVzcG9uc2VZYW1s/5IDAwAAESpteXNxbC5QYWNr" +
	"ZXRJbmZv/4gBAA4qbXlzcWwuUmVxdWVzdP+XAwEBB1JlcXVlc3QB/5gAAQEBDFBhY2tldEJ1bmRsZQH/mgAAADz/mQMBAQxQYWNr" +
	"ZXRCdW5kbGUB/5oAAQMBBkhlYWRlcgH/iAABB01lc3NhZ2UBEAABBE1ldGEB/4QAAABK/5gDAQAADypteXNxbC5SZXNwb25zZf+b" +
	"AwEBCFJlc3BvbnNlAf+cAAECAQxQYWNrZXRCdW5kbGUB/5oAAQdQYXlsb2FkAQwAAABY/5wDAQAAEypteXNxbC5QYWNrZXRCdW5k" +
	"bGX/mgEADSpteXNxbC5QYWNrZXT/nQMBAQZQYWNrZXQB/54AAQIBBkhlYWRlcgH/igABB1BheWxvYWQBCgAAAP+i/54DAQAADSpt" +
	"eXNxbC5IZWFkZXL/igEAEipteXNxbC5RdWVyeVBhY2tldP+fAwEBC1F1ZXJ5UGFja2V0Af+gAAEGAQdDb21tYW5kAQYAAQ5QYXJh" +
	"bWV0ZXJDb3VudAEEAAEKTnVsbEJpdG1hcAEKAAERTmV3UGFyYW1zQmluZEZsYWcBBgABClBhcmFtZXRlcnMB/6QAAQVRdWVyeQEM" +
	"AAAAIP+jAgEBEVtdbXlzcWwuUGFyYW1ldGVyAf+kAAH/ogAAQP+hAwEBCVBhcmFtZXRlcgH/ogABBAEEVHlwZQEGAAEIVW5zaWdu" +
	"ZWQBAgABBE5hbWUBDAABBVZhbHVlARAAAABm/6ABAB8qbXlzcWwuTG9jYWxJbkZpbGVSZXF1ZXN0UGFja2V0/6UDAQEYTG9jYWxJ" +
	"bkZpbGVSZXF1ZXN0UGFja2V0Af+mAAECAQpQYWNrZXRUeXBlAQYAAQhGaWxlbmFtZQEMAAAA/4L/pgEAFCpteXNxbC5UZXh0UmVz" +
	"dWx0U2V0/6cDAQENVGV4dFJlc3VsdFNldAH/qAABBQELQ29sdW1uQ291bnQBBgABB0NvbHVtbnMB/6wAAQ9FT0ZBZnRlckNvbHVt" +
	"bnMBCgABBFJvd3MB/7QAAQ1GaW5hbFJlc3BvbnNlAf+2AAAAKv+rAgEBG1tdKm15c3FsLkNvbHVtbkRlZmluaXRpb240MQH/rAAB" +
	"/6oAAP/J/6kDAQL/qgABDwEGSGVhZGVyAf+KAAEHQ2F0YWxvZwEMAAEGU2NoZW1hAQwAAQVUYWJsZQEMAAEIT3JnVGFibGUBDAAB" +
	"BE5hbWUBDAABB09yZ05hbWUBDAABC0ZpeGVkTGVuZ3RoAQYAAQxDaGFyYWN0ZXJTZXQBBgABDENvbHVtbkxlbmd0aAEGAAEEVHlw" +
	"ZQEGAAEFRmxhZ3MBBgABCERlY2ltYWxzAQYAAQZGaWxsZXIBCgABDERlZmF1bHRWYWx1ZQEMAAAAH/+zAgEBEFtdKm15c3FsLlRl" +
	"eHRSb3cB/7QAAf+uAAAk/60DAQL/rgABAgEGSGVhZGVyAf+KAAEGVmFsdWVzAf+yAAAAIv+xAgEBE1tdbXlzcWwuQ29sdW1uRW50" +
	"cnkB/7IAAf+wAABC/68DAQELQ29sdW1uRW50cnkB/7AAAQQBBFR5cGUBBgABBE5hbWUBDAABBVZhbHVlARAAAQhVbnNpZ25lZAEC" +
	"AAAAL/+1AwEBD0dlbmVyaWNSZXNwb25zZQH/tgABAgEERGF0YQEKAAEEVHlwZQEMAAAA/5b/qAEAHipteXNxbC5CaW5hcnlQcm90" +
	"b2NvbFJlc3VsdFNldP+3AwEBF0JpbmFyeVByb3RvY29sUmVzdWx0U2V0Af+4AAEFAQtDb2x1bW5Db3VudAEGAAEHQ29sdW1ucwH/" +
	"rAABD0VPRkFmdGVyQ29sdW1ucwEKAAEEUm93cwH/vAABDUZpbmFsUmVzcG9uc2UB/7YAAAAh/7sCAQESW10qbXlzcWwuQmluYXJ5" +
	"Um93Af+8AAH/ugAARf+5AwEC/7oAAQQBBkhlYWRlcgH/igABBlZhbHVlcwH/sgABCk9rQWZ0ZXJSb3cBAgABDVJvd051bGxCdWZm" +
	"ZXIBCgAAAFX/uAEAFipteXNxbC5HZW5lcmljUmVzcG9uc2X/tgEAEipteXNxbC5Db2x1bW5Db3VudP+9AwEBC0NvbHVtbkNvdW50" +
	"Af++AAEBAQVDb3VudAEGAAAA/7X/vgEAGSpteXNxbC5Db2x1bW5EZWZpbml0aW9uNDH/qgMBAAAOKm15c3FsLlRleHRSb3f/rgMB" +
	"AAAQKm15c3FsLkJpbmFyeVJvd/+6AwEAABIqbXlzcWwuQ29sdW1uRW50cnn/sAEAGCpteXNxbC5TdG10UHJlcGFyZVBhY2tldP+/" +
	"AwEBEVN0bXRQcmVwYXJlUGFja2V0Af/AAAECAQdDb21tYW5kAQYAAQVRdWVyeQEMAAAA/gEe/8ABABoqbXlzcWwuU3RtdFByZXBh" +
	"cmVPa1BhY2tldP/BAwEBE1N0bXRQcmVwYXJlT2tQYWNrZXQB/8IAAQ0BBlN0YXR1cwEGAAELU3RhdGVtZW50SUQBBgABCk51bUNv" +
	"bHVtbnMBBgABCU51bVBhcmFtcwEGAAEGRmlsbGVyAQYAARBXYXJuaW5nQXZhaWxhYmxlAQIAAQxXYXJuaW5nQ291bnQBBgABFE1l" +
	"dGFGb2xsb3dzQXZhaWxhYmxlAQIAAQtNZXRhRm9sbG93cwEGAAEJUGFyYW1EZWZzAf+sAAERRU9GQWZ0ZXJQYXJhbURlZnMBCgAB" +
	"CkNvbHVtbkRlZnMB/6wAARJFT0ZBZnRlckNvbHVtbkRlZnMBCgAAAP+8/8IBABgqbXlzcWwuU3RtdEV4ZWN1dGVQYWNrZXT/wwMB" +
	"ARFTdG10RXhlY3V0ZVBhY2tldAH/xAABCAEGU3RhdHVzAQYAAQtTdGF0ZW1lbnRJRAEGAAEFRmxhZ3MBBgABDkl0ZXJhdGlvbkNv" +
	"dW50AQYAAQ5QYXJhbWV0ZXJDb3VudAEEAAEKTnVsbEJpdG1hcAEKAAERTmV3UGFyYW1zQmluZEZsYWcBBgABClBhcmFtZXRlcnMB" +
	"/6QAAAB0/8QBABAqbXlzcWwuUGFyYW1ldGVy/6IBABYqbXlzcWwuU3RtdEZldGNoUGFja2V0/8UDAQEPU3RtdEZldGNoUGFja2V0" +
	"Af/GAAEDAQZTdGF0dXMBBgABC1N0YXRlbWVudElEAQYAAQdOdW1Sb3dzAQYAAABT/8YBABYqbXlzcWwuU3RtdENsb3NlUGFja2V0" +
	"/8cDAQEPU3RtdENsb3NlUGFja2V0Af/IAAECAQZTdGF0dXMBBgABC1N0YXRlbWVudElEAQYAAABT/8gBABYqbXlzcWwuU3RtdFJl" +
	"c2V0UGFja2V0/8kDAQEPU3RtdFJlc2V0UGFja2V0Af/KAAECAQZTdGF0dXMBBgABC1N0YXRlbWVudElEAQYAAAB6/8oBAB0qbXlz" +
	"cWwuU3RtdFNlbmRMb25nRGF0YVBhY2tldP/LAwEBFlN0bXRTZW5kTG9uZ0RhdGFQYWNrZXQB/8wAAQQBBlN0YXR1cwEGAAELU3Rh" +
	"dGVtZW50SUQBBgABC1BhcmFtZXRlcklEAQYAAQREYXRhAQoAAAA6/8wBABEqbXlzcWwuUXVpdFBhY2tldP/NAwEBClF1aXRQYWNr" +
	"ZXQB/84AAQEBB0NvbW1hbmQBBgAAAEn/zgEAEypteXNxbC5Jbml0REJQYWNrZXT/zwMBAQxJbml0REJQYWNrZXQB/9AAAQIBB0Nv" +
	"bW1hbmQBBgABBlNjaGVtYQEMAAAARv/QAQAXKm15c3FsLlN0YXRpc3RpY3NQYWNrZXT/0QMBARBTdGF0aXN0aWNzUGFja2V0Af/S" +
	"AAEBAQdDb21tYW5kAQYAAAA8/9IBABIqbXlzcWwuRGVidWdQYWNrZXT/0wMBAQtEZWJ1Z1BhY2tldAH/1AABAQEHQ29tbWFuZAEG" +
	"AAAAOv/UAQARKm15c3FsLlBpbmdQYWNrZXT/1QMBAQpQaW5nUGFja2V0Af/WAAEBAQdDb21tYW5kAQYAAABQ/9YBABwqbXlzcWwu" +
	"UmVzZXRDb25uZWN0aW9uUGFja2V0/9cDAQEVUmVzZXRDb25uZWN0aW9uUGFja2V0Af/YAAEBAQdDb21tYW5kAQYAAABO/9gBABYq" +
	"bXlzcWwuU2V0T3B0aW9uUGFja2V0/9kDAQEPU2V0T3B0aW9uUGFja2V0Af/aAAECAQZTdGF0dXMBBgABBk9wdGlvbgEGAAAARv/a" +
	"AQAXKm15c3FsLkNoYW5nZVVzZXJQYWNrZXT/2wMBARBDaGFuZ2VVc2VyUGFja2V0Af/cAAEBAQdDb21tYW5kAQYAAAB9/9wBAA8q" +
	"bXlzcWwuT0tQYWNrZXT/3QMBAQhPS1BhY2tldAH/3gABBgEGSGVhZGVyAQYAAQxBZmZlY3RlZFJvd3MBBgABDExhc3RJbnNlcnRJ" +
	"RAEGAAELU3RhdHVzRmxhZ3MBBgABCFdhcm5pbmdzAQYAAQRJbmZvAQwAAAB2/94BABAqbXlzcWwuRVJSUGFja2V0/98DAQEJRVJS" +
	"UGFja2V0Af/gAAEFAQZIZWFkZXIBBgABCUVycm9yQ29kZQEGAAEOU1FMU3RhdGVNYXJrZXIBDAABCFNRTFN0YXRlAQwAAQxFcnJv" +
	"ck1lc3NhZ2UBDAAAAFT/4AEAECpteXNxbC5FT0ZQYWNrZXT/4QMBAQlFT0ZQYWNrZXQB/+IAAQMBBkhlYWRlcgEGAAEIV2Fybmlu" +
	"Z3MBBgABC1N0YXR1c0ZsYWdzAQYAAAD/2//iAQAZKm15c3FsLkhhbmRzaGFrZVYxMFBhY2tldP/jAwEBEkhhbmRzaGFrZVYxMFBh" +
	"Y2tldAH/5AABCQEPUHJvdG9jb2xWZXJzaW9uAQYAAQ1TZXJ2ZXJWZXJzaW9uAQwAAQxDb25uZWN0aW9uSUQBBgABDkF1dGhQbHVn" +
	"aW5EYXRhAQoAAQZGaWxsZXIBBgABD0NhcGFiaWxpdHlGbGFncwEGAAEMQ2hhcmFjdGVyU2V0AQYAAQtTdGF0dXNGbGFncwEGAAEO" +
	"QXV0aFBsdWdpbk5hbWUBDAAAAP4BAP/kAQAgKm15c3FsLkhhbmRzaGFrZVJlc3BvbnNlNDFQYWNrZXT/5QMBARlIYW5kc2hha2VS" +
	"ZXNwb25zZTQxUGFja2V0Af/mAAEKAQ9DYXBhYmlsaXR5RmxhZ3MBBgABDU1heFBhY2tldFNpemUBBgABDENoYXJhY3RlclNldAEG" +
	"AAEGRmlsbGVyAf/oAAEIVXNlcm5hbWUBDAABDEF1dGhSZXNwb25zZQEKAAEIRGF0YWJhc2UBDAABDkF1dGhQbHVnaW5OYW1lAQwA" +
	"ARRDb25uZWN0aW9uQXR0cmlidXRlcwH/hAABFFpzdGRDb21wcmVzc2lvbkxldmVsAQYAAAAZ/+cBAQEJWzIzXXVpbnQ4Af/oAAEG" +
	"AS4AAP+W/+YaBBcAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAXKm15c3FsLlNTTFJlcXVlc3RQYWNrZXT/6QMBARBTU0xSZXF1ZXN0" +
	"UGFja2V0Af/qAAEEAQ9DYXBhYmlsaXR5RmxhZ3MBBgABDU1heFBhY2tldFNpemUBBgABDENoYXJhY3RlclNldAEGAAEGRmlsbGVy" +
	"Af/oAAAA/43/6hoEFwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB4qbXlzcWwuQXV0aFN3aXRjaFJlcXVlc3RQYWNrZXT/6wMBARdB" +
	"dXRoU3dpdGNoUmVxdWVzdFBhY2tldAH/7AABAwEJU3RhdHVzVGFnAQYAAQpQbHVnaW5OYW1lAQwAAQpQbHVnaW5EYXRhAQwAAABT" +
	"/+wBAB8qbXlzcWwuQXV0aFN3aXRjaFJlc3BvbnNlUGFja2V0/+0DAQEYQXV0aFN3aXRjaFJlc3BvbnNlUGFja2V0Af/uAAEBAQRE" +
	"YXRhAQwAAABV/+4BABkqbXlzcWwuQXV0aE1vcmVEYXRhUGFja2V0/+8DAQESQXV0aE1vcmVEYXRhUGFja2V0Af/wAAECAQlTdGF0" +
	"dXNUYWcBBgABBERhdGEBDAAAAG//8AEAGypteXNxbC5BdXRoTmV4dEZhY3RvclBhY2tldP/xAwEBFEF1dGhOZXh0RmFjdG9yUGFj" +
	"a2V0Af/yAAEDAQpQYWNrZXRUeXBlAQYAAQpQbHVnaW5OYW1lAQwAAQpQbHVnaW5EYXRhAQwAAAD/if/yAQAOKnBvc3RncmVzLlNw" +
	"ZWP/8wMBAQRTcGVjAf/0AAEGAQhNZXRhZGF0YQH/hAABCFJlcXVlc3RzAf/4AAEIUmVzcG9uc2UB//wAAQlDcmVhdGVkQXQBBAAB" +
	"EFJlcVRpbWVzdGFtcE1vY2sB/5YAARBSZXNUaW1lc3RhbXBNb2NrAf+WAAAAJf/3AgEBFltdcG9zdGdyZXMuUmVxdWVzdFlhbWwB" +
	"//gAAf/2AAAw//UDAQELUmVxdWVzdFlhbWwB//YAAQIBBE1ldGEB/4QAAQdNZXNzYWdlAf+MAAAAJv/7AgEBF1tdcG9zdGdyZXMu" +
	"UmVzcG9uc2VZYW1sAf/8AAH/+gAAMf/5AwEBDFJlc3BvbnNlWWFtbAH/+gABAgEETWV0YQH/hAABB01lc3NhZ2UB/4wAAAD/gP/0" +
	"AQAVKnBvc3RncmVzLlJlcXVlc3RZYW1s//YDAgAAFipwb3N0Z3Jlcy5SZXNwb25zZVlhbWz/+gMCAAAUKnBvc3RncmVzLlBhY2tl" +
	"dEluZm///QMBAQpQYWNrZXRJbmZvAf/+AAECAQZIZWFkZXIB/gEAAAEEVHlwZQEMAAAANP//AwEBBkhlYWRlcgH+AQAAAQIBDVBh" +
	"eWxvYWRMZW5ndGgBBgABCFBhY2tldElEAQwAAABA//4BABEqcG9zdGdyZXMuUmVxdWVzdP4BAQMBAQdSZXF1ZXN0Af4BAgABAQEM" +
	"UGFja2V0QnVuZGxlAf4BBAAAACr+AQMDAQEMUGFja2V0QnVuZGxlAf4BBAABAQEHUGFja2V0cwH+AQgAAAAj/gEHAgEBEVtdcG9z" +
	"dGdyZXMuUGFja2V0Af4BCAAB/gEGAAA4/gEFAwEBBlBhY2tldAH+AQYAAQMBBkhlYWRlcgH//gABB01lc3NhZ2UBEAABBE1ldGEB" +
	"/4QAAABR/gECAwEAABIqcG9zdGdyZXMuUmVzcG9uc2X+AQkDAQEIUmVzcG9uc2UB/gEKAAECAQxQYWNrZXRCdW5kbGUB/gEEAAEH" +
	"UGF5bG9hZAEMAAAA/63+AQoDAQAAFipwb3N0Z3Jlcy5QYWNrZXRCdW5kbGX+AQQBABAqcG9zdGdyZXMuUGFja2V0/gEGAQAQKnBv" +
	"c3RncmVzLkhlYWRlcv4BAAEAFiptb2RlbHMuTW9uZ29PcE1lc3NhZ2X+AQsDAQEOTW9uZ29PcE1lc3NhZ2UB/gEMAAEDAQhGbGFn" +
	"Qml0cwEEAAEIU2VjdGlvbnMB/gEOAAEIQ2hlY2tzdW0BBAAAABj+AQ0CAQEIW11zdHJpbmcB/gEOAAEMAAD/nv4BDAEAFCptb2Rl" +
	"bHMuTW9uZ29PcFF1ZXJ5/gEPAwEBDE1vbmdvT3BRdWVyeQH+ARAAAQYBBUZsYWdzAQQAARJGdWxsQ29sbGVjdGlvbk5hbWUBDAAB" +
	"DE51bWJlclRvU2tpcAEEAAEOTnVtYmVyVG9SZXR1cm4BBAABBVF1ZXJ5AQwAARRSZXR1cm5GaWVsZHNTZWxlY3RvcgEMAAAA/4n+" +
	"ARABABQqbW9kZWxzLk1vbmdvT3BSZXBsef4BEQMBAQxNb25nb09wUmVwbHkB/gESAAEFAQ1SZXNwb25zZUZsYWdzAQQAAQhDdXJz" +
	"b3JJRAEEAAEMU3RhcnRpbmdGcm9tAQQAAQ5OdW1iZXJSZXR1cm5lZAEEAAEJRG9jdW1lbnRzAf4BDgAAAE7+ARIBABYqbW9kZWxz" +
	"Lk1vbmdvT3BVbmtub3du/gETAwEBDk1vbmdvT3BVbmtub3duAf4BFAABAgEGT3Bjb2RlAQQAAQREYXRhAQoAAAD/jP4BFAEAGSpt" +
	"b2RlbHMuTW9uZ29PcENvbXByZXNzZWT+ARUDAQERTW9uZ29PcENvbXByZXNzZWQB/gEWAAEEAQ5PcmlnaW5hbE9wY29kZQEEAAEQ" +
	"VW5jb21wcmVzc2VkU2l6ZQEEAAEMQ29tcHJlc3NvcklEAQQAAQ5Db21wcmVzc2VkRGF0YQEKAAAAcf4BFgEAFSptb2RlbHMuTW9u" +
	"Z29PcFVwZGF0Zf4BFwMBAQ1Nb25nb09wVXBkYXRlAf4BGAABBAESRnVsbENvbGxlY3Rpb25OYW1lAQwAAQVGbGFncwEEAAEIU2Vs" +
	"ZWN0b3IBDAABBlVwZGF0ZQEMAAAAaf4BGAEAFSptb2RlbHMuTW9uZ29PcEluc2VydP4BGQMBAQ1Nb25nb09wSW5zZXJ0Af4BGgAB" +
	"AwEFRmxhZ3MBBAABEkZ1bGxDb2xsZWN0aW9uTmFtZQEMAAEJRG9jdW1lbnRzAf4BDgAAAGb+ARoBABUqbW9kZWxzLk1vbmdvT3BE" +
	"ZWxldGX+ARsDAQENTW9uZ29PcERlbGV0ZQH+ARwAAQMBEkZ1bGxDb2xsZWN0aW9uTmFtZQEMAAEFRmxhZ3MBBAABCFNlbGVjdG9y" +
	"AQwAAABx/gEcAQAWKm1vZGVscy5Nb25nb09wR2V0TW9yZf4BHQMBAQ5Nb25nb09wR2V0TW9yZQH+AR4AAQMBEkZ1bGxDb2xsZWN0" +
	"aW9uTmFtZQEMAAEOTnVtYmVyVG9SZXR1cm4BBAABCEN1cnNvcklEAQQAAABo/gEeAQAaKm1vZGVscy5Nb25nb09wS2lsbEN1cnNv" +
	"cnP+AR8DAQESTW9uZ29PcEtpbGxDdXJzb3JzAf4BIAABAgERTnVtYmVyT2ZDdXJzb3JJRHMBBAABCUN1cnNvcklEcwH+ASIAAAAX" +
	"/gEhAgEBB1tdaW50NjQB/gEiAAEEAAA+/gEgAQAlZ28ubW9uZ29kYi5vcmcvbW9uZ28tZHJpdmVyL3YyL2Jzb24uRP4BJQIBAQFE" +
	"Af4BJgAB/gEkAAAj/gEjAwEBAUUB/gEkAAECAQNLZXkBDAABBVZhbHVlARAAAABo/gEmAgAAJWdvLm1vbmdvZGIub3JnL21vbmdv" +
	"LWRyaXZlci92Mi9ic29uLkX+ASQBACVnby5tb25nb2RiLm9yZy9tb25nby1kcml2ZXIvdjIvYnNvbi5B/gEnAgEBAUEB/gEoAAEQ" +
	"AABc/gEoAgAAKmdvLm1vbmdvZGIub3JnL21vbmdvLWRyaXZlci92Mi9ic29uLkJpbmFyef4BKQMBAQZCaW5hcnkB/gEqAAECAQdT" +
	"dWJ0eXBlAQYAAQREYXRhAQoAAAA+/gEqAQAlZ28ubW9uZ29kYi5vcmcvbW9uZ28tZHJpdmVyL3YyL2Jzb24uTf4BKwQBAQFNAf4B" +
	"LAABDAEQAABN/gEsAgAALGdvLm1vbmdvZGIub3JnL21vbmdvLWRyaXZlci92Mi9ic29uLk9iamVjdElE/gEtAQEBCE9iamVjdElE" +
	"Af4BLgABBgEYAAAS/gEuDgAMAAAAAAAAAAAAAAAA"

// TestDecodesPreChangeGobBlob is the real backward-compatibility guard.
//
// It decodes goldenGobV1Base64 (above) — a blob the ORIGINAL gob.Register(&T{})
// code wrote — and proves the current registrations still resolve the exact wire
// names that build produced, i.e. that every pinned literal equals the old
// default, so real mocks.gob files recorded by older keploy versions keep
// decoding. A wrong literal makes gob fail to resolve that element's concrete
// type and this test fails, which the same-literal freeze test above cannot
// catch.
func TestDecodesPreChangeGobBlob(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString(goldenGobV1Base64)
	if err != nil {
		t.Fatalf("decode embedded golden blob: %v", err)
	}
	var decoded []any
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&decoded); err != nil {
		t.Fatalf("current code cannot decode a mocks.gob written by the pre-RegisterName "+
			"build — a pinned wire name differs from gob's old default and existing mocks "+
			"would break: %v", err)
	}
	if len(decoded) != len(goldenGobNames) {
		t.Fatalf("golden blob has %d values, goldenGobNames has %d — regenerate the blob",
			len(decoded), len(goldenGobNames))
	}
	// Order-independent: the set of concrete Go types recovered from the old-format
	// bytes must equal the set we register today.
	got := make([]string, len(decoded))
	for i, v := range decoded {
		got[i] = reflect.TypeOf(v).String()
	}
	want := make([]string, len(goldenGobNames))
	for i, tc := range goldenGobNames {
		want[i] = reflect.TypeOf(tc.value).String()
	}
	sort.Strings(got)
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pre-change blob decoded to a different type set at %d: got %q want %q",
				i, got[i], want[i])
		}
	}
}
