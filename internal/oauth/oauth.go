package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// UserInfo is the normalized result from any provider.
type UserInfo struct {
	Provider   string
	ProviderID string
	Email      string
	Name       string
	AvatarURL  string
}

// Provider abstracts an OAuth2 authorization code flow.
type Provider interface {
	Name() string
	AuthURL(state, redirectURI string) string
	Exchange(ctx context.Context, code, redirectURI string) (*UserInfo, error)
}

// Registry holds configured providers.
type Registry struct {
	providers map[string]Provider
}

func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

func (r *Registry) Register(p Provider)              { r.providers[p.Name()] = p }
func (r *Registry) Get(name string) (Provider, bool) { p, ok := r.providers[name]; return p, ok }

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	return out
}

// ─── GitHub ───

type GitHub struct{ ClientID, ClientSecret string }

func (g *GitHub) Name() string { return "github" }

func (g *GitHub) AuthURL(state, redirect string) string {
	return "https://github.com/login/oauth/authorize?" + url.Values{
		"client_id": {g.ClientID}, "redirect_uri": {redirect},
		"scope": {"user:email"}, "state": {state},
	}.Encode()
}

func (g *GitHub) Exchange(ctx context.Context, code, redirect string) (*UserInfo, error) {
	tok, err := postForm(ctx, "https://github.com/login/oauth/access_token", url.Values{
		"client_id": {g.ClientID}, "client_secret": {g.ClientSecret},
		"code": {code}, "redirect_uri": {redirect},
	})
	if err != nil {
		return nil, fmt.Errorf("github token: %w", err)
	}

	var user struct {
		ID        int    `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
		Email     string `json:"email"`
	}
	if err := apiGet(ctx, "https://api.github.com/user", tok, &user); err != nil {
		return nil, err
	}

	email := user.Email
	if email == "" {
		var emails []struct {
			Email    string `json:"email"`
			Primary  bool   `json:"primary"`
			Verified bool   `json:"verified"`
		}
		if err := apiGet(ctx, "https://api.github.com/user/emails", tok, &emails); err == nil {
			for _, e := range emails {
				if e.Primary && e.Verified {
					email = e.Email
					break
				}
			}
		}
	}

	name := user.Name
	if name == "" {
		name = user.Login
	}
	return &UserInfo{
		Provider: "github", ProviderID: fmt.Sprintf("%d", user.ID),
		Email: email, Name: name, AvatarURL: user.AvatarURL,
	}, nil
}

// ─── Google ───

type Google struct{ ClientID, ClientSecret string }

func (g *Google) Name() string { return "google" }

func (g *Google) AuthURL(state, redirect string) string {
	return "https://accounts.google.com/o/oauth2/v2/auth?" + url.Values{
		"client_id": {g.ClientID}, "redirect_uri": {redirect},
		"response_type": {"code"}, "scope": {"openid email profile"},
		"state": {state}, "access_type": {"offline"},
	}.Encode()
}

func (g *Google) Exchange(ctx context.Context, code, redirect string) (*UserInfo, error) {
	tok, err := postForm(ctx, "https://oauth2.googleapis.com/token", url.Values{
		"client_id": {g.ClientID}, "client_secret": {g.ClientSecret},
		"code": {code}, "grant_type": {"authorization_code"}, "redirect_uri": {redirect},
	})
	if err != nil {
		return nil, fmt.Errorf("google token: %w", err)
	}

	var user struct {
		ID      string `json:"id"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
	}
	if err := apiGet(ctx, "https://www.googleapis.com/oauth2/v2/userinfo", tok, &user); err != nil {
		return nil, err
	}

	return &UserInfo{
		Provider: "google", ProviderID: user.ID,
		Email: user.Email, Name: user.Name, AvatarURL: user.Picture,
	}, nil
}

// ─── Helpers ───

func postForm(ctx context.Context, endpoint string, form url.Values) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Error != "" {
		return "", fmt.Errorf("oauth error: %s", result.Error)
	}
	return result.AccessToken, nil
}

func apiGet(ctx context.Context, endpoint, token string, dest any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("api request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return json.Unmarshal(body, dest)
}
