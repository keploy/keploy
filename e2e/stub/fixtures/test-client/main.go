package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

type User struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: test-client <command>")
		fmt.Println("Commands: health, all")
		os.Exit(1)
	}

	baseURL := os.Getenv("API_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:9000"
	}

	command := os.Args[1]

	switch command {
	case "health":
		runHealthCheck(baseURL)
	case "all":
		runAllTests(baseURL)
	default:
		fmt.Printf("Unknown command: %s\n", command)
		os.Exit(1)
	}
}

func runHealthCheck(baseURL string) {
	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		fmt.Printf("Health check failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Health check failed with status: %d\n", resp.StatusCode)
		os.Exit(1)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("Failed to decode response: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Health check passed: %v\n", result)
}

func runAllTests(baseURL string) {
	// Test 1: Health check
	fmt.Println("Test 1: Health check")
	runHealthCheck(baseURL)

	// Test 2: Create a user
	fmt.Println("\nTest 2: Create user")
	user := User{
		Name:  "Test User",
		Email: "test@example.com",
	}
	userData, _ := json.Marshal(user)
	resp, err := http.Post(baseURL+"/api/users", "application/json", bytes.NewBuffer(userData))
	if err != nil {
		fmt.Printf("Failed to create user: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var createdUser User
	if err := json.NewDecoder(resp.Body).Decode(&createdUser); err != nil {
		fmt.Printf("Failed to decode user: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created user: %+v\n", createdUser)

	// Test 3: Get all users
	fmt.Println("\nTest 3: Get all users")
	resp, err = http.Get(baseURL + "/api/users")
	if err != nil {
		fmt.Printf("Failed to get users: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var users []User
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		fmt.Printf("Failed to decode users: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Users: %+v\n", users)

	// Test 4: Get products
	fmt.Println("\nTest 4: Get products")
	resp, err = http.Get(baseURL + "/api/products")
	if err != nil {
		fmt.Printf("Failed to get products: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Products: %s\n", string(body))

	// Test 5: Get time
	fmt.Println("\nTest 5: Get time")
	resp, err = http.Get(baseURL + "/api/time")
	if err != nil {
		fmt.Printf("Failed to get time: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ = io.ReadAll(resp.Body)
	fmt.Printf("Time: %s\n", string(body))

	// Test 6: Echo
	fmt.Println("\nTest 6: Echo")
	echoData := map[string]interface{}{
		"message": "Hello, World!",
		"number":  42,
	}
	echoJSON, _ := json.Marshal(echoData)
	resp, err = http.Post(baseURL+"/api/echo", "application/json", bytes.NewBuffer(echoJSON))
	if err != nil {
		fmt.Printf("Failed to echo: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ = io.ReadAll(resp.Body)
	fmt.Printf("Echo response: %s\n", string(body))

	// Test 7: Search
	fmt.Println("\nTest 7: Search")
	resp, err = http.Get(baseURL + "/api/search?q=Product")
	if err != nil {
		fmt.Printf("Failed to search: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ = io.ReadAll(resp.Body)
	fmt.Printf("Search results: %s\n", string(body))

	fmt.Println("\nAll tests completed successfully!")
}
