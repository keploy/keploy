package grpcserver

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/keploy/go-sdk/integrations/kgrpcserver"
	"github.com/keploy/go-sdk/keploy"
	"go.keploy.io/server/graph"
	grpcMock "go.keploy.io/server/grpc/mock"
	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/grpc/utils"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/service/mock"
	regression2 "go.keploy.io/server/pkg/service/regression"
	tcSvc "go.keploy.io/server/pkg/service/testCase"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

type Server struct {
	logger         *zap.Logger
	testExport     bool
	testReportPath string
	svc            regression2.Service
	tcSvc          tcSvc.Service
	mock           mock.Service
	proto.UnimplementedRegressionServiceServer
}

func New(k *keploy.Keploy, logger *zap.Logger, svc regression2.Service, m mock.Service, tc tcSvc.Service, listener net.Listener, testExport bool, testReportPath string) error {

	// create an instance for grpc server
	srv := grpc.NewServer(kgrpcserver.UnaryInterceptor(k))
	proto.RegisterRegressionServiceServer(srv, &Server{
		logger:         logger,
		svc:            svc,
		mock:           m,
		testExport:     testExport,
		testReportPath: testReportPath,
		tcSvc:          tc,
	})
	reflection.Register(srv)
	err := srv.Serve(listener)
	return err

}

func (srv *Server) StartMocking(ctx context.Context, request *proto.StartMockReq) (*proto.StartMockResp, error) {
	if request.Mode == "test" {
		return &proto.StartMockResp{
			Exists: false,
		}, nil
	}
	exists, err := srv.mock.FileExists(ctx, request.Path, request.OverWrite)
	return &proto.StartMockResp{
		Exists: exists,
	}, err
}

func (srv *Server) PutMock(ctx context.Context, request *proto.PutMockReq) (*proto.PutMockResp, error) {
	err := srv.mock.Put(ctx, request.Path, request.Mock, request.Mock.Spec.Metadata)
	if err != nil {
		return nil, err
	}
	return &proto.PutMockResp{Inserted: 1}, nil
}

func (srv *Server) GetMocks(ctx context.Context, request *proto.GetMockReq) (*proto.GetMockResp, error) {
	// reads the mocks from yaml file
	mocks, err := srv.mock.GetAll(ctx, request.Path, request.Name)
	if err != nil {
		return &proto.GetMockResp{}, err
	}
	res, err := grpcMock.Decode(mocks)
	if err != nil {
		srv.logger.Error(err.Error())
		return &proto.GetMockResp{}, err
	}
	response := &proto.GetMockResp{
		Mocks: res,
	}
	return response, nil
}

func (srv *Server) End(ctx context.Context, request *proto.EndRequest) (*proto.EndResponse, error) {
	stat := models.TestRunStatusFailed
	id := request.Id
	if request.Status == "true" {
		stat = models.TestRunStatusPassed
	}
	now := time.Now().Unix()

	err := srv.svc.PutTest(ctx, models.TestRun{
		ID:      id,
		Updated: now,
		Status:  stat,
	}, srv.testExport, id, "", "", srv.testReportPath, 0)
	if err != nil {
		return &proto.EndResponse{Message: err.Error()}, nil
	}
	return &proto.EndResponse{Message: "OK"}, nil
}

func (srv *Server) Start(ctx context.Context, request *proto.StartRequest) (*proto.StartResponse, error) {
	t := request.Total
	total, err := strconv.Atoi(t)
	if err != nil {
		return nil, err
	}
	app := request.App
	if app == "" {
		return nil, errors.New("app is required in request")
	}
	id := uuid.New().String()
	now := time.Now().Unix()

	err = srv.svc.PutTest(ctx, models.TestRun{
		ID:      id,
		Created: now,
		Updated: now,
		Status:  models.TestRunStatusRunning,
		CID:     graph.DEFAULT_COMPANY,
		App:     app,
		User:    graph.DEFAULT_USER,
		Total:   total,
	}, srv.testExport, id, request.TestCasePath, request.MockPath, srv.testReportPath, total)
	if err != nil {
		return nil, err
	}
	return &proto.StartResponse{Id: id}, nil
}

func getProtoMap(m map[string][]string) map[string]*proto.StrArr {
	res := map[string]*proto.StrArr{}
	for k, v := range m {
		arr := &proto.StrArr{}
		arr.Value = append(arr.Value, v...)
		res[k] = arr
	}
	return res
}
func getProtoTC(tcs models.TestCase) (*proto.TestCase, error) {
	reqHeader := getProtoMap(map[string][]string(tcs.HttpReq.Header))
	respHeader := getProtoMap(map[string][]string(tcs.HttpResp.Header))
	deps := []*proto.Dependency{}
	allKeys := getProtoMap(map[string][]string(tcs.AllKeys))
	anchors := getProtoMap(map[string][]string(tcs.Anchors))
	for _, j := range tcs.Deps {
		data := []*proto.DataBytes{}
		for _, k := range j.Data {
			data = append(data, &proto.DataBytes{
				Bin: k,
			})
		}
		deps = append(deps, &proto.Dependency{
			Name: j.Name,
			Type: string(j.Type),
			Meta: j.Meta,
			Data: data,
		})
	}
	ptcs := &proto.TestCase{
		Id:       tcs.ID,
		Created:  tcs.Created,
		Updated:  tcs.Updated,
		Captured: tcs.Captured,
		CID:      tcs.CID,
		AppID:    tcs.AppID,
		URI:      tcs.URI,
		HttpReq: &proto.HttpReq{
			Method:     string(tcs.HttpReq.Method),
			ProtoMajor: int64(tcs.HttpReq.ProtoMajor),
			ProtoMinor: int64(tcs.HttpReq.ProtoMinor),
			URL:        tcs.HttpReq.URL,
			URLParams:  tcs.HttpReq.URLParams,
			Header:     reqHeader,
			Body:       tcs.HttpReq.Body,
			Form:       grpcMock.GetProtoFormData(tcs.HttpReq.Form),
		},
		HttpResp: &proto.HttpResp{
			StatusCode:    int64(tcs.HttpResp.StatusCode),
			Header:        respHeader,
			Body:          tcs.HttpResp.Body,
			StatusMessage: tcs.HttpResp.StatusMessage,
			ProtoMajor:    int64(tcs.HttpResp.ProtoMajor),
			ProtoMinor:    int64(tcs.HttpResp.ProtoMinor),
			Binary:        tcs.HttpResp.Binary,
		},
		Deps:    deps,
		Mocks:   tcs.Mocks,
		AllKeys: allKeys,
		Anchors: anchors,
		Noise:   tcs.Noise,
	}
	return ptcs, nil
}

func (srv *Server) GetTC(ctx context.Context, request *proto.GetTCRequest) (*proto.TestCase, error) {
	id := request.Id
	app := request.App
	// print(tcs)
	tcs, err := srv.tcSvc.Get(ctx, graph.DEFAULT_COMPANY, app, id)
	if err != nil {
		return nil, err
	}
	ptcs, err := getProtoTC(tcs)
	if err != nil {
		return nil, err
	}
	return ptcs, nil
}

func (srv *Server) GetTCS(ctx context.Context, request *proto.GetTCSRequest) (*proto.GetTCSResponse, error) {
	app := request.App
	if app == "" {
		return nil, errors.New("app is required in request")
	}
	offsetStr := request.Offset
	limitStr := request.Limit
	var (
		offset int
		limit  int
		err    error
		tcs    []models.TestCase
		eof    bool = srv.testExport
	)
	if offsetStr != "" {
		offset, err = strconv.Atoi(offsetStr)
		if err != nil {
			srv.logger.Error("request for fetching testcases in converting offset to integer")
		}
	}
	if limitStr != "" {
		limit, err = strconv.Atoi(limitStr)
		if err != nil {
			srv.logger.Error("request for fetching testcases in converting limit to integer")
		}
	}

	tcs, err = srv.tcSvc.GetAll(ctx, graph.DEFAULT_COMPANY, app, &offset, &limit, request.TestCasePath, request.MockPath)
	if err != nil {
		return nil, err
	}
	var ptcs []*proto.TestCase
	for i := 0; i < len(tcs); i++ {
		ptc, err := getProtoTC(tcs[i])
		if err != nil {
			return nil, err
		}
		ptcs = append(ptcs, ptc)
	}
	return &proto.GetTCSResponse{Tcs: ptcs, Eof: eof}, nil
}

func (srv *Server) PostTC(ctx context.Context, request *proto.TestCaseReq) (*proto.PostTCResponse, error) {
	// find noisy fields
	m, err := pkg.FlattenHttpResponse(utils.GetHttpHeader(request.HttpResp.Header), request.HttpResp.Body)
	if err != nil {
		msg := "error in flattening http response"
		srv.logger.Error(msg, zap.Error(err))
		return nil, errors.New(msg)
	}
	noise := pkg.FindNoisyFields(m, func(k string, vals []string) bool {
		// check if k is date
		for _, v := range vals {
			if pkg.IsTime(v) {
				return true
			}
		}

		// maybe we need to concatenate the values
		return pkg.IsTime(strings.Join(vals, ", "))
	})

	deps := []models.Dependency{}
	for _, j := range request.Dependency {
		data := [][]byte{}
		for _, k := range j.Data {
			data = append(data, k.Bin)
		}
		deps = append(deps, models.Dependency{
			Name: j.Name,
			Type: models.DependencyType(j.Type),
			Meta: j.Meta,
			Data: data,
		})
	}
	now := time.Now().UTC().Unix()
	tc := models.TestCase{
		ID:       uuid.New().String(),
		Created:  now,
		Updated:  now,
		Captured: request.Captured,
		URI:      request.URI,
		AppID:    request.AppID,
		HttpReq: models.HttpReq{
			Method:     models.Method(request.HttpReq.Method),
			ProtoMajor: int(request.HttpReq.ProtoMajor),
			ProtoMinor: int(request.HttpReq.ProtoMinor),
			URL:        request.HttpReq.URL,
			URLParams:  request.HttpReq.URLParams,
			Body:       request.HttpReq.Body,
			Header:     utils.GetHttpHeader(request.HttpReq.Header),
			Form:       grpcMock.GetMockFormData(request.HttpReq.Form),
		},
		HttpResp: models.HttpResp{
			StatusCode:    int(request.HttpResp.StatusCode),
			Body:          request.HttpResp.Body,
			Header:        utils.GetHttpHeader(request.HttpResp.Header),
			StatusMessage: request.HttpResp.StatusMessage,
			ProtoMajor:    int(request.HttpResp.ProtoMajor),
			ProtoMinor:    int(request.HttpResp.ProtoMinor),
			Binary:        request.HttpResp.Binary,
		},
		Deps:  deps,
		Noise: noise,
		Mocks: request.Mocks,
	}}, request.TestCasePath, request.MockPath, graph.DEFAULT_COMPANY, request.Remove, request.Replace)
		Type:  request.Type,
	}
	if request.GrpcReq != nil {
		tc.GrpcReq = models.GrpcReq{
			Body:   request.GrpcReq.Body,
			Method: request.GrpcReq.Method,
		}
	}
	if request.GrpcResp != nil {
		tc.GrpcResp = models.GrpcResp{
			Body: request.GrpcResp.Body,
			Err:  request.GrpcResp.Err,
		}
	}
	inserted, err := srv.tcSvc.Insert(ctx, []models.TestCase{tc}, request.TestCasePath, request.MockPath, graph.DEFAULT_COMPANY)
	if err != nil {
		srv.logger.Error("error putting testcase", zap.Error(err))
		return nil, err
	}
	if len(inserted) == 0 {
		srv.logger.Error("unknown failure while inserting testcase")
		return nil, err
	}
	return &proto.PostTCResponse{
		TcsId: map[string]string{"id": inserted[0]},
	}, nil
}

func (srv *Server) DeNoise(ctx context.Context, request *proto.TestReq) (*proto.DeNoiseResponse, error) {
	ctx = context.WithValue(ctx, "reqType", models.Kind(request.Type))
	var body string
	switch request.Type {
	case string(models.HTTP):
		body = request.Resp.Body
	case string(models.GRPC_EXPORT):
		body = request.GrpcResp.Body
	}
	err := srv.svc.DeNoise(ctx, graph.DEFAULT_COMPANY, request.ID, request.AppID, body, utils.GetStringMap(request.Resp.Header), request.TestCasePath)
	if err != nil {
		return &proto.DeNoiseResponse{Message: err.Error()}, nil
	}
	return &proto.DeNoiseResponse{Message: "OK"}, nil
}

func (srv *Server) Test(ctx context.Context, request *proto.TestReq) (*proto.TestResponse, error) {

	ctx = context.WithValue(ctx, "reqType", models.Kind(request.Type))
	var (
		pass bool
		err  error
	)
	switch request.Type {
	case string(models.HTTP):
		pass, err = srv.svc.Test(ctx, graph.DEFAULT_COMPANY, request.AppID, request.RunID, request.ID, request.TestCasePath, request.MockPath, models.HttpResp{
			StatusCode:    int(request.Resp.StatusCode),
			Header:        utils.GetStringMap(request.Resp.Header),
			Body:          request.Resp.Body,
			StatusMessage: request.Resp.StatusMessage,
			ProtoMajor:    int(request.Resp.ProtoMajor),
			ProtoMinor:    int(request.Resp.ProtoMinor),
		})
	case string(models.GRPC_EXPORT):
		pass, err = srv.svc.TestGrpc(ctx, models.GrpcResp{
			Body: request.GrpcResp.Body,
			Err:  request.GrpcResp.Err,
		}, graph.DEFAULT_COMPANY, request.AppID, request.RunID, request.ID, request.TestCasePath, request.MockPath)
	}

	// pass, err := srv.svc.Test(ctx, graph.DEFAULT_COMPANY, request.AppID, request.RunID, request.ID, request.TestCasePath, request.MockPath, models.HttpResp{
	// 	StatusCode:    int(request.Resp.StatusCode),
	// 	Header:        utils.GetStringMap(request.Resp.Header),
	// 	Body:          request.Resp.Body,
	// 	StatusMessage: request.Resp.StatusMessage,
	// 	ProtoMajor:    int(request.Resp.ProtoMajor),
	// 	ProtoMinor:    int(request.Resp.ProtoMinor),
	// })
	if err != nil {
		return nil, err
	}
	return &proto.TestResponse{
		Pass: map[string]bool{"pass": pass},
	}, nil
}
