package main

import (
	"context"
	"fmt"
	"net/http"

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
