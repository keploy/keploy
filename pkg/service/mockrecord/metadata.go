package mockrecord

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// ExtractMetadata analyzes recorded mocks and returns metadata for contextual naming.
func ExtractMetadata(mocks []*models.Mock, command string) *models.MockMetadata {
	meta := &models.MockMetadata{
		ServiceName: inferServiceName(command),
		Timestamp:   time.Now(),
	}

	protocols := make([]string, 0, 4)
	seenProtocols := make(map[string]bool)
	seenEndpoints := make(map[string]bool)

	addProtocol := func(name string) {
		if name == "" || seenProtocols[name] {
			return
		}
		seenProtocols[name] = true
		protocols = append(protocols, name)
	}

	addEndpoint := func(ep models.EndpointInfo) {
		key := strings.Join([]string{ep.Protocol, ep.Host, ep.Path, ep.Method}, "|")
		if key == "|||" || seenEndpoints[key] {
			return
		}
		seenEndpoints[key] = true
		meta.Endpoints = append(meta.Endpoints, ep)
	}

	for _, mock := range mocks {
		if mock == nil {
			continue
		}

		switch mock.Kind {
		case models.HTTP:
			addProtocol("HTTP")
			if mock.Spec.HTTPReq != nil {
				parsed, err := url.Parse(mock.Spec.HTTPReq.URL)
				host := ""
				path := ""
				if err == nil {
					host = parsed.Hostname()
					if host == "" {
						host = parsed.Host
					}
					path = parsed.Path
				}
				addEndpoint(models.EndpointInfo{
					Protocol: "HTTP",
					Host:     host,
					Path:     path,
					Method:   string(mock.Spec.HTTPReq.Method),
				})
			}
		case models.GRPC_EXPORT:
			addProtocol("gRPC")
			if mock.Spec.GRPCReq != nil {
				pseudo := mock.Spec.GRPCReq.Headers.PseudoHeaders
				path := pseudo[":path"]
				method := ""
				if path != "" {
					parts := strings.Split(path, "/")
					method = parts[len(parts)-1]
				}
				addEndpoint(models.EndpointInfo{
					Protocol: "gRPC",
					Host:     pseudo[":authority"],
					Path:     path,
					Method:   method,
				})
			}
		case models.Postgres:
			addProtocol("Postgres")
		case models.MySQL:
			addProtocol("MySQL")
		case models.REDIS:
			addProtocol("Redis")
		case models.Mongo:
			addProtocol("MongoDB")
		case models.GENERIC:
			addProtocol("Generic")
		default:
			addProtocol(string(mock.Kind))
		}
	}

	meta.Protocols = protocols
	return meta
}

func inferServiceName(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return "app"
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		return "app"
	}

	if parts[0] == "sudo" {
		parts = parts[1:]
		for len(parts) > 0 && strings.HasPrefix(parts[0], "-") {
			parts = parts[1:]
		}
	}
	if len(parts) > 0 && parts[0] == "env" {
		parts = parts[1:]
		for len(parts) > 0 && strings.Contains(parts[0], "=") {
			parts = parts[1:]
		}
	}
	if len(parts) == 0 {
		return "app"
	}

	switch parts[0] {
	case "go":
		if len(parts) > 2 && parts[1] == "run" {
			return cleanServiceName(parts[2])
		}
	case "java":
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "-jar" {
				return cleanServiceName(parts[i+1])
			}
		}
	case "node", "python", "python3":
		if len(parts) > 1 {
			return cleanServiceName(parts[1])
		}
	case "npm", "yarn", "pnpm":
		if cwd, err := os.Getwd(); err == nil {
			return filepath.Base(cwd)
		}
	}

	return cleanServiceName(parts[0])
}

func cleanServiceName(raw string) string {
	name := strings.TrimSpace(strings.Trim(raw, "\"'"))
	if name == "" {
		return "app"
	}
	name = filepath.Base(name)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if name == "" {
		return "app"
	}
	return name
}
