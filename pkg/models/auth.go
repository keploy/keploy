package models

type AuthReq struct {
	InstallationID string `json:"installationID"`
	GitHubToken    string `json:"gitHubtoken"`
	Platform       string `json:"platform"`
}

type AuthResp struct {
	IsValid  bool   `json:"isValid"`
	EmailID  string `json:"email"`
	JwtToken string `json:"jwtToken"`
	Error    string `json:"error"`
}
