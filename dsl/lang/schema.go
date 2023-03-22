package lang

import "github.com/hashicorp/hcl/v2"

const requestId = "request"

var (
	requestLabel  = []string{"method", "description"}
	requestSchema = &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{},
		Attributes: []hcl.AttributeSchema{
			{Name: "url", Required: true},
			{Name: "headers", Required: false},
			{Name: "body", Required: false},
			{Name: "status", Required: false},
		},
	}
)

var schema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{},
	Blocks: []hcl.BlockHeaderSchema{
		{
			Type:       requestId,
			LabelNames: requestLabel,
		},
	},
}
