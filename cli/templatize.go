package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("Templatize", Templatize)
}

func Templatize(ctx context.Context, logger *zap.Logger, conf *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "templatize",
		Short:   "templatize the keploy testcases for re-record",
		Example: `keploy templatize -c "/path/to/user/app"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Read the testcases from the path provided.
			for _, testSet := range conf.Templatize.TestSets {
				path := conf.Path + "/keploy/" + testSet
				utils.ReadTempValues(testSet)
				// Read the utils.TemplatizedValues from the templatized.json file.
				testYaml := testdb.New(logger, conf.Path)
				tcs, err := testYaml.GetTestCases(ctx, path)
				if err != nil {
					logger.Error("failed to get testcases", zap.Error(err))
					return err
				}
				// logger.Info("These are the testcases that we got:", zap.Any("testcases", tcs))
				// Just get the request information from the testcases.
				var requests []models.HTTPReq

				for _, tc := range tcs {
					// logger.Info("Request Information", zap.Any("Request", tc.HTTPReq))
					requests = append(requests, tc.HTTPReq)
				}
				// Initialize a map to store counts of each field-value pair
				// commonFields := make(map[string]int)
				// // Find out the commmon fields in the request.
				// for _, req := range requests {
				// 	for key, val := range req {
				// 		if _, ok := commonFields[key]; ok {
				// 			commonFields[key]++
				// 		} else {
				// 			commonFields[key] = 1
				// 		}
				// 	}
				// }
				// Get the body of the response.
				for i := 0; i < len(tcs)-1; i++ {
					jsonResponse, err := parseIntoJson(tcs[i].HTTPResp.Body)
					if err != nil {
						logger.Error("failed to parse response into json", zap.Error(err))
						return err
					}
					// Compare the keys to the headers.
					for j := i + 1; j < len(tcs); j++ {
						compareVals(tcs[j].HTTPReq.Header, jsonResponse)
						// Write the new headers to the file.
						err = testYaml.InsertTestCase(ctx, tcs[j], path)
						if err != nil {
							logger.Error("Error inserting the new testcase to the file", zap.Error(err))
						}
					}
					// Add the jsonResponse back to tcs.
					jsonData, err := json.Marshal(jsonResponse)
					if err != nil {
						logger.Error("failed to marshal json data", zap.Error(err))
						return err
					}
					tcs[i].HTTPResp.Body = string(jsonData)
					// Write the new response to the file.
					err = testYaml.InsertTestCase(ctx, tcs[i], path)
					if err != nil {
						logger.Error("Error inserting the new testcase to the file", zap.Error(err))
					}
				}
				// Compare the requests for the common fields.
				for i := 0; i < len(tcs)-1; i++ {
					// Check for headers first.
					for j := i + 1; j < len(tcs); j++ {
						compareReqHeaders(tcs[i].HTTPReq.Header, tcs[i+1].HTTPReq.Header)
						err = testYaml.InsertTestCase(ctx, tcs[j], path)
						if err != nil {
							logger.Error("Error inserting the new testcase to the file", zap.Error(err))
						}
					}
					// Record the new testcases.
					err = testYaml.InsertTestCase(ctx, tcs[i], path)
					if err != nil {
						logger.Error("Error inserting the new testcase to the file", zap.Error(err))
					}
				}
				// Parse the URL and check if the value is in the body.
				for i := 0; i < len(tcs)-1; i++ {
					jsonResponse, err := parseIntoJson(tcs[i].HTTPResp.Body)
					if err != nil {
						logger.Error("failed to parse response into json", zap.Error(err))
						return err
					}
					for j := i + 1; j < len(tcs); j++ {
						url1, err := url.Parse(tcs[j].HTTPReq.URL)
						url := strings.Split(url1.Path, "/")
						if err != nil {
							logger.Error("failed to parse the url", zap.Error(err))
							break
						}
						compareVals(url[len(url)-1], jsonResponse)
						err = testYaml.InsertTestCase(ctx, tcs[j], path)
						if err != nil {
							logger.Error("Error inserting the new testcase to the file", zap.Error(err))
						}
					}
					// Record the new testcase.
					jsonData, err := json.Marshal(jsonResponse)
					if err != nil {
						logger.Error("failed to marshal json data", zap.Error(err))
						return err
					}
					tcs[i].HTTPResp.Body = string(jsonData)
					err = testYaml.InsertTestCase(ctx, tcs[i], path)
					if err != nil {
						logger.Error("Error inserting the new testcase to the file", zap.Error(err))
					}
				}
				// for i, req := range requests {
				// 	for key, val := range req.Header {
				// 		// Check if value matches any value in the response.
				// 		if key == "Authorization" {
				// 			val = strings.Replace(val, "Bearer ", "", -1)
				// 		}
				// 		index := contains(responses, val)
				// 		if index != -1 && key != "Content-Length" {
				// 			logger.Info("We found a match", zap.Any("value", val))
				// 			if index < i {
				// 				requests =
				// 			}
				// 		}
				// 	}
				// }
				// Write the resultMap to a file called templatized.yaml
				// logger.Debug("This is the map that we have at the end", zap.Any("resultMap", resultMap))
				//Write the values of the templatizedValues map to it.
				jsonTemp, err := json.MarshalIndent(utils.TemplatizedValues, "", " ")
				if err != nil {
					logger.Error("failed to marshal the temp data into yaml", zap.Error(err))
				}
				err = os.WriteFile(path+"/templatized.json", jsonTemp, 0644)
				if err != nil {
					logger.Error("Error writing to templatized.json", zap.Error(err))
				}
				// for key, val := range resultMap {
				// 	_, err := file.WriteString(fmt.Sprintf("test-%d: test-%d\n", key, val))
				// 	if err != nil {
				// 		logger.Error("failed to write to file", zap.Error(err))
				// 		return err
				// 	}
				// }
				// Compare the json responses and find out the common fields between the testcases.
				// for i := range len(tcs) - 2 {
				// 	if tcs[i].HTTPResp.Body == "" || tcs[i+1].HTTPReq.Body == "" {
				// 		continue
				// 	}
				// 	jsonResponse, err := parseIntoJson(tcs[i].HTTPResp.Body)
				// 	if err != nil {
				// 		logger.Error("failed to parse response into json", zap.Error(err))
				// 		fmt.Println("This is the value of the jsonResponse:", jsonResponse)
				// 		return err
				// 	}
				// 	jsonRequest, err := parseIntoJson(tcs[i+1].HTTPReq.Body)
				// 	if err != nil {
				// 		logger.Error("failed to parse request in json", zap.Error(err))
				// 		fmt.Println("This is the value of the jsonRequest:", jsonRequest)
				// 		return err
				// 	}
				// 	// Compare the key and value of both the request and response
				// 	compareReqandResp(jsonRequest, jsonResponse)
				// }
			}
			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add templatize flags")
		return nil
	}

	return cmd
}

func checkType(val interface{}) string {
	switch v := val.(type) {
	case map[string]interface{}:
		return "map"
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		return v
	}
	return ""
}

func compareReqHeaders(req1 map[string]string, req2 map[string]string) {
	for key, val1 := range req1 {
		// Check if the value is already present in the templatized values.
		if strings.HasPrefix(val1, "{{") && strings.HasSuffix(val1, "}}") {
			continue
		}
		if val2, ok := req2[key]; ok {
			if val1 == val2 {
				newKey := insertUnique(key, val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				req2[key] = fmt.Sprintf("{{ %s }}", newKey)
				req1[key] = fmt.Sprintf("{{ %s }}", newKey)
			}
		}
	}
}

// func compareReqandResp(jsonRequest map[string]string, jsonResponse map[string]interface{}) {
// 	for key, val := range jsonRequest {
// 		if key == "Authorization" {
// 			val = strings.Replace(val, "Bearer ", "", -1)
// 		}
// 		fmt.Println("This is the key and the value", key, val)
// 		if value, ok := jsonResponse[key]; ok {
// 			if value == val {
// 				// Write the value to a file called templatized.yaml
// 				file, err := os.Create("templatized.yaml")
// 				if err != nil {
// 					fmt.Println("Error in opening file:", err)
// 				}
// 				defer file.Close()
// 				_, err = file.WriteString(fmt.Sprintf("%s: %s\n", key, val))
// 				if err != nil {
// 					log.Fatal("Error in writing to the file", err)
// 				}
// 			}
// 		}
// 	}
// }

// func compareJsons(map1 map[string]string, map2 map[string]interface{}) {
// 	for _, val := range map2 {
// 		key, token := findToken(&val)
// 		for key1, val1 := range map1 {
// 			if key == "Authorization" && len(strings.Split(val1, " ")) > 1 {
// 				val1 = strings.Split(val1, " ")[1]
// 			}
// 		}
// 	}
// }

// func findToken(val *interface{}) (string, string) {
// 	switch v := (*val).(type) {
// 	case string:
// 		// if strings.Contains(v, "Bearer") {
// 		// 	fmt.Println("This is the value of the token:", v)
// 		// 	return v
// 		// }
// 	case map[string]interface{}:
// 		for key, val := range v {
// 			if key == "token" {
// 				v[key] = val.(string)
// 				return key, val.(string)
// 			} else {
// 				findToken(&val)
// 			}
// 		}
// 	case []interface{}:
// 		for _, val := range v {
// 			findToken(&val)
// 		}
// 	default:
// 		fmt.Println("This is the default value:", v)
// 	}
// 	return "", ""
// }

func compareVals(map1 interface{}, map2 map[string]interface{}) {
	switch v := map1.(type) {
	case map[string]string:
		for key, val1 := range v {
			authType := ""
			if key == "Authorization" && len(strings.Split(val1, " ")) > 1 {
				authType = strings.Split(val1, " ")[0]
				val1 = strings.Split(val1, " ")[1]
			}
			if strings.HasPrefix(val1, "{{") && strings.HasSuffix(val1, "}}") {
				continue
			}
			newKey, ok := parseBody(val1, map2)
			if !ok {
				continue
			}
			// Add the template.
			val1 = strings.Replace(val1, val1, fmt.Sprintf("%s {{ %s }}", authType, newKey), -1)
			v[key] = val1
		}
	case *string:
		if strings.HasPrefix(*v, "{{") && strings.HasSuffix(*v, "}}") {
			return
		}
		newKey, ok := parseBody(*v, map2)
		if !ok {
			return
		}
		// Add the template
		*v = strings.Replace(*v, *v, fmt.Sprintf("{{ %s }}", newKey), -1)
		map1 = v
	}

}

func parseBody(val1 string, map2 map[string]interface{}) (string, bool) {
	for key1, val2 := range map2 {
		valType := checkType(val2)
		if valType == "map" {
			map3, _ := val2.(map[string]interface{})
			for key2, v := range map3 {
				if _, ok := utils.TemplatizedValues[val1]; ok {
					continue
				}
				v := checkType(v)
				if val1 == v {
					newKey := insertUnique(key2, v, utils.TemplatizedValues)
					if newKey == "" {
						newKey = key2
					}
					map3[newKey] = fmt.Sprintf("{{ %s }}", newKey)
					return newKey, true
				}
			}
		} else if val1 == checkType(val2) {
			newKey := insertUnique(key1, checkType(val2), utils.TemplatizedValues)
			if newKey == "" {
				newKey = key1
			}
			map2[key1] = fmt.Sprintf("{{ %s }}", newKey)
			return newKey, true
		}
	}
	return "", false
}

func insertUnique(baseKey, value string, myMap map[string]interface{}) string {
	if myMap[baseKey] == value {
		return ""
	}
	key := baseKey
	i := 0
	for {
		if _, exists := myMap[key]; !exists {
			myMap[key] = value
			break
		}
		i++
		key = baseKey + strconv.Itoa(i)
	}
	return key
}

	func parseIntoJson(response string) (map[string]interface{}, error) {
		// Parse the response into a json object.
		var jsonResponse map[string]interface{}
		if err := json.Unmarshal([]byte(response), &jsonResponse); err != nil {
			return nil, err
		}
		return jsonResponse, nil
	}

// func contains(responses []string, val string) int {
// 	for i, response := range responses {
// 		if strings.Contains(response, val) {
// 			return i
// 		}
// 	}
// 	return -1
// }
