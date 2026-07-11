package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

func webSearchTool(ctx context.Context, args map[string]any) string {
	query := toolStringArg(args, "query", 500)
	if query == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "query is required"})
	}
	limit := clampToolInt(args, "limit", 5, 1, 10)
	results, err := webSearch(ctx, query, limit)
	if err != nil {
		return toolJSON(map[string]any{"error": "web_search_failed", "message": err.Error(), "query": query})
	}
	return toolJSON(map[string]any{"query": query, "results": results})
}

func webFetchTool(ctx context.Context, args map[string]any) string {
	rawURL := toolStringArg(args, "url", 4000)
	if rawURL == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "url is required"})
	}
	maxChars := clampToolInt(args, "max_chars", 12000, 1000, 50000)
	page, err := webFetch(ctx, rawURL, maxChars)
	if err != nil {
		return toolJSON(map[string]any{"error": "web_fetch_failed", "message": err.Error(), "url": rawURL})
	}
	return toolJSON(page)
}

func webSearch(ctx context.Context, query string, limit int) ([]WebSearchResult, error) {
	endpoint := strings.TrimSpace(getenv("KAROZ_WEB_SEARCH_ENDPOINT", ""))
	var searchURL string
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		values := parsed.Query()
		values.Set("q", query)
		parsed.RawQuery = values.Encode()
		searchURL = parsed.String()
	} else {
		values := url.Values{"q": {query}}
		searchURL = "https://html.duckduckgo.com/html/?" + values.Encode()
	}
	body, finalURL, err := fetchURL(ctx, searchURL, 2<<20)
	if err != nil {
		return nil, err
	}
	results := parseSearchResults(string(body), finalURL, limit)
	if len(results) == 0 {
		return nil, errors.New("no search results parsed")
	}
	return results, nil
}

func webFetch(ctx context.Context, rawURL string, maxChars int) (map[string]any, error) {
	body, finalURL, err := fetchURL(ctx, rawURL, 4<<20)
	if err != nil {
		return nil, err
	}
	content := string(body)
	title := extractHTMLTitle(content)
	text := extractReadableText(content)
	text, truncated := truncateString(text, maxChars)
	return map[string]any{
		"url":       finalURL,
		"title":     title,
		"text":      text,
		"truncated": truncated,
	}, nil
}

func fetchURL(ctx context.Context, rawURL string, maxBytes int64) ([]byte, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", errors.New("only http and https URLs are supported")
	}
	if parsed.Host == "" {
		return nil, "", errors.New("url host is required")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Karoz/0.1 (+https://local.karoz)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > maxBytes {
		body = body[:maxBytes]
	}
	return body, resp.Request.URL.String(), nil
}

var (
	duckResultRE  = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	genericLinkRE = regexp.MustCompile(`(?is)<a[^>]+href="(https?://[^"]+)"[^>]*>(.*?)</a>`)
	snippetRE     = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>|<div[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</div>`)
	titleRE       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	scriptRE      = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>|<noscript[^>]*>.*?</noscript>`)
	tagRE         = regexp.MustCompile(`(?is)<[^>]+>`)
	spaceRE       = regexp.MustCompile(`[ \t\r\n]+`)
)

func parseSearchResults(markup, baseURL string, limit int) []WebSearchResult {
	snippets := extractSearchSnippets(markup)
	var results []WebSearchResult
	for _, match := range duckResultRE.FindAllStringSubmatch(markup, -1) {
		if len(results) >= limit {
			break
		}
		link := normalizeSearchURL(match[1], baseURL)
		title := cleanHTMLText(match[2])
		if link == "" || title == "" || hasResultURL(results, link) {
			continue
		}
		snippet := ""
		if len(snippets) > len(results) {
			snippet = snippets[len(results)]
		}
		results = append(results, WebSearchResult{Title: title, URL: link, Snippet: snippet})
	}
	if len(results) > 0 {
		return results
	}
	for _, match := range genericLinkRE.FindAllStringSubmatch(markup, -1) {
		if len(results) >= limit {
			break
		}
		link := normalizeSearchURL(match[1], baseURL)
		title := cleanHTMLText(match[2])
		if link == "" || title == "" || hasResultURL(results, link) {
			continue
		}
		results = append(results, WebSearchResult{Title: title, URL: link})
	}
	return results
}

func extractSearchSnippets(markup string) []string {
	var snippets []string
	for _, match := range snippetRE.FindAllStringSubmatch(markup, -1) {
		for i := 1; i < len(match); i++ {
			if strings.TrimSpace(match[i]) != "" {
				snippets = append(snippets, cleanHTMLText(match[i]))
				break
			}
		}
	}
	return snippets
}

func normalizeSearchURL(raw, base string) string {
	raw = html.UnescapeString(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !parsed.IsAbs() && base != "" {
		if baseParsed, err := url.Parse(base); err == nil {
			parsed = baseParsed.ResolveReference(parsed)
		}
	}
	if strings.HasPrefix(parsed.Path, "/l/") {
		if target := parsed.Query().Get("uddg"); target != "" {
			return target
		}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.String()
}

func hasResultURL(results []WebSearchResult, url string) bool {
	for _, result := range results {
		if result.URL == url {
			return true
		}
	}
	return false
}

func extractHTMLTitle(markup string) string {
	match := titleRE.FindStringSubmatch(markup)
	if len(match) < 2 {
		return ""
	}
	return cleanHTMLText(match[1])
}

func extractReadableText(markup string) string {
	text := scriptRE.ReplaceAllString(markup, " ")
	text = strings.ReplaceAll(text, "</p>", "\n")
	text = strings.ReplaceAll(text, "</div>", "\n")
	text = strings.ReplaceAll(text, "</li>", "\n")
	text = strings.ReplaceAll(text, "<br>", "\n")
	text = strings.ReplaceAll(text, "<br/>", "\n")
	text = tagRE.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(spaceRE.ReplaceAllString(line, " "))
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func cleanHTMLText(value string) string {
	value = tagRE.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	value = strings.TrimSpace(spaceRE.ReplaceAllString(value, " "))
	return value
}
