# Keploy gRPC Demo Application

This directory contains a sample gRPC application (a Greeter service and client) to demonstrate Keploy's capabilities for recording and testing gRPC interactions.

## Directory Structure

```
keploy_grpc_demo_files/
├── demo_grpc_service/
│   ├── proto/
│   │   ├── greeter.proto         # Protocol buffer definition for the Greeter service
│   │   ├── greeter.pb.go         # Generated Go code for messages
│   │   └── greeter_grpc.pb.go    # Generated Go code for gRPC client and server
│   ├── client/
│   │   ├── main.go             # gRPC client application
│   │   └── go.mod              # Client Go module file
│   │   └── go.sum              # Client Go sum file
│   ├── main.go                 # gRPC server application
│   ├── go.mod                  # Server Go module file
│   └── go.sum                  # Server Go sum file
├── .gitignore                  # Git ignore file
└── README.md                   # This README file
```

## Prerequisites

- Go (version 1.20 or higher recommended)
- Protocol Buffer Compiler (`protoc`)
- Go plugins for `protoc`:
  - `protoc-gen-go`
  - `protoc-gen-go-grpc`
- Keploy binary

If you don't have `protoc` and the Go plugins, install them:
```bash
# For protoc (example for Debian/Ubuntu)
sudo apt update && sudo apt install -y protobuf-compiler

# For Go plugins
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Ensure your GOPATH/bin is in your PATH
export PATH="$PATH:$(go env GOPATH)/bin"
```

The generated proto files (`greeter.pb.go` and `greeter_grpc.pb.go`) are already included in the `demo_grpc_service/proto/` directory. If you modify `greeter.proto`, you'll need to regenerate them:
```bash
cd demo_grpc_service/proto
protoc --go_out=. --go_opt=paths=source_relative \
    --go-grpc_out=. --go-grpc_opt=paths=source_relative \
    greeter.proto
cd ../.. # Back to keploy_grpc_demo_files
```

## Running the Demo

### 1. Start the gRPC Service

Open a terminal and navigate to the `demo_grpc_service` directory:
```bash
cd demo_grpc_service
go run main.go
```
The server will start and listen on `localhost:50051`. You should see a log message like: `server listening at [::]:50051`.

### 2. Run the gRPC Client

Open another terminal and navigate to the `demo_grpc_service/client` directory:
```bash
cd demo_grpc_service/client
go run main.go
```
The client will send a request to the server and print the response:
`Greeting: Hello Keploy User`

You can also pass a name as a command-line argument:
```bash
go run main.go "Your Name"
```
Output: `Greeting: Hello Your Name`

## Using Keploy with the gRPC Demo

### 1. Record gRPC Interactions

Keploy can record the gRPC calls made between the client and the server.

Navigate to the root of this demo directory (`keploy_grpc_demo_files`). The command below assumes Keploy will manage running the server.

```bash
# Ensure you are in the keploy_grpc_demo_files directory
# If your demo_grpc_service/main.go is in the current directory, the command would be:
# keploy record -c "go run main.go" --traffic-type="grpc"
# Since main.go is in demo_grpc_service, adjust the command:

keploy record -c "go run demo_grpc_service/main.go" --traffic-type="grpc"
```

Keploy will start, instrument your gRPC server, and wait for incoming calls. Now, in the other terminal (where you ran the client), run the client again:
```bash
# In the client's terminal (demo_grpc_service/client directory)
go run main.go "Test User"
```
You should see the client output. Keploy will capture this interaction. You can run the client multiple times with different names if you wish.

Once done, stop Keploy (usually Ctrl+C in the Keploy terminal).

### 2. View Recorded Test Cases and Mocks

Keploy stores the recorded interactions in a `keploy` directory (created in the directory where Keploy was run, i.e., `keploy_grpc_demo_files`).
-   **Test Cases:** `keploy/tests/test-set-0/` (or similar) will contain YAML files for each recorded gRPC call (e.g., `test-1.yaml`).
-   **Mocks:** If your service made any outgoing calls (none in this simple demo), they would be recorded in `keploy/mocks/test-set-0/`.

### 3. Test (Replay) Recorded Interactions

Now, you can run Keploy in test mode. Keploy will replay the recorded requests against your server and compare the responses with the recorded ones.

Ensure your gRPC server is **not** running manually. Keploy will run it using the command you provide.

Navigate to the root of this demo directory (`keploy_grpc_demo_files`).

```bash
keploy test -c "go run demo_grpc_service/main.go" --traffic-type="grpc" --delay 10
```

-   `-c "go run demo_grpc_service/main.go"`: This is the command Keploy uses to start your gRPC server.
-   `--traffic-type="grpc"`: Specifies that Keploy should expect gRPC traffic.
-   `--delay 10`: This adds a 10-second delay after starting your application before Keploy sends test requests. This can be useful to ensure your server has enough time to initialize fully.

Keploy will run the tests and show you a summary of the results (pass/fail). You can see detailed test reports in `keploy/reports/`.

This completes the demo of using Keploy with a gRPC application!
```
