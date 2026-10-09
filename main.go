package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

func init() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Lmicroseconds | log.Ltime)
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
