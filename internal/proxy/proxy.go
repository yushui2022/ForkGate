package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yushui2022/ForkGate/internal/branches"
	"github.com/yushui2022/ForkGate/internal/mitm"
	"github.com/yushui2022/ForkGate/internal/secretguard"
	"github.com/yushui2022/ForkGate/internal/store"
)

type Proxy struct {
	store     *store.Store
	ca        *mitm.Authority
	transport *http.Transport
	guard     *secretguard.Guard
	branches  *branches.Manager
	mu        sync.Mutex
	tunnels   map[net.Conn]struct{}
	closed    bool
}

func (p *Proxy) SetGuard(guard *secretguard.Guard) {
	p.mu.Lock()
	p.guard = guard
	p.mu.Unlock()
}

func (p *Proxy) SetBranchManager(manager *branches.Manager) {
	p.mu.Lock()
	p.branches = manager
	p.mu.Unlock()
}

func New(s *store.Store, ca *mitm.Authority) *Proxy {
	return &Proxy{
		store: s,
		ca:    ca,
		transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		tunnels: make(map[net.Conn]struct{}),
	}
}

// Close terminates hijacked CONNECT tunnels and releases idle upstream
// connections. http.Server.Shutdown cannot see connections after Hijack.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	tunnels := make([]net.Conn, 0, len(p.tunnels))
	for conn := range p.tunnels {
		tunnels = append(tunnels, conn)
	}
	p.tunnels = make(map[net.Conn]struct{})
	p.mu.Unlock()
	for _, conn := range tunnels {
		_ = conn.Close()
	}
	p.transport.CloseIdleConnections()
	return nil
}

func (p *Proxy) trackTunnel(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.tunnels[conn] = struct{}{}
	return true
}

func (p *Proxy) untrackTunnel(conn net.Conn) {
	p.mu.Lock()
	delete(p.tunnels, conn)
	p.mu.Unlock()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	branch, release, err := p.acquireBranch(r)
	if err != nil {
		writeBranchAuthError(w, err)
		return
	}
	defer release()
	if r.URL == nil || r.URL.Host == "" || r.URL.Scheme != "http" || r.URL.User != nil {
		http.Error(w, "explicit proxy requires an absolute URL", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	r.RequestURI = ""
	stripHopHeaders(r.Header)
	r.Header.Del("Proxy-Authorization")
	result, err := p.inspect(r, host)
	if err != nil {
		http.Error(w, "request body unavailable", http.StatusBadRequest)
		return
	}
	if result.Blocked {
		p.recordSecretEvent(r.Context(), "secret.blocked", result)
		if result.Oversize {
			http.Error(w, `{"error":"forkgate_body_too_large"}`, http.StatusRequestEntityTooLarge)
		} else {
			writeSecretBlocked(w, result)
		}
		return
	}
	if result.Flagged {
		p.recordSecretEvent(r.Context(), "secret.flagged", result)
	}
	if branches.ShouldStage(branch, r.Method) {
		staged, err := p.stage(branch, r)
		if err != nil {
			if errors.Is(err, branches.ErrStagingUnavailable) {
				http.Error(w, `{"error":"staging_unavailable"}`, http.StatusServiceUnavailable)
			} else if errors.Is(err, branches.ErrBodyTooLarge) {
				http.Error(w, `{"error":"forkgate_body_too_large"}`, http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, `{"error":"staging_failed"}`, http.StatusInternalServerError)
			}
			return
		}
		writeStaged(w, staged.ID)
		return
	}
	resp, err := p.transport.RoundTrip(r)
	if err != nil {
		p.record(r.Context(), "request.error", map[string]any{"method": r.Method, "host": host, "path": path, "kind": "upstream_roundtrip"})
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	stripHopHeaders(resp.Header)
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	p.record(r.Context(), "request.forwarded", map[string]any{"method": r.Method, "host": host, "path": path, "status": resp.StatusCode})
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	branch, release, err := p.acquireBranch(r)
	if err != nil {
		writeBranchAuthError(w, err)
		return
	}
	defer release()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking is not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientRW, err := hijacker.Hijack()
	if err != nil {
		return
	}
	target := r.Host
	if target == "" {
		_ = clientConn.Close()
		return
	}
	if !p.trackTunnel(clientConn) {
		_ = clientConn.Close()
		return
	}
	defer p.untrackTunnel(clientConn)
	defer clientConn.Close()
	_, _ = clientRW.WriteString("HTTP/1.1 200 Connection Established\r\nProxy-Agent: ForkGate\r\n\r\n")
	if err := clientRW.Flush(); err != nil {
		_ = clientConn.Close()
		return
	}

	clientTLS := tls.Server(&bufferedConn{Conn: clientConn, reader: clientRW.Reader}, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host := hello.ServerName
			if host == "" {
				host = target
			}
			leaf, err := p.ca.Leaf(host)
			if err != nil {
				return nil, err
			}
			return &leaf, nil
		},
	})
	defer clientTLS.Close()
	_ = clientTLS.SetDeadline(time.Now().Add(10 * time.Second))
	if err := clientTLS.Handshake(); err != nil {
		p.record(r.Context(), "request.tls_error", map[string]any{"target": target, "kind": "client_handshake"})
		return
	}
	_ = clientTLS.SetDeadline(time.Time{})
	serverName := clientTLS.ConnectionState().ServerName
	if serverName == "" {
		serverName = hostOnly(target)
	}
	upstreamTarget := target
	if _, port, splitErr := net.SplitHostPort(target); splitErr == nil {
		upstreamTarget = net.JoinHostPort(serverName, port)
	}
	p.record(r.Context(), "request.connect", map[string]any{"target": target, "sni": serverName})

	reader := bufio.NewReader(clientTLS)
	for {
		_ = clientTLS.SetReadDeadline(time.Now().Add(60 * time.Second))
		inner, err := readRequestLimited(reader, 64<<10)
		if err != nil {
			if err != io.EOF {
				kind := "request_parse"
				if errors.Is(err, errRequestHeadersTooLarge) {
					kind = "request_headers_too_large"
				}
				p.record(r.Context(), "request.tls_error", map[string]any{"target": target, "kind": kind})
			}
			return
		}
		inner.URL.Scheme = "https"
		inner.URL.Host = upstreamTarget
		inner.Host = upstreamTarget
		inner.RequestURI = ""
		stripHopHeaders(inner.Header)
		inner.Header.Del("Proxy-Authorization")
		result, err := p.inspect(inner, hostOnly(upstreamTarget))
		if err != nil {
			writeTunnelError(clientTLS, http.StatusBadRequest, `{"error":"forkgate_body_unavailable"}`)
			return
		}
		if result.Blocked {
			p.recordSecretEvent(r.Context(), "secret.blocked", result)
			if result.Oversize {
				writeTunnelError(clientTLS, http.StatusRequestEntityTooLarge, `{"error":"forkgate_body_too_large"}`)
			} else {
				writeTunnelError(clientTLS, http.StatusForbidden, secretBlockedJSON(result))
			}
			return
		}
		if result.Flagged {
			p.recordSecretEvent(r.Context(), "secret.flagged", result)
		}
		if branches.ShouldStage(branch, inner.Method) {
			staged, err := p.stage(branch, inner)
			if err != nil {
				if errors.Is(err, branches.ErrStagingUnavailable) {
					writeTunnelError(clientTLS, http.StatusServiceUnavailable, `{"error":"staging_unavailable"}`)
				} else if errors.Is(err, branches.ErrBodyTooLarge) {
					writeTunnelError(clientTLS, http.StatusRequestEntityTooLarge, `{"error":"forkgate_body_too_large"}`)
				} else {
					writeTunnelError(clientTLS, http.StatusInternalServerError, `{"error":"staging_failed"}`)
				}
				return
			}
			writeTunnelStaged(clientTLS, staged.ID)
			return
		}
		resp, err := p.transport.RoundTrip(inner)
		if err != nil {
			innerPath := inner.URL.EscapedPath()
			if innerPath == "" {
				innerPath = "/"
			}
			p.record(r.Context(), "request.error", map[string]any{"method": inner.Method, "host": hostOnly(upstreamTarget), "path": innerPath, "kind": "upstream_roundtrip"})
			_, _ = fmt.Fprintf(clientTLS, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		innerPath := inner.URL.EscapedPath()
		if innerPath == "" {
			innerPath = "/"
		}
		p.record(r.Context(), "request.forwarded", map[string]any{"method": inner.Method, "host": hostOnly(upstreamTarget), "path": innerPath, "status": resp.StatusCode})
		// Keep the MITM lifecycle explicit: one inner HTTP/1.1 request per
		// CONNECT tunnel. A later phase can add long-lived tunnel ownership.
		resp.Close = true
		stripHopHeaders(resp.Header)
		resp.Header.Set("Connection", "close")
		_ = clientTLS.SetWriteDeadline(time.Now().Add(60 * time.Second))
		if err := resp.Write(clientTLS); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()
		return
	}
}

func (p *Proxy) acquireBranch(r *http.Request) (*store.BranchRecord, func(), error) {
	p.mu.Lock()
	manager := p.branches
	p.mu.Unlock()
	if manager == nil {
		return nil, func() {}, nil
	}
	token := parseBranchBearer(r.Header.Get("Proxy-Authorization"))
	branch, release, err := manager.Acquire(r.Context(), token)
	if err != nil {
		return &branch, release, err
	}
	return &branch, release, nil
}

func (p *Proxy) stage(branch *store.BranchRecord, r *http.Request) (store.StagedWrite, error) {
	p.mu.Lock()
	manager := p.branches
	p.mu.Unlock()
	if manager == nil || branch == nil {
		return store.StagedWrite{}, branches.ErrStagingUnavailable
	}
	return manager.Stage(r.Context(), *branch, r)
}

func writeStaged(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-ForkGate-Staged", id)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("{}"))
}

func writeTunnelStaged(conn net.Conn, id string) {
	body := "{}"
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 202 Accepted\r\nContent-Type: application/json\r\nX-ForkGate-Staged: %s\r\nContent-Length: 2\r\nConnection: close\r\n\r\n%s", id, body)
}

func parseBranchBearer(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func writeBranchAuthError(w http.ResponseWriter, err error) {
	status := http.StatusProxyAuthRequired
	payload := map[string]string{"error": "branch_token_required"}
	if errors.Is(err, branches.ErrBranchSealed) {
		status = http.StatusServiceUnavailable
		payload["error"] = "branch_sealed"
		w.Header().Set("Retry-After", "1")
	} else if errors.Is(err, branches.ErrBranchTerminal) {
		status = http.StatusGone
		payload["error"] = "branch_terminal"
	} else if !errors.Is(err, branches.ErrInvalidToken) {
		status = http.StatusServiceUnavailable
		payload["error"] = "branch_auth_unavailable"
	}
	if status == http.StatusProxyAuthRequired {
		w.Header().Set("Proxy-Authenticate", `Bearer realm="forkgate-branch"`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (p *Proxy) inspect(r *http.Request, host string) (secretguard.Result, error) {
	p.mu.Lock()
	guard := p.guard
	p.mu.Unlock()
	if guard == nil {
		return secretguard.Result{}, nil
	}
	limit := guard.MaxBodyBytes()
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
		_ = r.Body.Close()
		if err != nil {
			return secretguard.Result{}, err
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	r.Header.Del("Content-Length")
	headers := r.Header.Clone()
	for name, values := range r.Trailer {
		headers[name] = append(headers[name], values...)
	}
	// Phase 1 does not forward request trailers. They have been included in
	// the scan above, and dropping them prevents an unbounded late data path.
	r.Trailer = nil
	pathQuery := r.URL.EscapedPath()
	if pathQuery == "" {
		pathQuery = "/"
	}
	if r.URL.RawQuery != "" {
		pathQuery += "?" + r.URL.RawQuery
	}
	return guard.Scan(secretguard.Request{
		Host:            host,
		PathQuery:       pathQuery,
		Headers:         headers,
		Body:            body,
		ContentType:     r.Header.Get("Content-Type"),
		ContentEncoding: r.Header.Get("Content-Encoding"),
	}), nil
}

func writeSecretBlocked(w http.ResponseWriter, result secretguard.Result) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(secretBlockedJSON(result)))
}

func secretBlockedJSON(result secretguard.Result) string {
	secret := "unknown"
	for _, finding := range result.Findings {
		if !finding.Allowed {
			secret = finding.Secret
			break
		}
	}
	payload, _ := json.Marshal(map[string]string{"error": "forkgate_secret_blocked", "secret": secret})
	return string(payload)
}

func (p *Proxy) recordSecretEvent(ctx context.Context, eventType string, result secretguard.Result) {
	findings := make([]map[string]any, 0, len(result.Findings))
	for _, finding := range result.Findings {
		if finding.Allowed {
			continue
		}
		findings = append(findings, map[string]any{
			"secret": finding.Secret, "encoding": finding.Encoding,
			"location": finding.Location, "offset": finding.Offset,
			"host": finding.Host,
		})
	}
	payload := map[string]any{"findings": findings}
	if result.Reason != "" {
		payload["reason"] = result.Reason
	}
	p.record(ctx, eventType, payload)
}

func writeTunnelError(conn net.Conn, status int, body string) {
	response := &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         true,
	}
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Connection", "close")
	_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	_ = response.Write(conn)
}

var errRequestHeadersTooLarge = errors.New("request headers too large")

func readRequestLimited(reader *bufio.Reader, max int) (*http.Request, error) {
	header := make([]byte, 0, minInt(max, 4096))
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > max-len(header) {
			return nil, errRequestHeadersTooLarge
		}
		header = append(header, fragment...)
		if bytes.HasSuffix(header, []byte("\r\n\r\n")) {
			return http.ReadRequest(bufio.NewReader(io.MultiReader(bytes.NewReader(header), reader)))
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			return nil, err
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func stripHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			h.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

func (p *Proxy) record(ctx context.Context, eventType string, payload map[string]any) {
	// A hijacked HTTP request's context may be canceled as soon as the
	// connection leaves net/http's ownership. Event persistence is a local
	// audit operation, so it must not depend on that request lifetime.
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}
	if err := p.store.RecordEvent(ctx, "", "", eventType, payload); err != nil {
		log.Printf("record event: %v", err)
	}
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func hostOnly(target string) string {
	if host, _, err := net.SplitHostPort(target); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(target, "[]")
}
