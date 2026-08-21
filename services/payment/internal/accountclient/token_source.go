package accountclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ClientCredentialsTokenSource implements TokenSource against the
// standard OAuth2 client-credentials grant. Ported verbatim from
// services/las/internal/glclient — tokens are cached in memory and
// refreshed once, shortly before expiry.
type ClientCredentialsTokenSource struct {
	tokenURL     string
	clientID     string
	clientSecret string
	scope        string
	httpClient   *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func NewClientCredentialsTokenSource(tokenURL, clientID, clientSecret, scope string) *ClientCredentialsTokenSource {
	return &ClientCredentialsTokenSource{
		tokenURL: tokenURL, clientID: clientID, clientSecret: clientSecret, scope: scope,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

const earlyRefreshWindow = 30 * time.Second

func (s *ClientCredentialsTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && time.Now().Add(earlyRefreshWindow).Before(s.expires) {
		return s.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", s.clientID)
	form.Set("client_secret", s.clientSecret)
	if s.scope != "" {
		form.Set("scope", s.scope)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("accountclient: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("accountclient: requesting access token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("accountclient: reading token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("accountclient: token endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("accountclient: decoding token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("accountclient: token response has no access_token")
	}

	s.token = tok.AccessToken
	s.expires = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return s.token, nil
}
