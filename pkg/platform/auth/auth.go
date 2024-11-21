package auth

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "time"

    "go.keploy.io/server/v2/pkg/models"
    "go.keploy.io/server/v2/pkg/service"
    "go.keploy.io/server/v2/utils"
    "go.uber.org/zap"
)

type Auth struct {
    serverURL      string
    jwtToken       string
    installationID string
    logger         *zap.Logger
    GitHubClientID string
}

func New(serverURL string, installationID string, logger *zap.Logger, gitHubClientID string) service.Auth {
    return &Auth{
        serverURL:      serverURL,
        installationID: installationID,
        logger:         logger,
        GitHubClientID: gitHubClientID,
    }
}

func (a *Auth) Login(ctx context.Context) bool {
    deviceCodeResp, err := requestDeviceCode(a.logger, a.GitHubClientID)
    if err != nil {
        a.logger.Error("Error requesting device code", zap.Error(err))
        return false
    }

    promptUser(deviceCodeResp)

    tokenResp, err := pollForAccessToken(ctx, a.logger, deviceCodeResp.DeviceCode, a.GitHubClientID, deviceCodeResp.Interval)
    if err != nil {
        if ctx.Err() == context.Canceled {
            return false
        }
        utils.LogError(a.logger, err, "failed to poll for access token")
        return false
    }

    userEmail, isValid, authErr, err := a.Validate(ctx, tokenResp.AccessToken)
    if err != nil {
        if ctx.Err() == context.Canceled {
            return false
        }
        a.logger.Error("Error checking auth", zap.Error(err))
        return false
    }

    if !isValid {
        a.logger.Error("Invalid token", zap.Any("error", authErr))
        return false
    }
    a.logger.Info("Successfully logged in to Keploy using GitHub as " + userEmail)
    return true
}

func (a *Auth) Validate(ctx context.Context, token string) (string, bool, string, error) {
    url := fmt.Sprintf("%s/auth/github", a.serverURL)
    requestBody := &models.AuthReq{
        GitHubToken:    token,
        InstallationID: a.installationID,
    }
    requestJSON, err := json.Marshal(requestBody)
    if err != nil {
        utils.LogError(a.logger, err, "failed to marshal request body for authentication")
        return "", false, "", fmt.Errorf("error marshaling request body for authentication: %s", err.Error())
    }

    req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(requestJSON))
    if err != nil {
        utils.LogError(a.logger, err, "failed to create request for authentication")
        return "", false, "", fmt.Errorf("error creating request for authentication: %s", err.Error())
    }
    req.Header.Set("Content-Type", "application/json")
    client := &http.Client{}
    res, err := client.Do(req)
    if err != nil {
        return "", false, "", fmt.Errorf("failed to authenticate: %s", err.Error())
    }

    defer func() {
        err := res.Body.Close()
        if err != nil {
            utils.LogError(a.logger, err, "failed to close response body for authentication")
        }
    }()

    var respBody models.AuthResp
    err = json.NewDecoder(res.Body).Decode(&respBody)
    if err != nil {
        return "", false, "", fmt.Errorf("error unmarshalling the authentication response: %s", err.Error())
    }

    if res.StatusCode != 200 || res.StatusCode >= 300 {
        return "", false, "", fmt.Errorf("failed to authenticate: %s", respBody.Error)
    }

    a.jwtToken = respBody.JwtToken
    return respBody.EmailID, respBody.IsValid, respBody.Error, nil
}

func (a *Auth) GetToken(ctx context.Context) (string, error) {
    if a.jwtToken == "" {
        _, _, _, err := a.Validate(ctx, "")
        if err != nil {
            return "", err
        }
    }

    if a.jwtToken == "" {
        a.logger.Warn("Looks like you are not logged in.")
        a.logger.Warn("Please follow the instructions to login.")
        isSuccessful := a.Login(ctx)
        if !isSuccessful {
            return "", fmt.Errorf("failed to login")
        }
    }

    return a.jwtToken, nil
}

func requestDeviceCode(logger *zap.Logger, gitHubClientID string) (*models.DeviceCodeResponse, error) {
    data := url.Values{}
    data.Set("client_id", gitHubClientID)
    data.Set("scope", "read:user")

    resp, err := http.PostForm(models.DeviceCodeURL, data)
    if err != nil {
        return nil, err
    }
    defer func() {
        err := resp.Body.Close()
        if err != nil {
            utils.LogError(logger, err, "failed to close response body")
        }
    }()

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }

    parsed, err := url.ParseQuery(string(body))
    if err != nil {
        return nil, err
    }

    deviceCodeResp := &models.DeviceCodeResponse{
        DeviceCode:      parsed.Get("device_code"),
        UserCode:        parsed.Get("user_code"),
        VerificationURI: parsed.Get("verification_uri"),
        Interval:        5,
    }

    return deviceCodeResp, nil
}

func promptUser(deviceCodeResp *models.DeviceCodeResponse) {
    fmt.Printf("Please go to %s and enter the code: %s\n", deviceCodeResp.VerificationURI, deviceCodeResp.UserCode)
}

func pollForAccessToken(ctx context.Context, logger *zap.Logger, deviceCode, gitHubClientID string, interval int) (*models.AccessTokenResponse, error) {
    data := url.Values{}
    data.Set("client_id", gitHubClientID)
    data.Set("device_code", deviceCode)
    data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

    fmt.Println("waiting for approval (this might take 4-5 sec for approval)...")

    for {
        resp, err := http.PostForm(models.TokenURL, data)
        if err != nil {
            return nil, err
        }
        defer func() {
            err := resp.Body.Close()
            if err != nil {
                utils.LogError(logger, err, "failed to close response body")
            }
        }()

        body, err := io.ReadAll(resp.Body)
        if err != nil {
            return nil, err
        }

        if resp.StatusCode != http.StatusOK {
            return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
        }

        var tokenResp models.AccessTokenResponse
        parsed, err := url.ParseQuery(string(body))
        if err != nil {
            return nil, err
        }
        if parsed.Get("error") == "authorization_pending" {
            select {
            case <-time.After(time.Duration(interval) * time.Second):
            case <-ctx.Done():
                return nil, ctx.Err()
            }
            continue
        } else if parsed.Get("error") != "" {
            return nil, fmt.Errorf("error: %s", parsed.Get("error_description"))
        }
        if accessToken := parsed.Get("access_token"); accessToken != "" {
            return &models.AccessTokenResponse{
                AccessToken: accessToken,
                TokenType:   parsed.Get("token_type"),
                Scope:       parsed.Get("scope"),
            }, nil
        }
        return &tokenResp, nil
    }
}
