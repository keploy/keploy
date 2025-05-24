module keploy_grpc_demo_files/demo_grpc_service/client

go 1.24.1

toolchain go1.24.3

require (
	google.golang.org/grpc v1.72.1
	keploy_grpc_demo_files/demo_grpc_service v0.0.0
)

require (
	golang.org/x/net v0.35.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.22.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250218202821-56aae31c358a // indirect
	google.golang.org/protobuf v1.36.5 // indirect
)

replace keploy_grpc_demo_files/demo_grpc_service => ../
