package backend

type UAA struct {
	ClientId     string `json:"clientid"`
	ClientSecret string `json:"clientsecret"`
	OAuthUrl     string `json:"url"`
}

type Token struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
	Jti         string `json:"jti"`
}
