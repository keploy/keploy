package grpcserver

import (
	"encoding/base64"
	"fmt"

	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func (srv *Server) Encode(doc *proto.Mock) models.Mock {
	res := models.Mock{
		Version: doc.Version,
		Kind:    doc.Kind,
		Name:    doc.Name,
	}
	switch doc.Kind {
	case string(models.HTTP_EXPORT):
		err := res.Spec.Encode(&models.HttpSpec{
			Metadata: doc.Spec.Metadata,
			Request: models.HttpReq{
				Method:     models.Method(doc.Spec.Req.Method),
				ProtoMajor: int(doc.Spec.Req.ProtoMajor),
				ProtoMinor: int(doc.Spec.Req.ProtoMinor),
				URL:        doc.Spec.Req.URL,
				Header:     getHttpHeader(doc.Spec.Req.Header),
				Body:       doc.Spec.Req.Body,
			},
			Response: models.HttpResp{
				StatusCode: int(doc.Spec.Res.StatusCode),
				Header:     getHttpHeader(doc.Spec.Res.Header),
				Body:       doc.Spec.Res.Body,
			},
			Objects: []models.Object{{
				Type: doc.Spec.Objects[0].Type,
				Data: string(doc.Spec.Objects[0].Data),
			}},
		})
		if err != nil {
			srv.logger.Error(fmt.Sprint("Failed to encode http spec for mock with name: ", doc.Name), zap.Error(err))
		}
	case string(models.GENERIC_EXPORT):
		err := res.Spec.Encode(&models.GenericSpec{
			Metadata: doc.Spec.Metadata,
			Objects:  toModelObjects(doc.Spec.Objects),
		})
		if err != nil {
			srv.logger.Error(fmt.Sprint("Failed to encode generic spec for mock with name: ", doc.Name), zap.Error(err))
		}
	default:
		srv.logger.Error(fmt.Sprint("Mock with name ", doc.Name, " is not of a valid kind"))
	}
	return res
}

func toModelObjects(objs []*proto.Mock_Object) []models.Object {
	res := []models.Object{}
	for _, j := range objs {
		res = append(res, models.Object{
			Type: j.Type,
			Data: base64.StdEncoding.EncodeToString(j.Data),
		})
	}
	return res
}

func (srv *Server) toProtoObjects(objs []models.Object) []*proto.Mock_Object {
	res := []*proto.Mock_Object{}
	for _, j := range objs {
		bin, err := base64.StdEncoding.DecodeString(j.Data)
		if err != nil {
			srv.logger.Error("failed to decode base64 data from yaml file into byte array", zap.Error(err))
			continue
		}
		res = append(res, &proto.Mock_Object{
			Type: j.Type,
			Data: bin,
		})
	}
	return res
}

func (srv *Server) Decode(doc []models.Mock) []*proto.Mock {
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
				srv.logger.Error(fmt.Sprint("Failed to decode the http spec of mock with name: ", j.Name), zap.Error(err))
			}
			mock.Spec = &proto.Mock_SpecSchema{
				Metadata: spec.Metadata,
				Req: &proto.HttpReq{
					Method:     string(spec.Request.Method),
					ProtoMajor: int64(spec.Request.ProtoMajor),
					ProtoMinor: int64(spec.Request.ProtoMinor),
					URL:        spec.Request.URL,
					Header:     getProtoMap(spec.Request.Header),
					Body:       spec.Request.Body,
				},
				Objects: []*proto.Mock_Object{{
					Type: spec.Objects[0].Type,
					Data: []byte(spec.Objects[0].Data),
				}},
				Res: &proto.HttpResp{
					StatusCode: int64(spec.Response.StatusCode),
					Header:     getProtoMap(spec.Response.Header),
					Body:       spec.Response.Body,
				},
			}
		case string(models.GENERIC_EXPORT):
			spec := &models.GenericSpec{}
			err := j.Spec.Decode(spec)
			if err != nil {
				srv.logger.Error(fmt.Sprint("Failed to decode the generic spec of mock with name: ", j.Name), zap.Error(err))
			}
			mock.Spec = &proto.Mock_SpecSchema{
				Metadata: spec.Metadata,
				Objects:  srv.toProtoObjects(spec.Objects),
			}
		default:
			srv.logger.Error(fmt.Sprint("Mock with name ", j.Name, " is not of a valid kind"))
		}
		res = append(res, mock)
	}
	return res
}
