package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// provider is an MCP server behind its own OAuth authorization server: metadata, client
// registration, an authorize page that approves at once, and a token endpoint that checks
// PKCE and counts renewals. Access tokens it issued are the only ones the MCP endpoint takes.
type provider struct {
	*httptest.Server
	mu         sync.Mutex
	challenges map[string]string
	issued     map[string]bool
	renewals   int
	// lifetime is how long a token from a sign-in lasts, in seconds; a renewed one lasts an hour.
	lifetime int
	register bool
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{challenges: map[string]string{}, issued: map[string]bool{}, lifetime: 3600, register: true}
	tools := mcp.NewServer(&mcp.Implementation{Name: "guarded", Version: "1"}, nil)
	mcp.AddTool(tools, &mcp.Tool{Name: "whoami"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	guarded := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return tools }, nil)
	mux := http.NewServeMux()
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	reply := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		ok := p.issued[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		p.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+p.URL+`/.well-known/oauth-protected-resource"`)
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		guarded.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"resource": p.URL + "/mcp", "authorization_servers": []string{p.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		meta := map[string]any{"issuer": p.URL, "authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
			"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}}
		if p.register {
			meta["registration_endpoint"] = p.URL + "/register"
		}
		reply(w, meta)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var asked map[string]any
		_ = json.NewDecoder(r.Body).Decode(&asked)
		asked["client_id"] = "registered-client"
		w.WriteHeader(http.StatusCreated)
		reply(w, asked)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("resource") != p.URL+"/mcp" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		code := fmt.Sprintf("code-%d", len(q.Get("state")))
		p.mu.Lock()
		p.challenges[code] = q.Get("code_challenge")
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		defer p.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if p.challenges[r.Form.Get("code")] != base64.RawURLEncoding.EncodeToString(sum[:]) {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-1" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			p.renewals++
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		lifetime := 3600
		if r.Form.Get("grant_type") == "authorization_code" {
			lifetime = p.lifetime
		}
		access := fmt.Sprintf("access-%d", len(p.issued)+1)
		p.issued[access] = true
		reply(w, map[string]any{"access_token": access, "token_type": "Bearer", "refresh_token": "refresh-1", "expires_in": lifetime})
	})
	return p
}

// browse stands in for the person's browser: it opens the page and follows it back to tap.
func browse(page string) error {
	resp, err := http.Get(page)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

func options(t *testing.T, p *provider) Options {
	return Options{ConfigPath: filepath.Join(t.TempDir(), "servers.json"), Name: "guarded", Endpoint: p.URL + "/mcp", Open: browse, Say: io.Discard, Version: "test"}
}

// TestAuthorizeSavesASignInOnlyItsOwnerCanRead pins the whole sign-in: discovery,
// registration, PKCE, the browser's return, and a private file that later connections use.
func TestAuthorizeSavesASignInOnlyItsOwnerCanRead(t *testing.T) {
	p := newProvider(t)
	o := options(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := Authorize(ctx, o)
	if err != nil || !result.SignedIn || result.Tools != 1 {
		t.Fatalf("Authorize = %+v, %v; want a sign-in and one tool", result, err)
	}
	st, err := os.Stat(Path(o.ConfigPath))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("the sign-in file is %v, %v; want mode 0600", st, err)
	}
	token, err := Handler(o.ConfigPath, o.Name, o.Endpoint).(*handler).source.Token()
	if err != nil || !p.issued[token.AccessToken] {
		t.Fatalf("a later connection got %v, %v; want the token the provider issued", token, err)
	}
	again, err := Authorize(ctx, o)
	if err != nil || again.SignedIn {
		t.Fatalf("a second Authorize = %+v, %v; want it to reuse the saved sign-in", again, err)
	}
}

// TestExpiredTokenIsRenewedOnceAndSaved pins renewal: an expired token is exchanged with the
// refresh token, and the new one is written back for other processes to find.
func TestExpiredTokenIsRenewedOnceAndSaved(t *testing.T) {
	p := newProvider(t)
	p.lifetime = 1
	o := options(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := Authorize(ctx, o); err != nil {
		t.Fatal(err)
	}
	first := Handler(o.ConfigPath, o.Name, o.Endpoint).(*handler).source
	second := Handler(o.ConfigPath, o.Name, o.Endpoint).(*handler).source
	a, err := first.Token()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Token()
	if err != nil || a.AccessToken != b.AccessToken || p.renewals != 1 {
		t.Fatalf("two processes got %q and %q after %d renewals, %v; want one renewal shared through the file", a.AccessToken, b.AccessToken, p.renewals, err)
	}
}

// TestHandlerNamesTheCommandThatSignsIn pins what a connection without a sign-in reports.
func TestHandlerNamesTheCommandThatSignsIn(t *testing.T) {
	p := newProvider(t)
	o := options(t, p)
	_, err := mcp.NewClient(&mcp.Implementation{Name: "tap", Version: "test"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: o.Endpoint, OAuthHandler: Handler(o.ConfigPath, o.Name, o.Endpoint), MaxRetries: -1}, nil)
	var need *Required
	if !errors.As(err, &need) || !strings.Contains(need.Error(), `tap auth guarded`) {
		t.Fatalf("Connect = %v; want an error naming \"tap auth guarded\"", err)
	}
}

// TestAuthorizeExplainsAProviderThatWillNotRegisterClients pins the way out when dynamic
// registration is not offered.
func TestAuthorizeExplainsAProviderThatWillNotRegisterClients(t *testing.T) {
	p := newProvider(t)
	p.register = false
	_, err := Authorize(context.Background(), options(t, p))
	if err == nil || !strings.Contains(err.Error(), "--client-id") {
		t.Fatalf("Authorize = %v; want an error that points to --client-id", err)
	}
}

// TestSignInFileOthersCanReadIsRefused pins that tokens are never read from a shared file.
func TestSignInFileOthersCanReadIsRefused(t *testing.T) {
	config := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(Path(config), []byte(`{"version":1,"servers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := read(Path(config)); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("read = %v; want a refusal that says how to fix the file", err)
	}
}
