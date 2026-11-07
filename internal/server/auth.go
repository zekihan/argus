package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"net/http"
	"os"
	"strings"
)

// authorize reads the token on every request so projected Secret updates take
// effect without restart.
// No credential or file contents are returned or logged, including on failure.
func authorize(r *http.Request, tokenFile string) int {
	file, err := os.Open(tokenFile)
	if err != nil {
		return http.StatusServiceUnavailable
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return http.StatusServiceUnavailable
	}
	token := strings.TrimSuffix(string(data), "\n")
	if len(token) < 32 {
		return http.StatusServiceUnavailable
	}
	for _, ch := range token {
		if ch < 33 || ch > 126 {
			return http.StatusServiceUnavailable
		}
	}
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 || !strings.HasPrefix(headers[0], "Bearer ") {
		return http.StatusUnauthorized
	}
	expected := sha256.Sum256([]byte(token))
	actual := sha256.Sum256([]byte(strings.TrimPrefix(headers[0], "Bearer ")))
	if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
		return http.StatusUnauthorized
	}
	return http.StatusOK
}
