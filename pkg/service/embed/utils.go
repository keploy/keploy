package embed

import (
	"encoding/json"
	"os"

	"github.com/pkoukk/tiktoken-go"
)

// CountTokens returns the number of tokens in a text string.
func CountTokens(text string, encodingName string) (int, error) {
	tke, err := tiktoken.EncodingForModel(encodingName)
	if err != nil {
		tke, err = tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			return 0, err
		}
	}
	tokens := tke.Encode(text, nil, nil)
	return len(tokens), nil
}

// LoadJSON loads data from a JSON file.
func LoadJSON(jsonFile string, v interface{}) error {
	data, err := os.ReadFile(jsonFile)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
