package publish

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// TraceURL answers "ip=<the caller's address>" among other lines. It is
// Cloudflare's, reached by address so detecting the IP needs no DNS, and
// over IPv4 because the A record is what it is for.
var TraceURL = "https://1.1.1.1/cdn-cgi/trace"

// DetectPublicIP asks the edge which address this machine comes from.
func DetectPublicIP(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, TraceURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "ip="); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", errors.New("the trace did not say the address")
}
