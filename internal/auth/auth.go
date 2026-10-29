// Package auth signs tap in to MCP servers and keeps what the sign-in gives it. The servers
// it signs in to are the ones that use OAuth, as the MCP authorization spec describes.
//
// Signing in is interactive and happens once, in Authorize: the SDK's authorization-code
// handler discovers the authorization server, registers tap as a client, and exchanges the
// code that the person's browser brings back. What it yields is saved beside the config, in a
// file only its owner can read. Serving is not interactive: Handler lends the saved token to a
// connection, renews it when it runs out, and reports Required when there is nothing to lend.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fschrhunt/tap/internal/wire"
	sdk "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// Required is the error for a server that wants a sign-in tap does not have.
type Required struct{ Server string }

func (r *Required) Error() string {
	return fmt.Sprintf("needs you to sign in: run \"tap auth %s\"", r.Server)
}

// requiredMessage matches exactly what Required.Error prints, and nothing around it.
var requiredMessage = regexp.MustCompile(`^needs you to sign in: run "tap auth [^"]+"$`)

// IsRequiredMessage reports whether msg is tap's own sign-in instruction rather than a
// connector's diagnostics. The instruction carries only a server's name, so a relay may
// show it to remote clients where it masks every other error.
func IsRequiredMessage(msg string) bool { return requiredMessage.MatchString(msg) }

// grant is one server's sign-in: the client tap is registered as, where its tokens are
// renewed, and the token itself. URL binds it to the endpoint it was given for.
type grant struct {
	URL          string        `json:"url"`
	ClientID     string        `json:"clientId"`
	ClientSecret string        `json:"clientSecret,omitempty"`
	AuthURL      string        `json:"authUrl"`
	TokenURL     string        `json:"tokenUrl"`
	AuthStyle    int           `json:"authStyle,omitempty"`
	Scopes       []string      `json:"scopes,omitempty"`
	Token        *oauth2.Token `json:"token"`
}

type store struct {
	Version int               `json:"version"`
	Servers map[string]*grant `json:"servers"`
}

// Path is the file that holds the sign-ins for the config at configPath.
func Path(configPath string) string { return configPath + ".auth.json" }

// read loads the store. A missing file is an empty store. A file that others can read, or
// that is not a regular file, is refused: it holds tokens.
func read(path string) (*store, error) {
	s := &store{Version: 1, Servers: map[string]*grant{}}
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", path)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s holds sign-in tokens and must be a file only you can read: run \"chmod 600 %s\"", path, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", path)
	}
	if err = json.Unmarshal(b, s); err != nil || s.Version != 1 {
		return nil, fmt.Errorf("%s is not a sign-in file tap wrote; remove it and run \"tap auth\" again", path)
	}
	if s.Servers == nil {
		s.Servers = map[string]*grant{}
	}
	for _, g := range s.Servers {
		if g == nil {
			return nil, fmt.Errorf("%s is not a sign-in file tap wrote; remove it and run \"tap auth\" again", path)
		}
	}
	return s, nil
}

// update changes the store under a lock shared by every tap process, and replaces the file
// atomically with owner-only permissions.
func update(path string, change func(*store) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	s, err := read(path)
	if err != nil {
		return err
	}
	if err = change(s); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tap-auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(append(b, '\n')); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// Has reports whether a sign-in is saved for the server at this endpoint.
func Has(configPath, name, endpoint string) bool {
	s, err := read(Path(configPath))
	return err == nil && s.Servers[name] != nil && s.Servers[name].URL == endpoint
}

// Fingerprints hashes the sign-in store and each grant's identity for session and catalog
// invalidation. Tokens are deliberately excluded: a routine renewal must not invalidate
// sessions or persisted catalogs, while sign-out and removal still change the set of grants.
// Invalid stores fail closed; tokens and client secrets never leave as plaintext.
func Fingerprints(configPath string) (string, map[string]string, error) {
	s, err := read(Path(configPath))
	if err != nil {
		return "", nil, err
	}
	grants := make(map[string]string, len(s.Servers))
	for name, g := range s.Servers {
		identity := *g
		identity.Token = nil
		b, err := json.Marshal(identity)
		if err != nil {
			return "", nil, err
		}
		grants[name] = fmt.Sprintf("%x", sha256.Sum256(b))
	}
	b, err := json.Marshal(grants)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), grants, nil
}

// Remove forgets a server's sign-in and reports whether there was one.
func Remove(configPath, name string) (bool, error) {
	removed := false
	err := update(Path(configPath), func(s *store) error {
		_, removed = s.Servers[name]
		delete(s.Servers, name)
		return nil
	})
	return removed, err
}

// source lends a server's saved token and renews it. The file is the truth: several tap
// processes share one sign-in, so a renewal is made under the store's lock, and a token
// another process has already renewed is taken from the file instead of renewed again.
type source struct {
	path, name, endpoint string
	client               *http.Client
	mu                   sync.Mutex
}

// errNone tells the SDK's transport to send the request without a token, so that the
// server's refusal reaches Handler.Authorize.
var errNone = &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: "no usable token is saved"}

// Token checks saved authority on every request and serializes renewal across processes.
func (s *source) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := read(s.path)
	if err != nil {
		return nil, err
	}
	g := st.Servers[s.name]
	if g == nil || g.URL != s.endpoint || g.Token == nil {
		return nil, errNone
	}
	if g.Token.Valid() {
		return g.Token, nil
	}
	var token *oauth2.Token
	err = update(s.path, func(st *store) error {
		g := st.Servers[s.name]
		if g == nil || g.URL != s.endpoint || g.Token == nil {
			return errNone
		}
		if g.Token.Valid() {
			token = g.Token
			return nil
		}
		if g.Token.RefreshToken == "" {
			return errNone
		}
		cfg := oauth2.Config{ClientID: g.ClientID, ClientSecret: g.ClientSecret, Scopes: g.Scopes,
			Endpoint: oauth2.Endpoint{AuthURL: g.AuthURL, TokenURL: g.TokenURL, AuthStyle: oauth2.AuthStyle(g.AuthStyle)}}
		ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), oauth2.HTTPClient, s.client), 30*time.Second)
		defer cancel()
		renewed, err := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: g.Token.RefreshToken}).Token()
		if err != nil {
			return err
		}
		if renewed.RefreshToken == "" {
			renewed.RefreshToken = g.Token.RefreshToken
		}
		g.Token, token = renewed, renewed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return token, nil
}

// handler is the non-interactive side: it lends the saved token, and answers a refusal by
// naming the command that signs in.
type handler struct {
	source *source
}

// Handler returns the OAuth handler for a connection to the named server's endpoint.
func Handler(configPath, name, endpoint string) sdk.OAuthHandler {
	return &handler{source: &source{path: Path(configPath), name: name, endpoint: endpoint, client: &http.Client{Timeout: 30 * time.Second}}}
}

func (h *handler) TokenSource(context.Context) (oauth2.TokenSource, error) { return h.source, nil }

func (h *handler) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	return &Required{Server: h.source.name}
}

// Options describe one sign-in.
type Options struct {
	ConfigPath, Name, Endpoint string
	// Headers are the server's static headers, sent with the MCP requests.
	Headers http.Header
	// ClientID and ClientSecret name an OAuth client registered by hand, for authorization
	// servers that do not let clients register themselves.
	ClientID, ClientSecret string
	// Port fixes the port of the loopback address the browser is sent back to; zero picks a
	// free one.
	Port int
	// Open shows the sign-in page to the person, usually by starting a browser. It may be nil.
	Open func(page string) error
	// RedirectURL, AuthPage and Callback let a remote client host the browser callback while
	// this process retains OAuth state, token exchange and credential storage.
	RedirectURL string
	AuthPage    chan<- string
	Callback    <-chan url.Values
	// Say receives what the person needs to read while they wait.
	Say     io.Writer
	Version string
}

// Result reports a sign-in: whether the server asked for one, and how many tools it lists now.
type Result struct {
	SignedIn bool
	HadGrant bool
	Tools    int
}

// Authorize connects to the server and, when it asks, signs in through the person's browser
// and saves the result. It returns when the server has answered with its tools, or ctx ends.
func Authorize(ctx context.Context, o Options) (*Result, error) {
	redirect := o.RedirectURL
	landed := make(chan url.Values, 1)
	if redirect == "" {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", o.Port))
		if err != nil {
			return nil, fmt.Errorf("cannot listen on 127.0.0.1:%d for the browser to come back to: %v", o.Port, err)
		}
		defer listener.Close()
		redirect = fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)
		web := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			message := "tap is signed in to " + o.Name + ". You can close this tab."
			if r.URL.Query().Get("code") == "" {
				message = "The sign-in did not finish. Go back to your terminal to see why."
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, message+"\n")
			select {
			case landed <- r.URL.Query():
			default:
			}
		})}
		go func() { _ = web.Serve(listener) }()
		defer web.Close()
	} else if o.Callback == nil || o.AuthPage == nil {
		return nil, fmt.Errorf("remote sign-in requires a callback and authorization page channel")
	}

	path := Path(o.ConfigPath)
	client := &http.Client{Timeout: 30 * time.Second}
	signedIn := false
	config := &sdk.AuthorizationCodeHandlerConfig{
		RedirectURL:         redirect,
		RequestRefreshToken: true,
		Client:              client,
		AuthorizationCodeFetcher: func(ctx context.Context, args *sdk.AuthorizationArgs) (*sdk.AuthorizationResult, error) {
			return fetch(ctx, o, args.URL, landed)
		},
		NewTokenSource: func(_ context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			err := update(path, func(s *store) error {
				s.Servers[o.Name] = &grant{URL: o.Endpoint, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, AuthURL: cfg.Endpoint.AuthURL,
					TokenURL: cfg.Endpoint.TokenURL, AuthStyle: int(cfg.Endpoint.AuthStyle), Scopes: cfg.Scopes, Token: token}
				return nil
			})
			if err != nil {
				return nil, err
			}
			signedIn = true
			return &source{path: path, name: o.Name, endpoint: o.Endpoint, client: client}, nil
		},
	}
	if o.ClientID != "" {
		config.PreregisteredClient = &oauthex.ClientCredentials{ClientID: o.ClientID}
		if o.ClientSecret != "" {
			config.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: o.ClientSecret}
		}
	} else {
		config.DynamicClientRegistrationConfig = &sdk.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{redirect}, ClientName: "tap", ClientURI: "https://github.com/fschrhunt/tap",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none", SoftwareID: "tap", SoftwareVersion: o.Version,
		}}
	}
	// A sign-in saved earlier is tried first, so running the command again says so instead
	// of sending the person through the browser for nothing.
	if Has(o.ConfigPath, o.Name, o.Endpoint) {
		config.InitialTokenSource = &source{path: path, name: o.Name, endpoint: o.Endpoint, client: client}
	}
	flow, err := sdk.NewAuthorizationCodeHandler(config)
	if err != nil {
		return nil, err
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "tap", Version: o.Version}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: o.Endpoint,
		HTTPClient: &http.Client{
			Transport:     headers{o.Headers},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		OAuthHandler:         flow,
		MaxRetries:           -1,
		MaxEventSize:         wire.MaxMessageBytes,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, explain(err, o)
	}
	defer session.Close()
	result := &Result{SignedIn: signedIn, HadGrant: Has(o.ConfigPath, o.Name, o.Endpoint)}
	for cursor := ""; ; {
		page, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, explain(err, o)
		}
		result.Tools += len(page.Tools)
		if cursor = page.NextCursor; cursor == "" {
			return result, nil
		}
	}
}

// fetch presents the sign-in page and waits for the browser callback, locally or through the
// selected remote client.
func fetch(ctx context.Context, o Options, page string, landed <-chan url.Values) (*sdk.AuthorizationResult, error) {
	if o.AuthPage != nil {
		select {
		case o.AuthPage <- page:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case query := <-o.Callback:
			return authorizationResult(query)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	fmt.Fprintf(o.Say, "Open this page to sign in to %s:\n\n  %s\n\n", o.Name, page)
	if o.Open != nil && o.Open(page) == nil {
		fmt.Fprintln(o.Say, "If a browser opened, finish signing in there.")
	}
	fmt.Fprintln(o.Say, "Waiting for you to finish in the browser. Ctrl-C stops waiting.")
	var query url.Values
	select {
	case query = <-landed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return authorizationResult(query)
}

func authorizationResult(query url.Values) (*sdk.AuthorizationResult, error) {
	if refusal := query.Get("error"); refusal != "" {
		if why := query.Get("error_description"); why != "" {
			refusal += ": " + why
		}
		return nil, fmt.Errorf("the sign-in was refused (%s)", refusal)
	}
	return &sdk.AuthorizationResult{Code: query.Get("code"), State: query.Get("state"), Iss: query.Get("iss")}, nil
}

// headers sends a server's static headers with every MCP request.
type headers struct{ set http.Header }

func (h headers) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	for k, v := range h.set {
		if r.Header.Get(k) == "" {
			r.Header[k] = v
		}
	}
	return wire.BoundResponse(http.DefaultTransport.RoundTrip(r))
}

// explain turns the failures a person can act on into what to do about them.
func explain(err error, o Options) error {
	text := err.Error()
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("stopped before the sign-in finished")
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("gave up waiting for the sign-in to finish; run the command again when you are ready")
	case strings.Contains(text, "no configured client registration methods"):
		return fmt.Errorf("%s does not let clients register themselves. Create an OAuth app with its provider, with the redirect address http://127.0.0.1:PORT/callback, and run \"tap auth %s --client-id ID --port PORT\"", o.Name, o.Name)
	case strings.Contains(text, "failed to register client"):
		return fmt.Errorf("%s refused to register tap as a client (%s). If you have an OAuth app there, pass --client-id", o.Name, text[strings.LastIndex(text, "failed to register client"):])
	case strings.Contains(text, "state mismatch"):
		return fmt.Errorf("the address that came back belongs to another sign-in; run the command again and use the new page")
	case strings.Contains(text, "the sign-in was refused"):
		return fmt.Errorf("%s", text[strings.Index(text, "the sign-in was refused"):])
	}
	var netErr *url.Error
	if errors.As(err, &netErr) {
		return fmt.Errorf("cannot reach %s: %v", o.Endpoint, netErr.Err)
	}
	return fmt.Errorf("could not sign in to %s: %s", o.Name, text)
}
