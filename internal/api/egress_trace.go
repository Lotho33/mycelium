package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// traceIP queries Cloudflare's trace endpoint through client and returns
// the public IP and country the request left from: the reliable way to know
// whether a tunnel really routes traffic.
func traceIP(client *http.Client) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://1.1.1.1/cdn-cgi/trace", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if out["ip"] == "" {
		return nil, fmt.Errorf("risposta inattesa dal servizio di trace")
	}
	return out, nil
}
