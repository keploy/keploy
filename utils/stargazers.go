package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Stargazer represents a GitHub stargazer entry from the API.
type Stargazer struct {
	User      User      `json:"user"`
	StarredAt time.Time `json:"starred_at"`
}

// User represents basic user info from the stargazers API.
type User struct {
	Login   string `json:"login"`
	HTMLURL string `json:"html_url"`
}

// UserDetails matches the exact structure from the JavaScript output.
type UserDetails struct {
	Username   string `json:"username"`
	ProfileURL string `json:"profile_url"`
	Email      string `json:"email"`
	Company    string `json:"company"`
	Location   string `json:"location"`
	Website    string `json:"website"`
	LinkedIn   string `json:"linkedin"`
	Twitter    string `json:"twitter"`
	Bio        string `json:"bio"`
}

const (
	githubAPIURL         = "https://api.github.com"
	repoPath             = "keploy/keploy" // Hardcoded for automation
	batchSize            = 50
	delayBetweenRequests = 2000 * time.Millisecond
	maxRetries           = 3
	rateLimitDelay       = 60 * time.Second
	perPage              = 100
	dataDir              = "../data" // Adjusted to point to root-level data folder
	outputFile           = "stargazers.json"
)

var (
	client = &http.Client{Timeout: 10 * time.Second}
	token  = os.Getenv("GITHUB_TOKEN")
)

func main() {
	// Set up logging
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if token == "" {
		log.Fatal("GITHUB_TOKEN environment variable is not set")
	}

	ctx := context.Background()

	// Fetch all stargazers
	stargazers, err := fetchAllStargazers(ctx)
	if err != nil {
		log.Fatalf("Failed to fetch stargazers: %v", err)
	}

	// Enrich stargazers with user details
	enriched, err := enrichStargazersInBatches(ctx, stargazers)
	if err != nil {
		log.Fatalf("Failed to enrich stargazers: %v", err)
	}

	// Save to JSON file
	if err := saveToJSON(enriched); err != nil {
		log.Fatalf("Failed to save stargazers to JSON: %v", err)
	}

	log.Printf("Successfully saved %d stargazers to %s/%s", len(enriched), dataDir, outputFile)
}

// fetchAllStargazers retrieves all stargazers for the repo.
func fetchAllStargazers(ctx context.Context) ([]Stargazer, error) {
	var allStargazers []Stargazer
	page := 1
	hasMore := true

	for hasMore {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/repos/%s/stargazers?page=%d&per_page=%d", githubAPIURL, repoPath, page, perPage), nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %v", err)
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github.v3.star+json")

		resp, err := doRequestWithRetry(req)
		if err != nil {
			return nil, fmt.Errorf("fetching page %d: %v", page, err)
		}
		defer resp.Body.Close()

		var stargazers []Stargazer
		if err := json.NewDecoder(resp.Body).Decode(&stargazers); err != nil {
			return nil, fmt.Errorf("decoding response: %v", err)
		}

		allStargazers = append(allStargazers, stargazers...)
		hasMore = len(stargazers) == perPage
		page++
		log.Printf("Fetched page %d, total stargazers: %d", page-1, len(allStargazers))
	}

	return allStargazers, nil
}

// enrichStargazersInBatches enriches stargazers in batches.
func enrichStargazersInBatches(ctx context.Context, stargazers []Stargazer) ([]UserDetails, error) {
	var enrichedStargazers []UserDetails
	var mu sync.Mutex
	var wg sync.WaitGroup

	sem := make(chan struct{}, batchSize) // Limit concurrency

	for i := 0; i < len(stargazers); i += batchSize {
		end := i + batchSize
		if end > len(stargazers) {
			end = len(stargazers)
		}
		batch := stargazers[i:end]

		wg.Add(len(batch))
		for _, stargazer := range batch {
			sem <- struct{}{} // Acquire semaphore
			go func(sg Stargazer) {
				defer wg.Done()
				defer func() { <-sem }() // Release semaphore

				userDetails, err := fetchUserDetailsWithRetry(ctx, sg.User.Login)
				if err != nil {
					log.Printf("Failed to fetch details for %s: %v", sg.User.Login, err)
					return
				}

				details := UserDetails{
					Username:   sg.User.Login,
					ProfileURL: sg.User.HTMLURL,
					Email:      nullString(userDetails["email"]),
					Company:    nullString(userDetails["company"]),
					Location:   nullString(userDetails["location"]),
					Website:    nullString(userDetails["blog"]),
					LinkedIn:   extractLinkedIn(userDetails),
					Twitter:    formatTwitter(userDetails["twitter_username"]),
					Bio:        nullString(userDetails["bio"]),
				}

				mu.Lock()
				enrichedStargazers = append(enrichedStargazers, details)
				mu.Unlock()
			}(stargazer)
		}

		wg.Wait() // Wait for batch to complete
		time.Sleep(delayBetweenRequests)
		log.Printf("Enriched batch ending at %d/%d", end, len(stargazers))
	}

	return enrichedStargazers, nil
}

// fetchUserDetailsWithRetry fetches user details with retry logic.
func fetchUserDetailsWithRetry(ctx context.Context, username string) (map[string]interface{}, error) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/users/%s", githubAPIURL, username), nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %v", err)
		}
		req.Header.Set("Authorization", "token "+token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HTTP request failed: %v", err)
		}

		if resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			var details map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&details); err != nil {
				return nil, fmt.Errorf("decoding user details: %v", err)
			}
			return details, nil
		}

		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden { // Rate limit
			log.Printf("Rate limit hit for %s, waiting 60s (attempt %d/%d)", username, attempt, maxRetries)
			time.Sleep(rateLimitDelay)
			continue
		}

		log.Printf("Attempt %d failed for %s: %s", attempt, username, resp.Status)
		if attempt == maxRetries {
			return nil, fmt.Errorf("failed to fetch user details for %s after %d retries: %s", username, maxRetries, resp.Status)
		}
		time.Sleep(delayBetweenRequests)
	}

	return nil, fmt.Errorf("unreachable code")
}

// doRequestWithRetry handles retries for the stargazers endpoint.
func doRequestWithRetry(req *http.Request) (*http.Response, error) {
	for i := 0; i < maxRetries; i++ {
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HTTP request failed: %v", err)
		}

		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			log.Printf("Rate limit hit, waiting %v...", rateLimitDelay)
			time.Sleep(rateLimitDelay)
			continue
		}

		if i == maxRetries-1 {
			return nil, fmt.Errorf("request failed after %d retries: %s", maxRetries, resp.Status)
		}

		log.Printf("Retry %d/%d after %s", i+1, maxRetries, resp.Status)
		time.Sleep(delayBetweenRequests)
	}

	return nil, fmt.Errorf("unreachable code")
}

// saveToJSON writes the enriched stargazers to a JSON file.
func saveToJSON(stargazers []UserDetails) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("creating data directory: %v", err)
	}

	filePath := filepath.Join(dataDir, outputFile)
	data, err := json.MarshalIndent(stargazers, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling JSON: %v", err)
	}

	return os.WriteFile(filePath, data, 0644)
}

// Helper functions
func nullString(val interface{}) string {
	if val == nil {
		return "N/A"
	}
	if s, ok := val.(string); ok && s != "" {
		return s
	}
	return "N/A"
}

func extractLinkedIn(userDetails map[string]interface{}) string {
	blog, ok := userDetails["blog"].(string)
	if ok && blog != "" && strings.Contains(blog, "linkedin.com") {
		return blog
	}
	return "N/A"
}

func formatTwitter(twitter interface{}) string {
	if twitterStr, ok := twitter.(string); ok && twitterStr != "" {
		return fmt.Sprintf("https://twitter.com/%s", twitterStr)
	}
	return "N/A"
}