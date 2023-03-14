package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v2"
)

// Define a struct that matches the structure of the JSON response
type University struct {
	Name          string `json:"name"`
	Country       string `json:"country"`
	WebPage       string `json:"web_page"`
	AlphaTwo      string `json:"alpha_two_code"`
	StateProvince string `json:"state-province"`
}

func main() {
	// Set the URL of the API endpoint to record
	baseURL := "http://universities.hipolabs.com/search?country="
	countries := []string{"India", "United+States"}

	// Create an HTTP client
	client := &http.Client{}

	// Create an OpenAPI schema
	spec := &openapi3.T{
		OpenAPI: "3.0.0",
		Info: &openapi3.Info{
			Title:   "My API",
			Version: "1.0.0",
		},
		Paths: openapi3.Paths{},
	}

	sucRes := "Successful response"

	// Iterate over the list of countries
	for _, country := range countries {
		// Create an HTTP request
		url := baseURL + country
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			log.Fatal(err)
		}

		// Add headers and parameters to the request if necessary
		// ...

		// Make the API call and record the response
		resp, err := client.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		defer resp.Body.Close()

		// Read the response body
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Fatal(err)
		}

		// Save the API call to a file
		filename := fmt.Sprintf("api_call_%s.json", country)
		err = os.WriteFile(filename, body, fs.FileMode(0644))
		if err != nil {
			log.Fatal(err)
		}

		// Unmarshal the API call into a []University struct
		var universities []University
		err = json.Unmarshal(body, &universities)
		if err != nil {
			log.Fatal(err)
		}

		// Get the endpoint path for this country
		path := fmt.Sprintf("/%s", country)

		// Create an OpenAPI operation for the endpoint
		op := &openapi3.Operation{
			Description: "Get universities in " + country,
			Responses: openapi3.Responses{
				"200": &openapi3.ResponseRef{
					Value: &openapi3.Response{
						Description: &sucRes,
					},
				},
			},
		}

		// Add the operation to the OpenAPI schema
		spec.Paths[path] = &openapi3.PathItem{
			Get: op,
		}
	}

	// Write the schema to a file
	schemaFilename := "openapi_schema.yaml"
	f, err := os.Create(schemaFilename)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	data, err := yaml.Marshal(spec)
	if err != nil {
		log.Fatal(err)
	}

	_, err = f.Write(data)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("OpenAPI schema generated successfully")
}
