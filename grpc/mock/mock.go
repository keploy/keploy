package mock

import (
	"encoding/base64"
	"fmt"

	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/grpc/utils"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func Encode(doc *proto.Mock, log *zap.Logger) models.Mock {
	res := models.Mock{
		Version: doc.Version,
		Kind:    doc.Kind,
		Name:    doc.Name,
	}
	switch doc.Kind {
	case string(models.HTTP_EXPORT):
		spec := models.HttpSpec{
			Metadata: doc.Spec.Metadata,
			Request: models.HttpReq{
				Method:     models.Method(doc.Spec.Req.Method),
				ProtoMajor: int(doc.Spec.Req.ProtoMajor),
				ProtoMinor: int(doc.Spec.Req.ProtoMinor),
				URL:        doc.Spec.Req.URL,
				Header:     utils.GetHttpHeader(doc.Spec.Req.Header),
				Body:       doc.Spec.Req.Body,
			},
			Response: models.HttpResp{
				StatusCode: int(doc.Spec.Res.StatusCode),
				Header:     utils.GetHttpHeader(doc.Spec.Res.Header),
				Body:       doc.Spec.Res.Body,
			},
			Objects: []models.Object{{
				Type: doc.Spec.Objects[0].Type,
				Data: string(doc.Spec.Objects[0].Data),
			}},
			Mocks:      doc.Spec.Mocks,
			Assertions: utils.GetHttpHeader(doc.Spec.Assertions),
		}
		err := res.Spec.Encode(&spec)
		if err != nil {
			log.Error(fmt.Sprint("Failed to encode http spec for mock with name: ", doc.Name), zap.Error(err))
		}
	case string(models.GENERIC_EXPORT):
		err := res.Spec.Encode(&models.GenericSpec{
			Metadata: doc.Spec.Metadata,
			Objects:  ToModelObjects(doc.Spec.Objects),
		})
		if err != nil {
			log.Error(fmt.Sprint("Failed to encode generic spec for mock with name: ", doc.Name), zap.Error(err))
		}
	default:
		log.Error(fmt.Sprint("Mock with name ", doc.Name, " is not of a valid kind"))
	}
	return res
}

func ToModelObjects(objs []*proto.Mock_Object) []models.Object {
	res := []models.Object{}
	for _, j := range objs {
		res = append(res, models.Object{
			Type: j.Type,
			Data: base64.StdEncoding.EncodeToString(j.Data),
		})
	}
	return res
}

func toProtoObjects(objs []models.Object, log *zap.Logger) []*proto.Mock_Object {
	res := []*proto.Mock_Object{}
	for _, j := range objs {
		bin, err := base64.StdEncoding.DecodeString(j.Data)
		if err != nil {
			log.Error("failed to decode base64 data from yaml file into byte array", zap.Error(err))
			continue
		}
		res = append(res, &proto.Mock_Object{
			Type: j.Type,
			Data: bin,
		})
	}
	return res
}

func Decode(doc []models.Mock, log *zap.Logger) []*proto.Mock {
	res := []*proto.Mock{}
	for _, j := range doc {
		mock := &proto.Mock{
			Version: j.Version,
			Name:    j.Name,
			Kind:    j.Kind,
		}
		switch j.Kind {
		case string(models.HTTP_EXPORT):
			spec := &models.HttpSpec{}
			err := j.Spec.Decode(spec)
			if err != nil {
				log.Error(fmt.Sprint("Failed to decode the http spec of mock with name: ", j.Name), zap.Error(err))
			}
			mock.Spec = &proto.Mock_SpecSchema{
				Metadata: spec.Metadata,
				Req: &proto.HttpReq{
					Method:     string(spec.Request.Method),
					ProtoMajor: int64(spec.Request.ProtoMajor),
					ProtoMinor: int64(spec.Request.ProtoMinor),
					URL:        spec.Request.URL,
					Header:     utils.GetProtoMap(spec.Request.Header),
					Body:       spec.Request.Body,
				},
				Objects: []*proto.Mock_Object{{
					Type: spec.Objects[0].Type,
					Data: []byte(spec.Objects[0].Data),
				}},
				Res: &proto.HttpResp{
					StatusCode: int64(spec.Response.StatusCode),
					Header:     utils.GetProtoMap(spec.Response.Header),
					Body:       spec.Response.Body,
				},
				Mocks:      spec.Mocks,
				Assertions: utils.GetProtoMap(spec.Assertions),	
			}
		case string(models.GENERIC_EXPORT):
			spec := &models.GenericSpec{}
			err := j.Spec.Decode(spec)
			if err != nil {
				log.Error(fmt.Sprint("Failed to decode the generic spec of mock with name: ", j.Name), zap.Error(err))
			}
			mock.Spec = &proto.Mock_SpecSchema{
				Metadata: spec.Metadata,
				Objects:  toProtoObjects(spec.Objects, log),
			}
		default:
			log.Error(fmt.Sprint("Mock with name ", j.Name, " is not of a valid kind"))
		}
		res = append(res, mock)
	}
	return res
}
