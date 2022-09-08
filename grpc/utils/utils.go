package utils

import (
	proto "go.keploy.io/server/grpc/regression"
)

func GetHttpHeader(m map[string]*proto.StrArr) map[string][]string {
	res := map[string][]string{}
	for k, v := range m {
		res[k] = v.Value
	}
	return res
}

func GetProtoMap(m map[string][]string) map[string]*proto.StrArr {
	res := map[string]*proto.StrArr{}
	for k, v := range m {
		arr := &proto.StrArr{}
		arr.Value = append(arr.Value, v...)
		res[k] = arr
	}
	return res
}

// map[string]*StrArr --> map[string][]string
func GetStringMap(m map[string]*proto.StrArr) map[string][]string {
	res := map[string][]string{}
	for k, v := range m {
		res[k] = v.Value
	}
	return res
}

func ToStrArr(arr []string) *proto.StrArr {
	return &proto.StrArr{
		Value: arr,
	}
}
