package pkg

import (
	// "encoding/json"
	"fmt"
	"testing"

	"github.com/go-test/deep"
	"go.uber.org/zap"
)

func TestJsonDiff(t *testing.T) {
	for _, tt := range []struct {
		exp    string
		actual string
		noise  []string
		result bool
	}{
		{
			exp:    `{"name": "Ritik", "set": 21}`,
			actual: `{"name": "Ritik", "set": false}`,
			noise:  []string{"set"},
			result: true,
		},
		{
			exp: `{
				"name": "Ritik",
				"set": true,
				"contact": ["1234567890", "0987654321"]}
			`,
			actual: `{ 
				"name": "Ritik",
				"set": false,
				"contact": ["1234567890", "0987654321"]}
			`,
			noise:  []string{"contact"},
			result: false,
		},
		{
			exp: `{
				"Name": "Ritik",
				"Address": {
					"City": "Delhi",
					"Pin": 110082
				}}
			`,
			actual: `{
				"Name": "Ritik",
				"Address": {
					"City": "Delhi",
					"Pin": 110091
				}}
			`,
			noise:  []string{"Address.Pin"},
			result: true,
		},
		{
			exp: `[
				{
					"Name": "Ritik",
					"Address": {
						"City": "Delhi",
						"Pin": 110082
					}
				},
				{
					"Name": "Sarthak",
					"Address": {
						"City": "Dwarka",
						"Pin": 110021
					}
				}
			]
			`,
			actual: `[
				{
					"Name": "Ritik",
					"Address": {
						"City": "Delhi",
						"Pin": 110082
					}
				},
				{
					"Name": "Sarthak",
					"Address": {
						"City": "Delhi NCR",
						"Pin": 110012
					}
				}
			]
			`,
			noise:  []string{"Address.Pin"},
			result: false,
		},
		{
			exp: `
				{
					"Profiles": [
						{
							"Name": "Ritik",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Delhi",
								"Pin": 110082
							}	
						},
						{
							"Name": "Sarthak",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Delhi",
								"Pin": 110021
							}
						}
					]
				}
			`,
			actual: `
				{
					"Profiles": [
						{
							"Name": "Ritik",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Delhi",
								"Pin": 110082
							}
						},
						{
							"Name": "Sarthak",
							"Contact": ["123", "456"], 
							"Address": {
								"City": "Delhi",
								"Pin": 110081
							}
						}
					]
				}
			`,
			noise:  []string{"set.somethingNotPresent", "Profiles.Address.Pin"},
			result: true,
		},
		{
			exp: `
				[
					{
						"name":    "Sarthak",
						"age":     21.0,
						"set":     false,
						"contact": [
							"Student", {"address": "Dwarka"}
						]
					},
					{
						"name":    "Ritik",
						"age":     21.0,
						"set":     false,
						"contact": [
							"Student", {"address": "Laxmi Nagar"}
						]
					}
				]
			`,
			actual: `
				[
					{
						"name":    "Sarthak",
						"age":     21.0,
						"set":     true,
						"contact": [
							"Student", {"address": "Delhi"}
						]
					},
					{
						"name":    "Ritik",
						"age":     21.0,
						"set":     false,
						"contact": [
							"Student", {"address": "Delhi"}
						]
					}
				]
			`,
			noise:  []string{"set", "contact.address"},
			result: true,
		},
		{
			exp: `
			{
				"Name": "Ritik",
				"Age": 21.0,
				"Address": {
					"City" : "Delhi",
					"PIN" : "110192"
				},
				"Father": {
					"Name": "Atul Jain",
					"Age": 45.0,
					"Address": {
						"City" : "Delhi",
						"PIN" : "110192"	
					}
				}
			}
			`,
			actual: `
			{
				"Name": "Ritik",
				"Age": 21.0,
				"Address": {
					"City" : "Delhi",
					"PIN" : "110131"
				},
				"Father": {
					"Name": "Atul Jain",
					"Age": 45.0,
					"Address": {
						"City" : "Delhi",
						"PIN" : "110192"	
					}
				}
			}
			`,
			noise:  []string{"Father.Age", "Father.Address.PIN"},
			result: false,
		},
	} {
		logger, _ := zap.NewProduction()
		defer logger.Sync()
		res, err := Match(tt.exp, tt.actual, tt.noise, logger)
		if err != nil {
			logger.Error("%v", zap.Error(err))
		}
		diff := deep.Equal(res, tt.result)
		if diff != nil {
			fmt.Println("This is diff", diff)
			t.Fatal(tt.exp, tt.actual, "THIS IS EXP", tt.result, " \n THIS IS ACT", res)
		}
	}

}
