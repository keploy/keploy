package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)


const (
	RepoOwner = "keploy"
	RepoName  = "keploy"
)

type Stargazer struct {
	Login string `json:"login"`
}


func fetchStargazers(token string) ([]Stargazer, error) {
	var stargazers []Stargazer
	client := &http.Client{Timeout: 10 * time.Second}
	page := 1

	for {
		url := fmt.Sprintf("https://api.github.com/repos/%s/%s/stargazers?per_page=100&page=%d", RepoOwner, RepoName, page)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}

		
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github.v3+json")

		resp, err := client.Do(req)
		if resp.StatusCode == 403 {
			resetTime := resp.Header.Get("X-RateLimit-Reset")
			fmt.Println("Rate limit exceeded. Try again after:", resetTime)
			return nil, fmt.Errorf("rate limit exceeded")
		}
		
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("error fetching stargazers: %s", string(body))
		}

		var data []Stargazer
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return nil, err
		}

		if len(data) == 0 {
			break 
		}

		stargazers = append(stargazers, data...)
		page++
	}

	return stargazers, nil
}

func saveToFile(filename string, data []Stargazer) error {
	file, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}


	if err := os.MkdirAll("data", os.ModePerm); err != nil {
		return err
	}

	return os.WriteFile(filename, file, 0644)
}

func main() {
	token := os.Getenv("PRO_ACCESS_TOKEN")
	if token == "" {
		fmt.Println("Error: PRO_ACCESS_TOKEN environment variable not set")
		return
	}

	stargazers, err := fetchStargazers(token)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	err = saveToFile("data/stargazers.json", stargazers)
	if err != nil {
		fmt.Println("Error saving file:", err)
		return
	}

	fmt.Printf("Fetched %d stargazers.\n", len(stargazers))
}
