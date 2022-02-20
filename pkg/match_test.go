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
			exp:    `{
				"data": {
					"url":"http://localhost:8080/GMWJGSAP",
					"body": "lorem ipsum jibrish"
				},
				"status":200
			}`,
			actual: `{
				"data": {
					"body": "lorem ipsum jibrish",
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"status":200
			}`,
			noise:  []string{"data.url"},
			result: true,
		},
		{
			exp:    `{
				"data": {
					"url":"http://localhost:8080/GMWJGSAP",
					"body": "paorum "
				},
				"status":200
			}`,
			actual: `{
				"data": {
					"body": "lorem ipsum jibrish",
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"status":200
			}`,
			noise:  []string{"data.url"},
			result: false,
		},
		{
			exp:    `{
				"data": {
					"url":"http://localhost:8080/GMWJGSAP",
					"body": "lorem ipsum jibrish"
				},
				"url":"http://localhost:8080/GMWJGSAP",
				"status":200
			}`,
			actual: `{
				"data": {
					"body": "lorem ipsum jibrish",
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"url":"http://localhost:8080/GMWJGSAP",
				"status":200
			}`,
			noise:  []string{"data.url"},
			result: true,
		},
		{
			exp: `{
				"data": {
					"url":"http://localhost:8080/GMWJGSAP",
					"body": "lorem ipsum jibrish"
				},
				"url":"http://localhost:6060/",
				"status":200
			}`,
			actual: `{
				"data": {
					"body": "lorem ipsum jibrish",
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"url":"http://localhost:8080/GMWJGSAP",
				"status":200
			}`,
			noise:  []string{"data.url"},
			result: false,
		},
		{
			exp:    `{
				"data": {
					"url":"http://localhost:8080/GMWJGSAP",
					"body": "lorem ipsum jibrish"
				},
				"url":"http://localhost:6060/",
				"status":200
			}`,
			actual: `{
				"data": {
					"body": "lorem ipsum jibrish",
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"status":200
			}`,
			noise:  []string{"data.url"},
			result: false,
		},
		{
			exp: `{
				"data": {
					"url":"http://localhost:8080/GMWJGSAP",
					"body": "lorem ipsum jibrish"
				},
				"status":200
			}`,
			actual: `{
				"data": {
					"body": "lorem ipsum jibrish",
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"url":"http://localhost:6060/",
				"status":200
			}`,
			noise:  []string{"data.url"},
			result: false,
		},
		{
			exp: `{
				"data": {
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"status":200
			}`,
			actual: `{
				"data": {
					"foo":"bar",
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"status":200
			}`,
			noise:  []string{"data.url"},
			result: false,
		},
		{
			exp: `{
				"data": {
					"foo":"bar",
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"status":200
			}`,
			actual: `{
				"data": {
					"url":"http://localhost:8080/GMWJGSAP"
				},
				"status":200
			}`,
			noise:  []string{"data.url"},
			result: false,
		},
		{
			exp:    `{"name": "Rob Pike", "set": 21}`,
			actual: `{"name": "Rob Pike", "set": false}`,
			noise:  []string{"set"},
			result: true,
		},
		{
			exp: `{
				"name": "Ken Thompson",
				"set": true,
				"contact": ["1234567890", "0987654321"]}
			`,
			actual: `{ 
				"name": "Ken Thompson",
				"set": false,
				"contact": ["2454665654", "3449834321"]}
			`,
			noise:  []string{"contact"},
			result: false,
		},
		{
			exp: `[
				{
					"Name": "Robert",
					"Address": {
						"City": "Delhi",
						"Pin": 110082
					}
				},
				{
					"Name": "Ken",
					"Address": {
						"City": "Jaipur",
						"Pin": 121212
					}
				}
			]
			`,
			actual: `[
				{
					"Name": "Robert",
					"Address": {
						"City": "Delhi",
						"Pin": 110031
					}
				},
				{
					"Name": "Ken",
					"Address": {
						"City": "Jaipur",
						"Pin": 919191
					}
				}
			]
			`,
			noise:  []string{"Address.Pin"},
			result: true,
		},
		{
			exp: `[
				{
					"Name": "Robert",
					"Address": {
						"City": "New York",
						"Pin": 110082
					}
				},
				{
					"Name": "Ken",
					"Address": {
						"City": "London",
						"Pin": 121212
					}
				}
			]
			`,
			actual: `[
				{
					"Name": "Robert",
					"Address": {
						"City": "Delhi",
						"Pin": 110031
					}
				},
				{
					"Name": "Ken",
					"Address": {
						"City": "Jaipur",
						"Pin": 919191
					}
				}
			]
			`,
			noise:  []string{"Address.Pin"},
			result: false,
		},
		{
			exp: `[
				{
					"Name": "Robert",
					"Address": {
						"City": "New York",
						"Pin": 110082
					}
				},
				{
					"Name": "Ken",
					"Age": 79,
					"Address": {
						"City": "London",
						"Pin": 121212
					}
				}
			]
			`,
			actual: `[
				{
					"Name": "Robert",
					"Address": {
						"City": "Delhi",
						"Pin": 110031
					}
				},
				{
					"Name": "Ken",
					"Address": {
						"City": "Jaipur",
						"Pin": 919191
					}
				}
			]
			`,
			noise:  []string{"Address.Pin"},
			result: false,
		},
		{
			exp: `[
				{
					"Name": "Robert",
					"Address": {
						"City": "Delhi",
						"Pin": 110082
					}
				},
				{
					"Name": "Rob",
					"Address": {
						"City": "New York",
						"Pin": 454545
					}
				},
				{
					"Name": "Ken",
					"Address": {
						"City": "Jaipur",
						"Pin": 121212
					}
				} 
			]
			`,
			actual: `[
				{
					"Name": "Robert",
					"Address": {
						"City": "Delhi",
						"Pin": 110031
					}
				},
				{
					"Name": "Ken",
					"Address": {
						"City": "Jaipur",
						"Pin": 919191
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
							"Name": "Henry",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Pennsylvania",
								"Pin": 19003
							}	
						},
						{
							"Name": "Ford",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Chicago",
								"Pin": 19001
							}
						}
					]
				}
			`,
			actual: `
				{
					"Profiles": [
						{
							"Name": "Henry",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Pennsylvania",
								"Pin": 110082
							}
						},
						{
							"Name": "Ford",
							"Contact": ["123", "456"], 
							"Address": {
								"City": "Chicago",
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
				{
					"Profiles": [
						{
							"Name": "Henry",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Pennsylvania",
								"Pin": 19003
							}	
						},
						{
							"Name": "Ford",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Chicago",
								"Pin": 19001
							}
						},
						{
							"Name": "Ansvi",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Geogia",
								"Pin": 19001
							}
						}
					]
				}
			`,
			actual: `
				{
					"Profiles": [
						{
							"Name": "Henry",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Pennsylvania",
								"Pin": 110082
							}
						},
						{
							"Name": "Ford",
							"Contact": ["123", "456"], 
							"Address": {
								"City": "Chicago",
								"Pin": 110081
							}
						}
					]
				}
			`,
			noise:  []string{"set.somethingNotPresent", "Profiles.Address.Pin"},
			result: false,
		},
		{
			exp: `
				{
					"Profiles": [
						{
							"Name": "Henry",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Pennsylvania",
								"Pin": 19003
							}	
						},
						{
							"Name": "Ford",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Chicago",
								"Pin": 19001
							}
						}
						]
					}
					`,
					actual: `
					{
					"Profiles": [
						{
							"Name": "Henry",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Pennsylvania",
								"Pin": 110082
							}
						},
						{
							"Name": "Ansvi",
							"Contact": ["123", "456"],
							"Address": {
								"City": "Geogia",
								"Pin": 19001
							}
						},
						{
							"Name": "Ford",
							"Contact": ["123", "456"], 
							"Address": {
								"City": "Chicago",
								"Pin": 110081
							}
						}
					]
				}
			`,
			noise:  []string{"set.somethingNotPresent", "Profiles.Address.Pin"},
			result: false,
		},
		{
			exp: `
				[
					{
						"name":    "Ashley",
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
						"name":    "Ashley",
						"age":     21.0,
						"set":     true,
						"contact": [
							"Student", {"address": "XYZ"}
						]
					},
					{
						"name":    "Barney",
						"age":     21.0,
						"set":     12,
						"contact": [
							"Student", {"address": "ABC"}
						]
					}
				]
			`,
			noise:  []string{"set", "contact.address"},
			result: false,
		},
		{
			exp: `
			{
				"Name": "Chandler Muriel Bing",
				"Age": 31.0,
				"Address": {
					"City" : "New York",
					"PIN" : "110192"
				},
				"Father": {
					"Name": "Charles Bing",
					"Age": 60.0,
					"Address": {
						"City" : "Atlantic City",
						"PIN" : "110192"	
					}
				}
			}
			`,
			actual: `
			{
				"Name": "Chandler Muriel Bing",
				"Age": 31.0,
				"Address": {
					"City" : "New York",
					"PIN" : "110131"
				},
				"Father": {
					"Name": "Charles Bing",
					"Age": 70.0,
					"Address": {
						"City" : "Atlantic City",
						"PIN" : "321109"	
					}
				}
			}
			`,
			noise:  []string{"Father.Age", "Father.Address.PIN"},
			result: false,
		},
		{
			exp:    `{"name": "Rob Pike", "set": {"date": "21/01/2030", "time":"20:08"}}`,
			actual: `{"name": "Rob Pike", "set": {"date": "10/11/2051", "time":"12:21"}}`,
			noise:  []string{"set.date", "set.time"},
			result: true,
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
