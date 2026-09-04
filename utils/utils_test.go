package utils

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestContainerNameFromDockerRun(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"space form", "docker run --rm --name dedup-go-test dedup-go:latest", "dedup-go-test"},
		{"equals form", "docker run --name=my-app img", "my-app"},
		{"name mid-flags", "docker run -d --name x --network y img", "x"},
		{"no name", "docker run --rm img", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainerNameFromDockerRun(tc.cmd); got != tc.want {
				t.Fatalf("ContainerNameFromDockerRun(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestIsShutdownError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "io.EOF error",
			err:  io.EOF,
			want: true,
		},
		{
			name: "io.ErrUnexpectedEOF error",
			err:  io.ErrUnexpectedEOF,
			want: true,
		},
		{
			name: "wrapped io.EOF error",
			err:  errors.New("read tcp 127.0.0.1:8080: EOF"),
			want: true,
		},
		{
			name: "connection refused error",
			err:  errors.New("dial tcp 127.0.0.1:8080: connect: connection refused"),
			want: true,
		},
		{
			name: "connection reset error",
			err:  errors.New("read tcp 127.0.0.1:8080: connection reset by peer"),
			want: true,
		},
		{
			name: "broken pipe error",
			err:  syscall.EPIPE,
			want: true,
		},
		{
			name: "use of closed network connection error",
			err:  errors.New("use of closed network connection"),
			want: true,
		},
		{
			name: "unrelated error",
			err:  errors.New("file not found"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsShutdownError(tt.err)
			if got != tt.want {
				t.Errorf("IsShutdownError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestReplaceHost(t *testing.T) {
	tests := []struct {
		name       string
		currentURL string
		ipAddress  string
		want       string
		wantErr    bool
	}{
		{
			name:       "valid http URL with hostname replaced by IP",
			currentURL: "http://example.com/api/v1",
			ipAddress:  "192.168.1.100",
			want:       "http://192.168.1.100/api/v1",
			wantErr:    false,
		},
		{
			name:       "valid http URL with host and port",
			currentURL: "http://example.com:8080/api/v1",
			ipAddress:  "192.168.1.100",
			want:       "http://192.168.1.100:8080/api/v1",
			wantErr:    false,
		},
		{
			name:       "empty IP address returns error",
			currentURL: "http://example.com/api/v1",
			ipAddress:  "",
			want:       "http://example.com/api/v1",
			wantErr:    true,
		},
		{
			name:       "invalid URL format returns error",
			currentURL: "http:// invalid url",
			ipAddress:  "192.168.1.100",
			want:       "http:// invalid url",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReplaceHost(tt.currentURL, tt.ipAddress)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReplaceHost() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ReplaceHost() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReplaceGrpcHost(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		ipAddress string
		want      string
		wantErr   bool
	}{
		{
			name:      "valid authority and ip",
			authority: "localhost:50051",
			ipAddress: "127.0.0.1",
			want:      "127.0.0.1:50051",
			wantErr:   false,
		},
		{
			name:      "empty IP address returns error",
			authority: "localhost:50051",
			ipAddress: "",
			want:      "localhost:50051",
			wantErr:   true,
		},
		{
			name:      "invalid authority without port returns error",
			authority: "localhost",
			ipAddress: "127.0.0.1",
			want:      "localhost",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReplaceGrpcHost(tt.authority, tt.ipAddress)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReplaceGrpcHost() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ReplaceGrpcHost() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReplaceGrpcPort(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		port      string
		want      string
		wantErr   bool
	}{
		{
			name:      "valid authority with port",
			authority: "localhost:50051",
			port:      "8080",
			want:      "localhost:8080",
			wantErr:   false,
		},
		{
			name:      "authority without port appends port",
			authority: "localhost",
			port:      "8080",
			want:      "localhost:8080",
			wantErr:   false,
		},
		{
			name:      "empty port returns error",
			authority: "localhost:50051",
			port:      "",
			want:      "localhost:50051",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReplaceGrpcPort(tt.authority, tt.port)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReplaceGrpcPort() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ReplaceGrpcPort() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReplaceBaseURL(t *testing.T) {
	tests := []struct {
		name       string
		currentURL string
		baseURL    string
		want       string
		wantErr    bool
	}{
		{
			name:       "valid replacement",
			currentURL: "http://old-host.com/api/v1/resource?query=1",
			baseURL:    "https://new-host.com:9000",
			want:       "https://new-host.com:9000/api/v1/resource?query=1",
			wantErr:    false,
		},
		{
			name:       "empty baseURL returns error",
			currentURL: "http://old-host.com/api/v1",
			baseURL:    "",
			want:       "http://old-host.com/api/v1",
			wantErr:    true,
		},
		{
			name:       "invalid currentURL returns error",
			currentURL: "http:// invalid url",
			baseURL:    "https://new-host.com",
			want:       "http:// invalid url",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReplaceBaseURL(tt.currentURL, tt.baseURL)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReplaceBaseURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ReplaceBaseURL() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReplacePort(t *testing.T) {
	tests := []struct {
		name       string
		currentURL string
		port       string
		want       string
		wantErr    bool
	}{
		{
			name:       "URL with existing port",
			currentURL: "http://localhost:8080/test",
			port:       "9090",
			want:       "http://localhost:9090/test",
			wantErr:    false,
		},
		{
			name:       "URL without existing port",
			currentURL: "http://localhost/test",
			port:       "9090",
			want:       "http://localhost:9090/test",
			wantErr:    false,
		},
		{
			name:       "empty port returns error",
			currentURL: "http://localhost/test",
			port:       "",
			want:       "http://localhost/test",
			wantErr:    true,
		},
		{
			name:       "invalid URL returns error",
			currentURL: "http:// invalid url",
			port:       "9090",
			want:       "http:// invalid url",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReplacePort(tt.currentURL, tt.port)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReplacePort() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ReplacePort() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetReqMeta(t *testing.T) {
	u, _ := url.Parse("http://example.com/test?param=1")
	req := &http.Request{
		Method: "GET",
		URL:    u,
		Host:   "example.com",
	}

	tests := []struct {
		name string
		req  *http.Request
		want map[string]string
	}{
		{
			name: "valid request",
			req:  req,
			want: map[string]string{
				"method": "GET",
				"url":    "http://example.com/test?param=1",
				"host":   "example.com",
			},
		},
		{
			name: "nil request",
			req:  nil,
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetReqMeta(tt.req)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetReqMeta() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKebabToCamel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single word",
			input: "config",
			want:  "config",
		},
		{
			name:  "two kebab words",
			input: "cmd-type",
			want:  "cmdType",
		},
		{
			name:  "multiple kebab words",
			input: "my-custom-flag-name",
			want:  "myCustomFlagName",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kebabToCamel(tt.input)
			if got != tt.want {
				t.Errorf("kebabToCamel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRemoveDoubleQuotes(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]interface{}
		want  map[string]interface{}
	}{
		{
			name: "removes double quotes from strings",
			input: map[string]interface{}{
				"header": `"Not/A)Brand";v="8", "Chromium";v="126"`,
				"num":    42,
			},
			want: map[string]interface{}{
				"header": `Not/A)Brand;v=8, Chromium;v=126`,
				"num":    42,
			},
		},
		{
			name:  "empty map",
			input: map[string]interface{}{},
			want:  map[string]interface{}{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			RemoveDoubleQuotes(tt.input)
			if !reflect.DeepEqual(tt.input, tt.want) {
				t.Errorf("RemoveDoubleQuotes() = %v, want %v", tt.input, tt.want)
			}
		})
	}
}

func TestFindDockerCmd(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want CmdType
	}{
		{name: "empty string", cmd: "", want: Empty},
		{name: "docker run", cmd: "docker run -d -p 80:80 nginx", want: DockerRun},
		{name: "sudo docker run", cmd: "sudo docker run -d nginx", want: DockerRun},
		{name: "docker container run", cmd: "docker container run alpine", want: DockerRun},
		{name: "sudo docker container run", cmd: "sudo docker container run alpine", want: DockerRun},
		{name: "docker start", cmd: "docker start my-container", want: DockerStart},
		{name: "sudo docker start", cmd: "sudo docker start my-container", want: DockerStart},
		{name: "docker container start", cmd: "docker container start my-container", want: DockerStart},
		{name: "sudo docker container start", cmd: "sudo docker container start my-container", want: DockerStart},
		{name: "docker compose", cmd: "docker compose up", want: DockerCompose},
		{name: "sudo docker compose", cmd: "sudo docker compose up", want: DockerCompose},
		{name: "docker-compose", cmd: "docker-compose up -d", want: DockerCompose},
		{name: "sudo docker-compose", cmd: "sudo docker-compose down", want: DockerCompose},
		{name: "native go command", cmd: "go run main.go", want: Native},
		{name: "native python command", cmd: "python3 app.py", want: Native},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindDockerCmd(tt.cmd)
			if got != tt.want {
				t.Errorf("FindDockerCmd(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  int
	}{
		{name: "int", input: 42, want: 42},
		{name: "int64", input: int64(100), want: 100},
		{name: "int32", input: int32(50), want: 50},
		{name: "float32", input: float32(3.14), want: 3},
		{name: "float64", input: float64(9.99), want: 9},
		{name: "valid string", input: "123", want: 123},
		{name: "invalid string", input: "abc", want: 0},
		{name: "json.Number int", input: json.Number("456"), want: 456},
		{name: "json.Number float", input: json.Number("789.5"), want: 789},
		{name: "unsupported type", input: true, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToInt(tt.input)
			if got != tt.want {
				t.Errorf("ToInt(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestToString(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  string
	}{
		{name: "int", input: 42, want: "42"},
		{name: "int64", input: int64(100), want: "100"},
		{name: "int32", input: int32(50), want: "50"},
		{name: "float64", input: 3.1415, want: "3.1415"},
		{name: "float32", input: float32(2.5), want: "2.5"},
		{name: "string", input: "hello", want: "hello"},
		{name: "unsupported type", input: true, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToString(tt.input)
			if got != tt.want {
				t.Errorf("ToString(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestToFloat(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  float64
	}{
		{name: "float64", input: 3.14, want: 3.14},
		{name: "int", input: 42, want: 42.0},
		{name: "valid string float", input: "12.34", want: 12.34},
		{name: "invalid string", input: "xyz", want: 0},
		{name: "unsupported type", input: true, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToFloat(tt.input)
			if got != tt.want {
				t.Errorf("ToFloat(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestKeys(t *testing.T) {
	tests := []struct {
		name  string
		input map[string][]string
		want  []string
	}{
		{
			name: "multiple keys",
			input: map[string][]string{
				"alpha": {"1"},
				"beta":  {"2"},
				"gamma": {"3"},
			},
			want: []string{"alpha", "beta", "gamma"},
		},
		{
			name:  "empty map",
			input: map[string][]string{},
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Keys(tt.input)
			sort.Strings(got)
			sort.Strings(tt.want)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Keys() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsureRmBeforeName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "inserts --rm before --name when missing",
			input: "docker run --name my-app img",
			want:  "docker run --rm --name my-app img",
		},
		{
			name:  "keeps unchanged when --rm already present before --name",
			input: "docker run --rm --name my-app img",
			want:  "docker run --rm --name my-app img",
		},
		{
			name:  "keeps unchanged when --name not present",
			input: "docker run img",
			want:  "docker run img",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EnsureRmBeforeName(tt.input)
			if got != tt.want {
				t.Errorf("EnsureRmBeforeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsDockerCmd(t *testing.T) {
	tests := []struct {
		name string
		cmd  CmdType
		want bool
	}{
		{name: "DockerRun", cmd: DockerRun, want: true},
		{name: "DockerStart", cmd: DockerStart, want: true},
		{name: "DockerCompose", cmd: DockerCompose, want: true},
		{name: "Native", cmd: Native, want: false},
		{name: "Empty", cmd: Empty, want: false},
		{name: "Other string", cmd: CmdType("custom"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsDockerCmd(tt.cmd)
			if got != tt.want {
				t.Errorf("IsDockerCmd(%v) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestHash(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{
			name:  "empty bytes",
			input: []byte(""),
			want:  "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:  "simple string keploy",
			input: []byte("keploy"),
			want:  "787009cfb43f5e53579a025af080167b9b32519140d8b42c48d32cee7a9baf3a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Hash(tt.input)
			if got != tt.want {
				t.Errorf("Hash(%q) = %v, want %v", string(tt.input), got, tt.want)
			}
		})
	}
}

func TestIsXMLResponse(t *testing.T) {
	tests := []struct {
		name string
		resp *models.HTTPResp
		want bool
	}{
		{
			name: "nil resp",
			resp: nil,
			want: false,
		},
		{
			name: "nil header",
			resp: &models.HTTPResp{Header: nil},
			want: false,
		},
		{
			name: "application/xml Content-Type",
			resp: &models.HTTPResp{
				Header: map[string]string{
					"Content-Type": "application/xml; charset=utf-8",
				},
			},
			want: true,
		},
		{
			name: "text/xml Content-Type",
			resp: &models.HTTPResp{
				Header: map[string]string{
					"Content-Type": "text/xml",
				},
			},
			want: true,
		},
		{
			name: "application/json Content-Type",
			resp: &models.HTTPResp{
				Header: map[string]string{
					"Content-Type": "application/json",
				},
			},
			want: false,
		},
		{
			name: "missing Content-Type",
			resp: &models.HTTPResp{
				Header: map[string]string{
					"Accept": "text/plain",
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsXMLResponse(tt.resp)
			if got != tt.want {
				t.Errorf("IsXMLResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTrimSpaces(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "spaces around equals and comma",
			input: "a = b , c = d",
			want:  "a=b,c=d",
		},
		{
			name:  "escaped separator preserved",
			input: `a\,b = c`,
			want:  `a\,b=c`,
		},
		{
			name:  "no separators",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimSpaces(tt.input)
			if got != tt.want {
				t.Errorf("TrimSpaces(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseMetadata(t *testing.T) {
	tests := []struct {
		name        string
		metadataStr string
		want        map[string]interface{}
		wantErr     bool
	}{
		{
			name:        "empty string",
			metadataStr: "",
			want:        nil,
			wantErr:     false,
		},
		{
			name:        "valid metadata string",
			metadataStr: "key1=val1,key2=val2",
			want: map[string]interface{}{
				"key1": "val1",
				"key2": "val2",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMetadata(tt.metadataStr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseMetadata() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseMetadata() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNetworkToHostShort(t *testing.T) {
	tests := []struct {
		name  string
		input uint16
		want  uint16
	}{
		{
			name:  "zero",
			input: 0x0000,
			want:  0x0000,
		},
		{
			name:  "standard port 80 (0x0050)",
			input: 0x0050,
			want:  0x5000,
		},
		{
			name:  "symmetric value 0x1212",
			input: 0x1212,
			want:  0x1212,
		},
		{
			name:  "asymmetric value 0x1234",
			input: 0x1234,
			want:  0x3412,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NetworkToHostShort(tt.input)
			if got != tt.want {
				t.Errorf("NetworkToHostShort(0x%04x) = 0x%04x, want 0x%04x", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseGRPCPath(t *testing.T) {
	tests := []struct {
		name            string
		path            string
		wantServiceFull string
		wantMethod      string
		wantErr         bool
	}{
		{
			name:            "valid gRPC path with leading slash",
			path:            "/package.Service/Method",
			wantServiceFull: "package.Service",
			wantMethod:      "Method",
			wantErr:         false,
		},
		{
			name:            "valid gRPC path with nested package",
			path:            "/com.example.grpc.Greeter/SayHello",
			wantServiceFull: "com.example.grpc.Greeter",
			wantMethod:      "SayHello",
			wantErr:         false,
		},
		{
			name:            "empty path",
			path:            "",
			wantServiceFull: "",
			wantMethod:      "",
			wantErr:         true,
		},
		{
			name:            "invalid segments count",
			path:            "/package.Service/Method/Extra",
			wantServiceFull: "",
			wantMethod:      "",
			wantErr:         true,
		},
		{
			name:            "missing service name",
			path:            "//Method",
			wantServiceFull: "",
			wantMethod:      "",
			wantErr:         true,
		},
		{
			name:            "missing method name",
			path:            "/package.Service/",
			wantServiceFull: "",
			wantMethod:      "",
			wantErr:         true,
		},
		{
			name:            "service name without dot",
			path:            "/Service/Method",
			wantServiceFull: "",
			wantMethod:      "",
			wantErr:         true,
		},
		{
			name:            "service name starting with dot",
			path:            "/.package.Service/Method",
			wantServiceFull: "",
			wantMethod:      "",
			wantErr:         true,
		},
		{
			name:            "service name ending with dot",
			path:            "/package.Service./Method",
			wantServiceFull: "",
			wantMethod:      "",
			wantErr:         true,
		},
		{
			name:            "service name with consecutive dots",
			path:            "/package..Service/Method",
			wantServiceFull: "",
			wantMethod:      "",
			wantErr:         true,
		},
		{
			name:            "invalid identifier with special char",
			path:            "/package.Service/Method-Name",
			wantServiceFull: "",
			wantMethod:      "",
			wantErr:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotService, gotMethod, err := ParseGRPCPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseGRPCPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
				return
			}
			if gotService != tt.wantServiceFull || gotMethod != tt.wantMethod {
				t.Errorf("ParseGRPCPath(%q) = (%q, %q), want (%q, %q)", tt.path, gotService, gotMethod, tt.wantServiceFull, tt.wantMethod)
			}
		})
	}
}

func TestIsValidGRPCIdentifier(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "empty string", input: "", want: false},
		{name: "valid camelCase", input: "SayHello", want: true},
		{name: "valid with underscore", input: "_privateMethod_1", want: true},
		{name: "invalid starting with digit", input: "1Method", want: false},
		{name: "invalid containing hyphen", input: "method-name", want: false},
		{name: "invalid containing dot", input: "method.name", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidGRPCIdentifier(tt.input)
			if got != tt.want {
				t.Errorf("isValidGRPCIdentifier(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}


func TestExtractIDFromStatusLine(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{
			name:  "valid status line",
			input: "NSpgid:\t12345",
			want:  12345,
		},
		{
			name:  "space separated status line",
			input: "Pid: 6789",
			want:  6789,
		},
		{
			name:  "invalid value in status line",
			input: "NSpgid: invalid",
			want:  -1,
		},
		{
			name:  "empty line",
			input: "",
			want:  -1,
		},
		{
			name:  "too many fields",
			input: "Field: 1 2 3",
			want:  -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractIDFromStatusLine(tt.input)
			if got != tt.want {
				t.Errorf("extractIDFromStatusLine(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestRenderTemplatesInString(t *testing.T) {
	logger := zap.NewNop()
	data := map[string]interface{}{
		"name": "Keploy",
		"port": 8080,
		"rate": 1.25,
	}

	tests := []struct {
		name     string
		input    string
		data     map[string]interface{}
		want     string
		wantErr  bool
	}{
		{
			name:    "simple template substitution",
			input:   "Hello {{.name}}",
			data:    data,
			want:    "Hello Keploy",
			wantErr: false,
		},
		{
			name:    "typed int substitution",
			input:   "Port: {{int .port}}",
			data:    data,
			want:    "Port: 8080",
			wantErr: false,
		},
		{
			name:    "non-template curly braces ignored",
			input:   "Formula: {{u^2}}",
			data:    data,
			want:    "Formula: {{u^2}}",
			wantErr: false,
		},
		{
			name:    "missing template field renders placeholder output with json escaping",
			input:   "Missing: {{.nonexistent}}",
			data:    data,
			want:    `Missing: \u003cno value\u003e`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderTemplatesInString(logger, tt.input, tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("RenderTemplatesInString() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("RenderTemplatesInString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
