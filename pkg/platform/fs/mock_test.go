package fs

import (
	"context"
	"go.keploy.io/server/pkg/models"
	"gopkg.in/yaml.v3"
	"log"
	"sync"
	"testing"
)

func TestMock(t *testing.T) {
	mE := mockExport{
		isTestMode: false,
		tests:      sync.Map{},
	}
	err := mE.Write(context.Background(), ".", models.Mock{
		Version: "3",
		Kind:    "http",
		Name:    "mymock",
		Spec: yaml.Node{
			Kind:        0,
			Style:       0,
			Tag:         "",
			Value:       "",
			Anchor:      "",
			Alias:       nil,
			Content:     nil,
			HeadComment: "",
			LineComment: "",
			FootComment: "",
			Line:        0,
			Column:      0,
		},
	})
	if err != nil {
		log.Fatal(err)
	}

}
