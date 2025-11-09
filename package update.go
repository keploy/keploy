package update

import (
    "encoding/json"
    "fmt"
    "net/http"
    "runtime"
    "io" // Used for downloading files
    "os" // Used for file operations like replacing the binary
    "path/filepath" // Used for finding the executable path
    // You will need a library for semantic versioning (semver) comparison
    // Use the official Go-version library or a common one like Masterminds/semver
    "github.com/Masterminds/semver/v3" 
)

// Constants for the Keploy repository
const (
    KeployRepoOwner = "keploy"
    KeployRepoName  = "keploy"
    // GitHub API endpoint for the latest release
    GitHubLatestReleaseURL = "https://api.github.com/repos/" + KeployRepoOwner + "/" + KeployRepoName + "/releases/latest"
)

// --- Struct to parse the GitHub API response ---
type GitHubRelease struct {
    TagName    string `json:"tag_name"`
    Assets     []struct {
        Name        string `json:"name"`
        DownloadURL string `json:"browser_download_url"`
    } `json:"assets"`
}

// 1. GetLatestVersion Fetches the latest release version from GitHub.
func GetLatestVersion() (*GitHubRelease, error) {
    resp, err := http.Get(GitHubLatestReleaseURL)
    if err != nil {
        return nil, fmt.Errorf("failed to reach GitHub API: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
    }

    var release GitHubRelease
    if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
        return nil, fmt.Errorf("failed to parse GitHub response: %w", err)
    }

    return &release, nil
}

// 2. IsUpdateAvailable Compares the current version with the latest version.
func IsUpdateAvailable(currentVersion string, latestRelease *GitHubRelease) (bool, error) {
    // ⚠️ NOTE: You must ensure your binary version (currentVersion) is correctly set at build time (e.g., v3.0.0)
    current, err := semver.NewVersion(currentVersion)
    if err != nil {
        // If the current version can't be parsed (e.g., "v0.0.0-dev"), treat it as needing a check
        return true, nil 
    }

    latest, err := semver.NewVersion(latestRelease.TagName)
    if err != nil {
        return false, fmt.Errorf("failed to parse latest tag %s: %w", latestRelease.TagName, err)
    }

    return latest.GreaterThan(current), nil
}

// 3. DownloadAndReplace handles the download and replacement process. (Placeholder)
func DownloadAndReplace(latestRelease *GitHubRelease) error {
    // --- STEP 3A: Find the correct asset name ---
    assetName := fmt.Sprintf("keploy-%s-%s", runtime.GOOS, runtime.GOARCH) // Example logic; Keploy may use a different convention (like a .tar.gz or .zip)
    
    var downloadURL string
    for _, asset := range latestRelease.Assets {
        if asset.Name == assetName {
            downloadURL = asset.DownloadURL
            break
        }
    }

    if downloadURL == "" {
        return fmt.Errorf("no suitable binary found for %s/%s", runtime.GOOS, runtime.GOARCH)
    }

    // --- STEP 3B: Download the file ---
    // (Actual download and file replacement code is complex due to OS/file locks, so this is a placeholder)
    
    // --- STEP 3C: Get the path to the currently running executable ---
    currentBinaryPath, err := os.Executable()
    if err != nil {
        return fmt.Errorf("failed to get current executable path: %w", err)
    }

    fmt.Printf("Downloading new version from %s to %s...\n", downloadURL, currentBinaryPath)

    // ... (Your code to download the new binary and replace currentBinaryPath will go here) ...
    
    return nil
}

// Main function to be called from the CLI
func UpdateCLI(currentVersion string) error {
    release, err := GetLatestVersion()
    if err != nil {
        return fmt.Errorf("update check failed: %w", err)
    }

    available, err := IsUpdateAvailable(currentVersion, release)
    if err != nil {
        return fmt.Errorf("version comparison failed: %w", err)
    }

    if !available {
        fmt.Printf("Keploy is already up-to-date (Version %s).\n", currentVersion)
        return nil
    }

    fmt.Printf("New version %s available! Updating...\n", release.TagName)
    
    return DownloadAndReplace(release)
}

// --- END pkg/update/updater.go ---