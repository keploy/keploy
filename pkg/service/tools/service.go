// Package tools provides utility functions for the service package.
package tools

import (
	"context"

	"go.keploy.io/server/v2/config"
)

type Service interface {
	Update(ctx context.Context) error
	CreateConfig(ctx context.Context, filePath string, config string) error
	Normalise(ctx context.Context, cfg *config.Config) error
}

type teleDB interface {
}
type TestCaseFile struct {
	Version string `yaml:"version"`
	Kind    string `yaml:"kind"`
	Name    string `yaml:"name"`
	Spec    struct {
		Metadata struct{} `yaml:"metadata"`
		Req      struct {
			Method     string            `yaml:"method"`
			ProtoMajor int               `yaml:"proto_major"`
			ProtoMinor int               `yaml:"proto_minor"`
			URL        string            `yaml:"url"`
			Header     map[string]string `yaml:"header"`
			Body       string            `yaml:"body"`
			Timestamp  string            `yaml:"timestamp"`
		} `yaml:"req"`
		Resp struct {
			StatusCode    int               `yaml:"status_code"`
			Header        map[string]string `yaml:"header"`
			Body          string            `yaml:"body"`
			StatusMessage string            `yaml:"status_message"`
			ProtoMajor    int               `yaml:"proto_major"`
			ProtoMinor    int               `yaml:"proto_minor"`
			Timestamp     string            `yaml:"timestamp"`
		} `yaml:"resp"`
		Objects    []interface{} `yaml:"objects"`
		Assertions struct {
			Noise map[string]interface{} `yaml:"noise"`
		} `yaml:"assertions"`
		Created int64 `yaml:"created"`
	} `yaml:"spec"`
	Curl string `yaml:"curl"`
}

func contains(list []string, item string) bool {
	for _, value := range list {
		if value == item {
			return true
		}
	}
	return false
}
