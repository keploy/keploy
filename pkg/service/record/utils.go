package record

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

func ReadTestCase(filepath string, fileName os.DirEntry) (models.TestCase, error) {

    filePath := fileName.Name() 
	absPath := path.Join(filepath, filePath) 

    // Read file content
    testCaseContent, err := os.ReadFile(absPath) 
    if err != nil {

        return models.TestCase{}, err
    }


    var testCase models.TestCase

    err = yaml.Unmarshal(testCaseContent, &testCase)
    if err != nil {

        return models.TestCase{}, err
    }

    return testCase, nil
}
func extractHostAndPort(curlCmd string) (string, string, error) {
    // Split the command string to find the URL
    parts := strings.Split(curlCmd, " ")
    for _, part := range parts {
        if strings.HasPrefix(part, "http") {
            u, err := url.Parse(part)
            if err != nil {
                return "", "", err
            }
            host := u.Hostname()
            port := u.Port()
            if port == "" {
                if u.Scheme == "https" {
                    port = "443"
                } else {
                    port = "80"
                }
            }
            return host, port, nil
        }
    }
    return "", "", fmt.Errorf("no URL found in CURL command")
}

func waitForPort(ctx context.Context, host, port string) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 1*time.Second)
            if err == nil {
                conn.Close()
                return nil 
            }
            time.Sleep(1 * time.Second)
        }
    }
}
