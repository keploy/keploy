// sample code for generating new testcases using combinational testing method by considering an API endpoint that validates user email and password to return a JWT token if credentials are valid

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

// Combinatorial testing function
func combinatorialTesting(emailValues []string, passwordValues []string) [][]string {
    // List to store test cases
    testCases := [][]string{}

    // Generate all combinations of email and password values
    for _, email := range emailValues {
        for _, password := range passwordValues {
            testCases = append(testCases, []string{email, password})
        }
    }

    return testCases
}

func main() {
    // Example usage
    emailValues := []string{"user1@example.com", "user2@example.com", "user3@example.com"}
    passwordValues := []string{"password1", "password2", "password3"}
    testCases := combinatorialTesting(emailValues, passwordValues)

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
