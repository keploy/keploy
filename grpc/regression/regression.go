package regression

// this will be the server file for the grpc connection

import (
	// "context"
	regression2 "go.keploy.io/server/pkg/service/regression"
	"go.keploy.io/server/pkg/service/run"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"net"
	proto "go.keploy.io/server/grpc/regression"
)

type server struct {
	logger *zap.Logger
	svc    regression2.Service
	run    run.Service
}

func main() {
	listener, err := net.Listen("tcp", ":4040")
	if err != nil {
		panic(err)
	}

	srv := grpc.NewServer()
	proto.RegisterAddServiceServer(srv, &server{})
	reflection.Register(srv)

	if e := srv.Serve(listener); e != nil {
		panic(e)
	}

}

// func (srv *server) End(ctx context.Context, request *proto.Request) (*proto.Response, error) {
	
// }

// func (s *server) Multiply(ctx context.Context, request *proto.Request) (*proto.Response, error) {
// 	a, b := request.GetA(), request.GetB()

// 	result := a * b

// 	return &proto.Response{Result: result}, nil
// }
