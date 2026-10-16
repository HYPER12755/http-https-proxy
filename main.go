package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	proxyUser    string
	proxyPass    string
	allowedPorts map[string]bool
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func randomSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("failed to generate secret: %v", err)
	}
	return hex.EncodeToString(b)
}

func credentialsPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "proxy.credentials")
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "proxy.credentials")
	}
	return "proxy.credentials"
}

func setupAuth() {
	proxyUser = envOr("PROXY_USER", "proxy")
	proxyPass = os.Getenv("PROXY_PASS")
	credPath := credentialsPath()

	if proxyPass == "" {
		if data, err := os.ReadFile(credPath); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				k, v, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				switch strings.TrimSpace(k) {
				case "PROXY_USER":
					if os.Getenv("PROXY_USER") == "" {
						proxyUser = strings.TrimSpace(v)
					}
				case "PROXY_PASS":
					proxyPass = strings.TrimSpace(v)
				}
			}
		}
		if proxyPass != "" {
			log.Printf("proxy auth enabled: user=%s (credentials loaded from %s)", proxyUser, credPath)
			return
		}
	}

	if proxyPass != "" {
		log.Printf("proxy auth enabled: user=%s (from PROXY_PASS)", proxyUser)
		return
	}

	proxyPass = randomSecret(16)
	content := fmt.Sprintf("PROXY_USER=%s\nPROXY_PASS=%s\n", proxyUser, proxyPass)
	if err := os.WriteFile(credPath, []byte(content), 0600); err == nil {
		log.Printf("proxy auth enabled: user=%s (new password generated and saved to %s)", proxyUser, credPath)
	} else {
		log.Printf("proxy auth enabled: user=%s pass=%s (WARNING: could not persist %s: %v)", proxyUser, proxyPass, credPath, err)
	}
}

func init() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Lmicroseconds | log.Ltime)
	setupAuth()
	allowedPorts = map[string]bool{}
	for _, p := range strings.Split(envOr("ALLOW_PORTS", "80,443"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			allowedPorts[p] = true
		}
	}
	if len(allowedPorts) > 0 {
		ports := make([]string, 0, len(allowedPorts))
		for p := range allowedPorts {
			ports = append(ports, p)
		}
		log.Printf("proxy port allowlist enabled: %v (set ALLOW_PORTS='' to allow all)", ports)
	}
}

func portAllowed(target string) bool {
	if len(allowedPorts) == 0 {
		return true
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "80")
	}
	_, port, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	return allowedPorts[port]
}

func checkAuth(r *http.Request) bool {
	if r.URL.Path == "/health" {
		return true
	}
	const prefix = "Basic "
	auth := r.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, prefix))
	if err != nil {
		return false
	}
	pair := bytes.SplitN(payload, []byte(":"), 2)
	if len(pair) != 2 {
		return false
	}
	sumUser := sha256.Sum256([]byte(proxyUser))
	sumPass := sha256.Sum256([]byte(proxyPass))
	gotUser := sha256.Sum256(pair[0])
	gotPass := sha256.Sum256(pair[1])
	userOK := subtle.ConstantTimeCompare(sumUser[:], gotUser[:]) == 1
	passOK := subtle.ConstantTimeCompare(sumPass[:], gotPass[:]) == 1
	return userOK && passOK
}

type statusWriter struct {
	http.ResponseWriter
	status int
	size   int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}
	n, err := sw.ResponseWriter.Write(b)
	sw.size += n
	return n, err
}

func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := sw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

func (sw *statusWriter) Flush() {
	if fl, ok := sw.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func durMs(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")
	if target != "" {
		u := mustParseURL(target)
		if u != nil {
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.URL.Path = u.Path
			r.Host = u.Host
		}
	}

	if r.URL.Scheme == "" {
		if strings.HasPrefix(strings.ToLower(r.Host), "https") {
			r.URL.Scheme = "https"
		} else {
			r.URL.Scheme = "http"
		}
	}

	reverseProxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.Host = r.URL.Host
			req.URL.Scheme = r.URL.Scheme
			req.URL.Host = r.URL.Host
		},
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: (&net.Dialer{
				Timeout: 15 * time.Second,
			}).DialContext,
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			log.Printf(`request from=%s method=%s url=%s host=%s status=502 err=%q duration_ms=%.3f`,
				r.RemoteAddr, r.Method, r.URL, r.Host, err.Error(), durMs(time.Now()))
			http.Error(w, "Bad Gateway: "+err.Error(), http.StatusBadGateway)
		},
	}
	reverseProxy.ServeHTTP(w, r)
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	dest, err := net.DialTimeout("tcp", r.URL.Host, 15*time.Second)
	if err != nil {
		log.Printf(`request from=%s method=CONNECT target=%s status=502 err=%q duration_ms=%.3f`,
			r.RemoteAddr, r.URL.Host, err.Error(), durMs(start))
		http.Error(w, "Bad Gateway: "+err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		log.Printf(`request from=%s method=CONNECT target=%s status=500 err="hijack unsupported" duration_ms=%.3f`,
			r.RemoteAddr, r.URL.Host, durMs(start))
		http.Error(w, "Hijack not supported", http.StatusInternalServerError)
		dest.Close()
		return
	}

	clientConn, _, err := hj.Hijack()
	if err != nil {
		log.Printf(`request from=%s method=CONNECT target=%s status=500 err=%q duration_ms=%.3f`,
			r.RemoteAddr, r.URL.Host, err.Error(), durMs(start))
		http.Error(w, "Hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		log.Printf(`request from=%s method=CONNECT target=%s status=200 err=%q duration_ms=%.3f`,
			r.RemoteAddr, r.URL.Host, err.Error(), durMs(start))
		clientConn.Close()
		dest.Close()
		return
	}

	var wg sync.WaitGroup
	var tx, rx int64
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(dest, clientConn)
		tx = n
		dest.Close()
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(clientConn, dest)
		rx = n
		clientConn.Close()
	}()
	wg.Wait()

	total := tx + rx
	ms := durMs(start)
	var speed float64
	if ms > 0 {
		speed = float64(total) / ms
	}
	log.Printf(`request from=%s method=CONNECT target=%s status=200 bytes=%d tx=%d rx=%d speed_bytes_per_ms=%.3f duration_ms=%.3f`,
		r.RemoteAddr, r.URL.Host, total, tx, rx, speed, ms)
}

func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return u
}

func dispatch(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health":
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	case r.Method == http.MethodConnect:
		handleConnect(w, r)
	default:
		handleRequest(w, r)
	}
}

func main() {
	srv := &http.Server{
		Addr:    ":1275",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			if !checkAuth(r) {
				log.Printf(`request from=%s method=%s url=%s status=407 err="proxy authentication required"`,
					r.RemoteAddr, r.Method, r.URL)
				w.Header().Set("Proxy-Authenticate", `Basic realm="proxy"`)
				http.Error(w, "Proxy authentication required", http.StatusProxyAuthRequired)
				return
			}
			target := r.URL.Host
			if target == "" {
				target = r.Host
			}
			if r.URL.Path != "/health" && !portAllowed(target) {
				log.Printf(`request from=%s method=%s target=%s status=403 err="destination port not allowed by proxy policy"`,
					r.RemoteAddr, r.Method, target)
				http.Error(w, "Forbidden: destination port not allowed", http.StatusForbidden)
				return
			}
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			dispatch(sw, r)
			if r.Method != http.MethodConnect {
				ms := durMs(start)
				var speed float64
				if ms > 0 {
					speed = float64(sw.size) / ms
				}
				log.Printf(`request from=%s method=%s scheme=%s host=%s url=%s status=%d bytes=%d speed_bytes_per_ms=%.3f duration_ms=%.3f`,
					r.RemoteAddr, r.Method, r.URL.Scheme, r.Host, r.URL, sw.status, sw.size, speed, ms)
			}
		}),
		ReadHeaderTimeout: 30 * time.Second,
	}

	log.Println("forward proxy listening on :1275 (HTTP + HTTPS/CONNECT)")
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
