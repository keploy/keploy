package regression

// this will be the server file for the grpc connection

import (
	// "context"
	"net"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	
)

type server struct{}

func main() {
	listener, err := net.Listen("tcp", ":4040")
	if err != nil {
		panic(err)
	}

	srv := grpc.NewServer()
	// proto.RegisterAddServiceServer(srv, &server{})
	reflection.Register(srv)

	if e := srv.Serve(listener); e != nil {
		panic(e)
	}

}

// func (s *server) Add(ctx context.Context, request *proto.Request) (*proto.Response, error) {
// 	a, b := request.GetA(), request.GetB()

// 	result := a + b

// 	return &proto.Response{Result: result}, nil
// }

// func (s *server) Multiply(ctx context.Context, request *proto.Request) (*proto.Response, error) {
// 	a, b := request.GetA(), request.GetB()

// 	result := a * b

// 	return &proto.Response{Result: result}, nil
// }
