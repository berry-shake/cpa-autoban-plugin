package plugin

import (
	_ "embed"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed page.html
var statusPage []byte

// pageCSP only allows same-origin framing, so the CPA management panel can
// embed the page and share its remembered management key.
var pageCSP = strings.Join([]string{
	"default-src 'none'",
	"style-src 'unsafe-inline'",
	"script-src 'unsafe-inline'",
	"img-src 'self' data:",
	"connect-src 'self'",
	"base-uri 'none'",
	"form-action 'none'",
	"frame-ancestors 'self'",
}, "; ")

func pageResponse() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            []string{"text/html; charset=utf-8"},
			"Cache-Control":           []string{"no-store"},
			"X-Content-Type-Options":  []string{"nosniff"},
			"Content-Security-Policy": []string{pageCSP},
		},
		Body: append([]byte(nil), statusPage...),
	}
}
