// sample code for generating new testcases using random generation method by considering an API endpoint that validates user email and password to return a JWT token if credentials are valid

package main

import (
    "encoding/json"
    "fmt"
    "math/rand"
    "net/http"
    "net/url"
    "strconv"
    "strings"
    "time"
)

type Response struct {
    Token string `json:"token"`
}

// Function to call API endpoint
func callAPI(email string, password string) (Response, error) {
    // Create request body
    values := url.Values{}
    values.Set("email", email)
    values.Set("password", password)
    body := strings.NewReader(values.Encode())

    // Make HTTP request
    req, err := http.NewRequest("POST", "https://api.example.com/login", body)
    if err != nil {
        return Response{}, err
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return Response{}, err
    }
    defer resp.Body.Close()

    // Parse response body
    var response Response
    err = json.NewDecoder(resp.Body).Decode(&response)
    if err != nil {
        return Response{}, err
    }

    return response, nil
}

// Random generation testing function
func randomTesting(numCases int) [][]string {
    // List to store test cases
    testCases := [][]string{}

    // Set up random generator
    source := rand.NewSource(time.Now().UnixNano())
    r := rand.New(source)

    // Generate test cases
    for i := 0; i < numCases; i++ {
        email := strconv.Itoa(r.Intn(1000000)) + "@example.com"
        password := strconv.Itoa(r.Intn(1000000))
        testCases = append(testCases, []string{email, password})
    }

    return testCases
}

func main() {
    // Example usage
    numCases := 10
    testCases := randomTesting(numCases)

    for _, testCase := range testCases {
        fmt.Printf("Testing callAPI(\"%s\", \"%s\")...\n", testCase[0], testCase[1])
        response, err := callAPI(testCase[0], testCase[1])
        if err != nil {
            fmt.Printf("Error: %v\n", err)
        } else {
            fmt.Printf("Response: %v\n", response)
        }
    }
}
