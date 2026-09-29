// Package secretguard detects common accidental secret exfiltration forms.
// It is deliberately a tripwire rather than a general DLP engine: encrypted,
// fragmented, and cross-request exfiltration are outside its guarantees.
package secretguard

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

const DefaultMaxBodyBytes int64 = 10 << 20

const maxSecrets = 1000

type Action string

const (
	ActionBlock Action = "block"
	ActionFlag  Action = "flag"
)

type AllowRule struct {
	Host      string   `json:"host"`
	Locations []string `json:"locations,omitempty"`
}

// SecretSpec contains the plaintext only at registration time. Guard keeps
// compiled patterns and never exposes the value through summaries or findings.
type SecretSpec struct {
	Name        string
	Value       string
	Allow       []AllowRule
	Action      Action
	EncodeDepth int
}

type Request struct {
	Host            string
	PathQuery       string
	Headers         http.Header
	Body            []byte
	ContentType     string
	ContentEncoding string
}

type Finding struct {
	Secret   string `json:"secret"`
	Encoding string `json:"encoding"`
	Location string `json:"location"`
	Offset   int    `json:"offset"`
	Host     string `json:"host"`
	Allowed  bool   `json:"allowed"`
}

type Result struct {
	Findings  []Finding
	Blocked   bool
	Flagged   bool
	Oversize  bool
	ScanError bool
	Reason    string
}

type Guard struct {
	mu             sync.RWMutex
	secrets        map[string]compiledSecret
	machine        *machine
	maxBodyBytes   int64
	oversizeAction Action
}

type compiledSecret struct {
	name   string
	allow  []AllowRule
	action Action
}

type pattern struct {
	bytes    []byte
	secret   string
	encoding string
}

func New() *Guard {
	return &Guard{
		secrets:        make(map[string]compiledSecret),
		machine:        buildMachine(nil),
		maxBodyBytes:   DefaultMaxBodyBytes,
		oversizeAction: ActionBlock,
	}
}

func (g *Guard) SetBodyPolicy(maxBytes int64, action Action) error {
	if maxBytes <= 0 {
		return errors.New("max body bytes must be positive")
	}
	if action != ActionBlock {
		return errors.New("oversize action must be block")
	}
	g.mu.Lock()
	g.maxBodyBytes, g.oversizeAction = maxBytes, action
	g.mu.Unlock()
	return nil
}

func (g *Guard) MaxBodyBytes() int64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.maxBodyBytes
}

func (g *Guard) Register(spec SecretSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return errors.New("secret name is required")
	}
	if spec.Value == "" {
		return errors.New("secret value is required")
	}
	if spec.Action == "" {
		spec.Action = ActionBlock
	}
	if spec.Action != ActionBlock && spec.Action != ActionFlag {
		return fmt.Errorf("invalid action %q", spec.Action)
	}
	if spec.EncodeDepth != 0 {
		return errors.New("encode depth 2 is not implemented yet")
	}
	if len(spec.Name) > 128 || strings.IndexFunc(spec.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return errors.New("invalid secret name")
	}
	allow := make([]AllowRule, len(spec.Allow))
	for i, rule := range spec.Allow {
		if strings.TrimSpace(rule.Host) == "" || len(rule.Host) > 253 {
			return errors.New("invalid allow host")
		}
		allow[i] = AllowRule{Host: rule.Host, Locations: append([]string(nil), rule.Locations...)}
	}
	patterns := makePatterns(spec.Name, []byte(spec.Value), spec.EncodeDepth)
	compiled := compiledSecret{name: spec.Name, allow: allow, action: spec.Action}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.secrets[spec.Name]; exists {
		return fmt.Errorf("secret %q already exists", spec.Name)
	}
	if len(g.secrets) >= maxSecrets {
		return errors.New("secret registry limit reached")
	}
	g.secrets[spec.Name] = compiled
	// Rebuild from the previous machine plus the newly registered patterns.
	all := append([]pattern(nil), g.machine.patterns...)
	all = append(all, patterns...)
	g.machine = buildMachine(all)
	return nil
}

func (g *Guard) Remove(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.secrets[name]; !ok {
		return false
	}
	delete(g.secrets, name)
	patterns := make([]pattern, 0)
	for _, p := range g.machine.patterns {
		if p.secret != name {
			patterns = append(patterns, p)
		}
	}
	g.machine = buildMachine(patterns)
	return true
}

func (g *Guard) Names() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	names := make([]string, 0, len(g.secrets))
	for name := range g.secrets {
		names = append(names, name)
	}
	return names
}

func (g *Guard) Scan(req Request) Result {
	g.mu.RLock()
	machine, secrets := g.machine, cloneSecrets(g.secrets)
	maxBody, oversizeAction := g.maxBodyBytes, g.oversizeAction
	g.mu.RUnlock()
	result := Result{}
	if int64(len(req.Body)) > maxBody {
		result.Oversize = true
		result.Reason = "body_too_large"
		// An oversized body cannot be fully inspected. Never forward an
		// unscanned body, even if a caller attempted to configure flag mode.
		result.Blocked = true
		_ = oversizeAction
		return result
	}
	host := normalizeHost(req.Host)
	seen := make(map[string]bool)
	check := func(location string, data []byte) {
		if len(data) == 0 {
			return
		}
		hits, overflow := machine.scan(data, 1024)
		if overflow {
			result.ScanError = true
			result.Blocked = true
			result.Reason = "too_many_matches"
			return
		}
		for _, hit := range hits {
			key := hit.secret + "\x00" + location
			if seen[key] {
				continue
			}
			seen[key] = true
			secret := secrets[hit.secret]
			allowed := allowed(secret.allow, host, location)
			result.Findings = append(result.Findings, Finding{
				Secret: hit.secret, Encoding: hit.encoding, Location: location,
				Offset: hit.offset, Host: host, Allowed: allowed,
			})
			if !allowed {
				if secret.action == ActionFlag {
					result.Flagged = true
				} else {
					result.Blocked = true
				}
			}
		}
	}
	for _, view := range textViews([]byte(req.PathQuery), false, true) {
		check("url", view)
	}
	for name, values := range req.Headers {
		location := "header:" + http.CanonicalHeaderKey(name)
		for _, value := range values {
			for _, view := range textViews([]byte(value), false, false) {
				check(location, view)
			}
		}
	}
	views, scanErr := bodyViews(req.Body, req.ContentType, req.ContentEncoding, maxBody)
	if scanErr != nil {
		result.ScanError = true
		result.Blocked = true
		result.Reason = "body_decode_failed"
	}
	for _, view := range views {
		check("body", view)
	}
	return result
}

type hit struct {
	secret, encoding string
	offset           int
}

func textViews(data []byte, jsonMode, urlDecode bool) [][]byte {
	if len(data) == 0 {
		return nil
	}
	views := [][]byte{data}
	if urlDecode {
		if decoded, err := url.QueryUnescape(string(data)); err == nil && decoded != string(data) {
			views = append(views, []byte(decoded))
		}
	}
	if stripped := bytes.ReplaceAll(data, []byte("\r\n"), nil); !bytes.Equal(stripped, data) {
		views = append(views, stripped)
	}
	if jsonMode {
		var value any
		if json.Unmarshal(data, &value) == nil {
			var stringsFound []string
			collectJSONStrings(value, &stringsFound)
			if len(stringsFound) > 0 {
				views = append(views, []byte(strings.Join(stringsFound, "\n")))
			}
		}
	}
	return views
}

func bodyViews(body []byte, contentType, contentEncoding string, max int64) ([][]byte, error) {
	contentType = strings.ToLower(contentType)
	jsonMode := strings.HasPrefix(contentType, "application/json")
	formMode := strings.HasPrefix(contentType, "application/x-www-form-urlencoded")
	views := textViews(body, jsonMode, false)
	if decoded, err := decompress(body, contentEncoding, max); err == nil {
		views = append(views, textViews(decoded, jsonMode, false)...)
		if formMode {
			if unescaped, unescapeErr := url.QueryUnescape(string(decoded)); unescapeErr == nil {
				views = append(views, []byte(unescaped))
			}
		}
	} else if err != nil {
		return views, err
	}
	if formMode {
		if decoded, err := url.QueryUnescape(string(body)); err == nil {
			views = append(views, []byte(decoded))
		}
	}
	return views, nil
}

func decompress(body []byte, encoding string, max int64) ([]byte, error) {
	encoding = strings.ToLower(strings.TrimSpace(encoding))
	if encoding == "" || encoding == "identity" {
		return nil, nil
	}
	if strings.Contains(encoding, ",") {
		return nil, errors.New("multiple content encodings are not supported")
	}
	var reader io.ReadCloser
	switch encoding {
	case "gzip":
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		reader = gz
	case "deflate":
		if looksLikeZlib(body) {
			zlibReader, err := zlib.NewReader(bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			reader = zlibReader
		} else {
			reader = flate.NewReader(bytes.NewReader(body))
		}
	case "br":
		reader = io.NopCloser(brotli.NewReader(bytes.NewReader(body)))
	default:
		return nil, errors.New("unsupported content encoding")
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil || int64(len(decoded)) > max {
		if err == nil {
			err = errors.New("decompressed body exceeds limit")
		}
		return nil, err
	}
	return decoded, nil
}

func looksLikeZlib(body []byte) bool {
	if len(body) < 2 || body[0]&0x0f != 8 {
		return false
	}
	return (int(body[0])<<8|int(body[1]))%31 == 0
}

func collectJSONStrings(value any, out *[]string) {
	switch v := value.(type) {
	case string:
		*out = append(*out, v)
	case []any:
		for _, item := range v {
			collectJSONStrings(item, out)
		}
	case map[string]any:
		for key, item := range v {
			*out = append(*out, key)
			collectJSONStrings(item, out)
		}
	}
}

func cloneSecrets(src map[string]compiledSecret) map[string]compiledSecret {
	dst := make(map[string]compiledSecret, len(src))
	for name, secret := range src {
		secret.allow = cloneAllowRules(secret.allow)
		dst[name] = secret
	}
	return dst
}

func allowed(rules []AllowRule, host, location string) bool {
	for _, rule := range rules {
		if !hostMatches(rule.Host, host) {
			continue
		}
		if len(rule.Locations) == 0 {
			return true
		}
		for _, candidate := range rule.Locations {
			if strings.EqualFold(strings.TrimSpace(candidate), location) {
				return true
			}
		}
	}
	return false
}

func hostMatches(pattern, host string) bool {
	pattern, host = normalizeHost(pattern), normalizeHost(host)
	if pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := strings.TrimPrefix(pattern, "*.")
		return host != suffix && strings.HasSuffix(host, "."+suffix)
	}
	return false
}

func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.Trim(host, "[]")
	}
	return strings.TrimSuffix(strings.Trim(host, "[]"), ".")
}

func makePatterns(name string, secret []byte, depth int) []pattern {
	patterns := []pattern{{bytes: append([]byte(nil), secret...), secret: name, encoding: "raw"}}
	if len(secret) < 12 {
		return patterns
	}
	for _, urlSafe := range []bool{false, true} {
		encoding := "base64"
		if urlSafe {
			encoding = "base64url"
		}
		for p := 0; p < 3; p++ {
			variant := base64Variant(secret, p, urlSafe)
			if len(variant) > 0 {
				patterns = append(patterns, pattern{bytes: []byte(variant), secret: name, encoding: encoding})
			}
		}
	}
	patterns = append(patterns,
		pattern{bytes: []byte(hex.EncodeToString(secret)), secret: name, encoding: "hex_lower"},
		pattern{bytes: []byte(strings.ToUpper(hex.EncodeToString(secret))), secret: name, encoding: "hex_upper"},
	)
	_ = depth
	return patterns
}

func base64Variant(secret []byte, offset int, urlSafe bool) string {
	input := append(make([]byte, offset), secret...)
	enc := base64.StdEncoding.EncodeToString(input)
	if urlSafe {
		enc = base64.RawURLEncoding.EncodeToString(input)
	}
	groups := (len(input) / 3)
	if offset > 0 {
		groups--
		if groups <= 0 {
			return ""
		}
		if len(enc) < 4 {
			return ""
		}
		enc = enc[4:]
	}
	length := groups * 4
	if length > len(enc) {
		length = len(enc)
	}
	return strings.TrimRight(enc[:length], "=")
}

type machine struct {
	nodes    []machineNode
	patterns []pattern
	dense    [][256]int32
}

type machineNode struct {
	next   map[byte]int
	fail   int
	output []pattern
}

func buildMachine(patterns []pattern) *machine {
	m := &machine{nodes: []machineNode{{next: make(map[byte]int)}}, patterns: append([]pattern(nil), patterns...)}
	for _, p := range patterns {
		if len(p.bytes) == 0 {
			continue
		}
		state := 0
		for _, b := range p.bytes {
			next, ok := m.nodes[state].next[b]
			if !ok {
				next = len(m.nodes)
				m.nodes[state].next[b] = next
				m.nodes = append(m.nodes, machineNode{next: make(map[byte]int)})
			}
			state = next
		}
		m.nodes[state].output = append(m.nodes[state].output, p)
	}
	queue := make([]int, 0)
	for _, next := range m.nodes[0].next {
		queue = append(queue, next)
	}
	for head := 0; head < len(queue); head++ {
		state := queue[head]
		for b, next := range m.nodes[state].next {
			queue = append(queue, next)
			fallback := m.nodes[state].fail
			for fallback != 0 {
				if candidate, ok := m.nodes[fallback].next[b]; ok {
					fallback = candidate
					goto found
				}
				fallback = m.nodes[fallback].fail
			}
			if candidate, ok := m.nodes[0].next[b]; ok {
				fallback = candidate
			} else {
				fallback = 0
			}
		found:
			m.nodes[next].fail = fallback
			m.nodes[next].output = append(m.nodes[next].output, m.nodes[fallback].output...)
		}
	}
	// A dense goto table removes map lookups from the hot path for the normal
	// sized registry. Keep the sparse representation for unusually large
	// registries so registration cannot allocate unbounded memory.
	if len(patterns) >= 20 && len(m.nodes) <= 50000 {
		m.dense = make([][256]int32, len(m.nodes))
		order := append([]int{0}, queue...)
		for _, state := range order {
			for value := 0; value < 256; value++ {
				byteValue := byte(value)
				if next, ok := m.nodes[state].next[byteValue]; ok {
					m.dense[state][value] = int32(next)
				} else if state != 0 {
					m.dense[state][value] = m.dense[m.nodes[state].fail][value]
				}
			}
		}
	}
	return m
}

func (m *machine) scan(data []byte, maxHits int) ([]hit, bool) {
	var hits []hit
	state := 0
	for i, b := range data {
		if len(m.dense) > 0 {
			state = int(m.dense[state][b])
			for _, p := range m.nodes[state].output {
				if len(hits) >= maxHits {
					return hits, true
				}
				hits = append(hits, hit{secret: p.secret, encoding: p.encoding, offset: i - len(p.bytes) + 1})
			}
			continue
		}
		for state != 0 {
			if next, ok := m.nodes[state].next[b]; ok {
				state = next
				goto matched
			}
			state = m.nodes[state].fail
		}
		if next, ok := m.nodes[0].next[b]; ok {
			state = next
		} else {
			state = 0
		}
	matched:
		for _, p := range m.nodes[state].output {
			if len(hits) >= maxHits {
				return hits, true
			}
			hits = append(hits, hit{secret: p.secret, encoding: p.encoding, offset: i - len(p.bytes) + 1})
		}
	}
	return hits, false
}

func cloneAllowRules(src []AllowRule) []AllowRule {
	dst := make([]AllowRule, len(src))
	for i, rule := range src {
		dst[i] = AllowRule{Host: rule.Host, Locations: append([]string(nil), rule.Locations...)}
	}
	return dst
}
