package lang

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

type Job struct {
	method      string
	description string
	url         string
	headers     map[string]cty.Value
	body        map[string]cty.Value
}

func (j *Job) run(ctx *hcl.EvalContext) error {
	fmt.Printf("----------\njob: %s, %s\n\n", j.method, j.description)
	fmt.Println("url: ", j.url)
	// id, _ := j.body["id"].AsBigFloat().Int64()
	// name := j.body["name"].AsString()
	// tp := j.body["name"].Type()
	// fmt.Println("body: ", id, name, tp.FriendlyName())
	switch j.method {
	case "get":
		return j.getRequest(ctx)
	case "post":
		return j.postRequest(ctx)
	default:
		return fmt.Errorf("unknown method: %s", j.method)
	}

}

func (j *Job) getRequest(ctx *hcl.EvalContext) error {

	resp, err := http.Get(j.url)
	if err != nil {
		return err
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		log.Println("status code: ", resp.StatusCode)
	} else {

		fmt.Println("get request to ", j.url, " success!")
	}

	return nil
}

func (j *Job) postRequest(ctx *hcl.EvalContext) error {

	dataStr := j.body["payload"].AsString()

	dataStr = strings.ReplaceAll(dataStr, "", "")
	fmt.Println("dataStr: ", dataStr)

	data, err := json.Marshal(dataStr)

	if err != nil {
		return err
	}

	resp, err := http.Post(j.url, j.headers["Content-Type"].AsString(), bytes.NewBuffer(data))

	if err != nil {
		return err
	}

	if resp.Body != nil {
		defer resp.Body.Close()
	}
	rspBy, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Println("status code: ", resp.StatusCode)
	} else {
		fmt.Println("post request to ", j.url, " success!")
		fmt.Println("response: ", string(rspBy))
	}

	return nil
}

type Pipeline struct {
	jobs []Job
	vars map[string]cty.Value
}

func (p *Pipeline) Run() {
	ctx := &hcl.EvalContext{
		Functions: map[string]function.Function{},
		Variables: p.vars,
	}
	for _, job := range p.jobs {
		job.run(ctx)
	}
}
