package lang

import (
	"errors"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

func (j *Job) FromHCLBlock(block *hcl.Block, ctx *hcl.EvalContext) error {

	bc, diags := block.Body.Content(requestSchema)

	if diags.HasErrors() {
		return diags.Errs()[0]
	}

	j.method = block.Labels[0]
	j.description = block.Labels[1]

	if attr, ok := bc.Attributes["url"]; ok {
		url, d := attr.Expr.Value(ctx)
		if d.HasErrors() {
			return d.Errs()[0]
		}
		j.url = url.AsString()
	}

	if attr, ok := bc.Attributes["headers"]; ok {
		headers, d := attr.Expr.Value(ctx)
		if d.HasErrors() {
			return d.Errs()[0]
		}
		j.headers = headers.AsValueMap()
	}

	if attr, ok := bc.Attributes["body"]; ok {
		body, d := attr.Expr.Value(ctx)
		if d.HasErrors() {
			return d.Errs()[0]
		}
		j.body = body.AsValueMap()
	}

	// if attr, ok := bc.Attributes["status"]; ok {
	// 	status, d := attr.Expr.Value(ctx)
	// 	if d.HasErrors() {
	// 		return d.Errs()[0]
	// 	}
	// 	val, _ := status.AsBigFloat().Int64()
	// 	j.status = int(val)
	// }

	return nil
}

func Decode(body hcl.Body) (*Pipeline, error) {
	ctx := &hcl.EvalContext{
		Functions: map[string]function.Function{},
	}
	pipeline := &Pipeline{
		jobs: make([]Job, 0),
		vars: make(map[string]cty.Value),
	}

	attributes, _ := body.JustAttributes()
	for name, value := range attributes {
		v, d := value.Expr.Value(ctx)
		if d.HasErrors() {
			fmt.Println(d.Errs()[0])
			continue
		}
		pipeline.vars[name] = v
	}

	ctx.Variables = pipeline.vars
	bc, _ := body.Content(schema)

	if len(bc.Blocks) == 0 {
		return nil, errors.New("at least one pipeline must be provided")
	}
	blocks := bc.Blocks.ByType()

	for blockName := range blocks {
		switch blockName {
		case requestId:
			for _, b := range blocks[blockName] {
				job := new(Job)
				err := job.FromHCLBlock(b, ctx)
				if err != nil {
					return nil, err
				}
				pipeline.jobs = append(pipeline.jobs, *job)
			}
		}
	}

	return pipeline, nil
}
