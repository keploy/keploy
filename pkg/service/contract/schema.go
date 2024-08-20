package contract

import (
	"context"
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"
	"go.keploy.io/server/v2/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

func validateSchema(openapi models.OpenAPI) error {
	openapiYAML, err := yamlLib.Marshal(openapi)
	if err != nil {
		return err
	}
	// Validate using kin-openapi
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openapiYAML)
	if err != nil {
		return err

	}
	// Validate the OpenAPI document
	if err := doc.Validate(context.Background()); err != nil {
		return err
	}

	fmt.Println("OpenAPI document is valid.")
	return nil
}
