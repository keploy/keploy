package grpc

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch a gRPC response array is compared in order even when
// the caller passes ignoreOrdering=true (test.ignoreOrdering defaults to true):
// a reorder is a difference, never forgiven as a multiset.
func TestMatch_ProvenSwitchComparesArraysInOrder(t *testing.T) {
	headers := models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{}}
	trailers := models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}}
	tc := &models.TestCase{Name: "reordered", GrpcResp: models.GrpcResp{
		Body:    models.GrpcLengthPrefixedMessage{DecodedData: `{"items": [{"id": 1}, {"id": 2}]}`, MessageLength: 30},
		Headers: headers, Trailers: trailers,
	}}
	actual := &models.GrpcResp{
		Body:    models.GrpcLengthPrefixedMessage{DecodedData: `{"items": [{"id": 2}, {"id": 1}]}`, MessageLength: 30},
		Headers: headers, Trailers: trailers,
	}

	t.Setenv(provenswitch.Env, "")
	if ok, _ := Match(tc, actual, map[string]map[string][]string{}, true, zap.NewNop(), false); !ok {
		t.Fatal("control: with the switch off and ignoreOrdering=true a reordered array must still match, as today")
	}
	t.Setenv(provenswitch.Env, "1")
	if ok, _ := Match(tc, actual, map[string]map[string][]string{}, true, zap.NewNop(), false); ok {
		t.Fatal("KEPLOY_PROVEN=1: a reordered response array matched; arrays are compared in order")
	}
}
