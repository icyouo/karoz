package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type codexAuthFile struct {
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	Tokens       struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh"`
	Expired     string `json:"expired"`
	Email       string `json:"email"`
	AuthMode    string `json:"auth_mode"`
}

type codexCredential struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
	Expired      time.Time
	Path         string
	Raw          codexAuthFile
}

func resolveCodexCredential(ctx context.Context) (codexCredential, error) {
	path := expandHome(getenv("KAROZ_CODEX_AUTH_PATH", "~/.codex/auth.json"))
	credential, err := readCodexCredential(path)
	if err != nil {
		return codexCredential{}, err
	}
	if credential.AccessToken == "" && strings.TrimSpace(credential.Raw.OpenAIAPIKey) != "" {
		return codexCredential{}, errors.New("codex auth file has OPENAI_API_KEY but no OAuth access_token; codex-direct requires CLI OAuth login")
	}
	if credential.AccessToken == "" {
		return codexCredential{}, errors.New("codex OAuth access_token not found in " + path)
	}
	if credential.RefreshToken != "" && time.Until(credential.Expired) <= 5*time.Minute {
		if refreshed, err := refreshCodexCredential(ctx, credential); err == nil {
			return refreshed, nil
		} else if time.Now().After(credential.Expired) {
			return codexCredential{}, err
		}
	}
	return credential, nil
}

func readCodexCredential(path string) (codexCredential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return codexCredential{}, fmt.Errorf("read codex auth file: %w", err)
	}
	var raw codexAuthFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return codexCredential{}, fmt.Errorf("parse codex auth file: %w", err)
	}
	expired, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw.Expired))
	if expired.IsZero() {
		expired = time.Now().Add(time.Hour)
	}
	return codexCredential{
		AccessToken:  strings.TrimSpace(raw.Tokens.AccessToken),
		RefreshToken: strings.TrimSpace(raw.Tokens.RefreshToken),
		AccountID:    strings.TrimSpace(raw.Tokens.AccountID),
		Expired:      expired,
		Path:         path,
		Raw:          raw,
	}, nil
}

func refreshCodexCredential(ctx context.Context, current codexCredential) (codexCredential, error) {
	form := url.Values{
		"client_id":     {"app_EMoamEEZ73f0CkXaXp7hrann"},
		"grant_type":    {"refresh_token"},
		"refresh_token": {current.RefreshToken},
		"scope":         {"openid profile email"},
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://auth.openai.com/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return codexCredential{}, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return codexCredential{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return codexCredential{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return codexCredential{}, fmt.Errorf("codex token refresh status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return codexCredential{}, err
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		return codexCredential{}, errors.New("codex token refresh did not return access_token")
	}
	next := current.Raw
	next.Tokens.AccessToken = strings.TrimSpace(decoded.AccessToken)
	if strings.TrimSpace(decoded.RefreshToken) != "" {
		next.Tokens.RefreshToken = strings.TrimSpace(decoded.RefreshToken)
	}
	if strings.TrimSpace(decoded.IDToken) != "" {
		next.Tokens.IDToken = strings.TrimSpace(decoded.IDToken)
	}
	next.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	if decoded.ExpiresIn > 0 {
		next.Expired = time.Now().Add(time.Duration(decoded.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	if err := writeJSONFileAtomic(current.Path, next, 0600); err != nil {
		return codexCredential{}, err
	}
	return readCodexCredential(current.Path)
}
