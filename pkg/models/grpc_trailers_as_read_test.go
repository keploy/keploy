package models

import "testing"

// A recorded answer's trailer block is read as a gRPC client reads it: its
// trailers when it has any, and, for a Trailers-Only answer (no trailers, its
// one header block carrying grpc-status: what enterprise's low-latency
// recorder kept, as headers alone), that header block. An answer with
// trailers keeps them, even when they lack the status and its headers carry
// one: a client reads the status off the block that ends the stream, never
// off the headers of a stream that went on.
func TestTrailersAsReadIsTheBlockAClientReadsTheStatusFrom(t *testing.T) {
	headers := func(kv ...string) GrpcHeaders {
		h := GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}}
		for i := 0; i < len(kv); i += 2 {
			if kv[i][0] == ':' {
				h.PseudoHeaders[kv[i]] = kv[i+1]
			} else {
				h.OrdinaryHeaders[kv[i]] = kv[i+1]
			}
		}
		return h
	}
	for _, c := range []struct {
		name       string
		resp       GrpcResp
		wantStatus string
		wantFrom   string
	}{
		{"trailers carry the status",
			GrpcResp{Headers: headers(":status", "200", "content-type", "application/grpc"), Trailers: headers("grpc-status", "13")}, "13", "trailers"},
		{"a Trailers-Only answer kept as headers alone",
			GrpcResp{Headers: headers(":status", "200", "content-type", "application/grpc", "grpc-status", "5"), Trailers: headers()}, "5", "headers"},
		{"a Trailers-Only answer kept as headers alone, decoded with no trailer maps",
			GrpcResp{Headers: headers(":status", "200", "grpc-status", "0")}, "0", "headers"},
		{"a Trailers-Only answer kept as both",
			GrpcResp{Headers: headers(":status", "200", "grpc-status", "5"), Trailers: headers(":status", "200", "grpc-status", "5")}, "5", "trailers"},
		{"trailers without the status, headers with one",
			GrpcResp{Headers: headers(":status", "200", "grpc-status", "0"), Trailers: headers("x-done", "yes")}, "", "trailers"},
		{"trailers of pseudo-headers alone, headers with a status",
			GrpcResp{Headers: headers(":status", "200", "grpc-status", "0"), Trailers: headers(":status", "200")}, "", "trailers"},
		{"no status anywhere",
			GrpcResp{Headers: headers(":status", "200", "content-type", "application/grpc"), Trailers: headers()}, "", "trailers"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.resp.TrailersAsRead()
			if s := got.OrdinaryHeaders["grpc-status"]; s != c.wantStatus {
				t.Fatalf("grpc-status %q, want %q", s, c.wantStatus)
			}
			from := "trailers"
			if len(got.PseudoHeaders)+len(got.OrdinaryHeaders) > 0 && sameHeaders(got, c.resp.Headers) && !sameHeaders(got, c.resp.Trailers) {
				from = "headers"
			}
			if from != c.wantFrom {
				t.Fatalf("read from the %s, want the %s", from, c.wantFrom)
			}
		})
	}
}

func sameHeaders(a, b GrpcHeaders) bool {
	if len(a.PseudoHeaders) != len(b.PseudoHeaders) || len(a.OrdinaryHeaders) != len(b.OrdinaryHeaders) {
		return false
	}
	for k, v := range a.PseudoHeaders {
		if b.PseudoHeaders[k] != v {
			return false
		}
	}
	for k, v := range a.OrdinaryHeaders {
		if b.OrdinaryHeaders[k] != v {
			return false
		}
	}
	return true
}
