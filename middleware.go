package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// requestIDMiddleware adds a unique request ID to each request context
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := uuid.New().String()
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		w.Header().Set("X-Request-ID", requestID)
		logInfo(requestID, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestSizeLimitMiddleware limits the size of request bodies
func requestSizeLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1048576) // 1MB limit
		next.ServeHTTP(w, r)
	})
}

// isLocalhost checks if the request is from localhost
func isLocalhost(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	// Check for localhost addresses
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// bearerAuthMiddleware validates bearer token authentication
// Allows requests from localhost to bypass authentication
func bearerAuthMiddleware(allowedTokens []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Allow localhost requests without authentication
			if isLocalhost(r) {
				next.ServeHTTP(w, r)
				return
			}

			// If no tokens configured, allow all requests (backward compatibility)
			if len(allowedTokens) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			// Extract Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, `{"status":"error","message":"Missing Authorization header"}`, http.StatusUnauthorized)
				return
			}

			// Check for Bearer prefix
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, `{"status":"error","message":"Invalid Authorization header format. Expected: Bearer <token>"}`, http.StatusUnauthorized)
				return
			}

			token := parts[1]

			// Validate token against allowed tokens
			validToken := false
			for _, allowedToken := range allowedTokens {
				if token == allowedToken {
					validToken = true
					break
				}
			}

			if !validToken {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, `{"status":"error","message":"Invalid authentication token"}`, http.StatusUnauthorized)
				return
			}

			// Token is valid, proceed
			next.ServeHTTP(w, r)
		})
	}
}
