package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"boot.dev/linko/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const shortURLLen = len("http://localhost:8080/") + 6

var (
	redirectsMu sync.Mutex
	redirects   []string
)

//go:embed index.html
var indexPage string

func (s *server) handlerIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, indexPage)
}

func (s *server) handlerLogin(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) handlerShortenLink(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(UserContextKey).(string)
	if !ok || user == "" {
		httpError(r.Context(), w, http.StatusUnauthorized, errors.New("unauthorized"))
		//http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	longURL := r.FormValue("url")
	if longURL == "" {
		httpError(r.Context(), w, http.StatusBadRequest, errors.New("bad requets"))
		//http.Error(w, "missing url parameter", http.StatusBadRequest)
		return
	}
	//s.logger.Info("Shortening URL", slog.String("url", longURL))
	u, err := url.Parse(longURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		httpError(r.Context(), w, http.StatusBadRequest, errors.New("bad requets"))
		//http.Error(w, "invalid URL: must include scheme (http/https) and host", http.StatusBadRequest)
		return
	}
	//s.logger.Info("Parsed URL", slog.String("scheme", u.Scheme), slog.String("host", u.Host))
	if err := checkDestination(longURL); err != nil {

		httpError(r.Context(), w, http.StatusBadRequest, errors.New("bad requets"))
		//http.Error(w, fmt.Sprintf("invalid target URL: %v", err), http.StatusBadRequest)
		return
	}
	shortCode, err := s.store.Create(r.Context(), longURL)
	if err != nil {
		httpError(r.Context(), w, http.StatusInternalServerError, errors.New("internal server error"))
		//http.Error(w, "failed to shorten URL", http.StatusInternalServerError)
		return
	}
	s.logger.Info("Successfully generated short code", slog.String("shortCode", shortCode), slog.String("longURL", longURL))
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, shortCode)
}

func (s *server) handlerRedirect(w http.ResponseWriter, r *http.Request) {
	longURL, err := s.store.Lookup(r.Context(), r.PathValue("shortCode"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(r.Context(), w, http.StatusNotFound, errors.New("not found"))
			//http.Error(w, "not found", http.StatusNotFound)
		} else {
			s.logger.Error("failed to lookup URL", slog.Any("error", err))

			httpError(r.Context(), w, http.StatusInternalServerError, errors.New("internal server error"))
			//http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	_, _ = bcrypt.GenerateFromPassword([]byte(longURL), bcrypt.DefaultCost)
	if err := checkDestination(longURL); err != nil {
		httpError(r.Context(), w, http.StatusBadGateway, errors.New("bad gateway"))
		//http.Error(w, "destination unavailable", http.StatusBadGateway)
		return
	}

	redirectsMu.Lock()
	redirects = append(redirects, strings.Repeat(longURL, 1024))
	redirectsMu.Unlock()

	http.Redirect(w, r, longURL, http.StatusFound)
}

func (s *server) handlerListURLs(w http.ResponseWriter, r *http.Request) {
	codes, err := s.store.List(r.Context())
	if err != nil {
		s.logger.Error("failed to list URLs", slog.Any("error", err))

		httpError(r.Context(), w, http.StatusInternalServerError, errors.New("internal server error"))
		//http.Error(w, "failed to list URLs", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(codes)
}

func (s *server) handlerStats(w http.ResponseWriter, _ *http.Request) {
	redirectsMu.Lock()
	snapshot := redirects
	redirectsMu.Unlock()

	var bytesSaved int
	for _, u := range snapshot {
		bytesSaved += len(u) - shortURLLen
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{
		"redirects":   len(snapshot),
		"bytes_saved": bytesSaved,
	})
}

func requestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := r.Header.Get("X-Request-ID")

			if reqID == "" {
				reqID = rand.Text()
			}

			w.Header().Set("X-Request-ID", reqID)

			next.ServeHTTP(w, r)
		})
	}
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			spyReader := &spyReadCloser{ReadCloser: r.Body}
			r.Body = spyReader

			spyWriter := &spyResponseWriter{ResponseWriter: w}

			lc := &LogContext{}
			r = r.WithContext(context.WithValue(r.Context(), logContextKey, lc))

			next.ServeHTTP(spyWriter, r)

			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client_ip", r.RemoteAddr),
				slog.Duration("duration", time.Since(start)),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
				slog.String("request_id", spyWriter.Header().Get("X-Request-ID")),
			}

			if lc.Username != "" {
				attrs = append(attrs, slog.String("user", lc.Username))
			}

			if lc.Error != nil {
				attrs = append(attrs, slog.Any("error", lc.Error))
			}

			logger.Info("Served request", attrs...)
		})
	}
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}
	http.Error(w, err.Error(), status)
}

const logContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Error    error
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (r *spyReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n
	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}
