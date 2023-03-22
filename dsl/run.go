package dsl

import (
	"io/ioutil"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"go.keploy.io/server/dsl/lang"
)

// inputPath is the path to the hcl file
// pass the path of test.hcl to this function to see the result
func Run(inputPath string) {

	pipeline, err := loadInputfile(inputPath)
	if err != nil {
		println(err.Error())
		os.Exit(1)
	}
	pipeline.Run()
}

func loadInputfile(path string) (*lang.Pipeline, error) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	file, diagnostics := hclsyntax.ParseConfig(content, path,
		hcl.Pos{Line: 1, Column: 1, Byte: 0})
	if diagnostics != nil && diagnostics.HasErrors() {
		return nil, diagnostics.Errs()[0]
	}

	out, decodeErr := lang.Decode(file.Body)
	if decodeErr != nil {
		return nil, decodeErr
	}

	return out, nil
}
