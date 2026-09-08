package robinhood

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const oauthStateVersion = 1

type oauthState struct {
	Version      int           `json:"version"`
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret,omitempty"`
	AuthURL      string        `json:"auth_url"`
	TokenURL     string        `json:"token_url"`
	RedirectURL  string        `json:"redirect_url"`
	Scopes       []string      `json:"scopes,omitempty"`
	Token        *oauth2.Token `json:"token"`
}

// SecretWriter persists the complete OAuth bundle outside the runner. It must
// return only after the durable replacement has succeeded.
type SecretWriter interface {
	Write(ctx context.Context, value []byte) error
}

// GitHubCLISecretWriter uses a short-lived GitHub App installation token from
// GH_TOKEN. The secret value is passed on stdin and never in process arguments.
type GitHubCLISecretWriter struct {
	Repository  string
	Environment string
	SecretName  string
}

func (w GitHubCLISecretWriter) Write(ctx context.Context, value []byte) error {
	if w.Repository == "" || w.Environment == "" || w.SecretName == "" {
		return errors.New("incomplete GitHub secret writer configuration")
	}
	cmd := exec.CommandContext(ctx, "gh", "secret", "set", w.SecretName,
		"--repo", w.Repository, "--env", w.Environment)
	cmd.Stdin = bytes.NewReader(value)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("replace GitHub environment secret: %w", err)
	}
	return nil
}

type oauthFileStore struct {
	path   string
	writer SecretWriter
	mu     sync.Mutex
}

func (s *oauthFileStore) load() (*oauthState, error) {
	info, err := os.Lstat(s.path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("OAuth state must be a regular file with mode 0600")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var state oauthState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode OAuth state: %w", err)
	}
	if state.Version != oauthStateVersion || state.ClientID == "" || state.Token == nil || state.TokenURL == "" {
		return nil, errors.New("OAuth state is incomplete or has an unsupported version")
	}
	return &state, nil
}

func (s *oauthFileStore) save(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) error {
	if cfg == nil || token == nil || cfg.ClientID == "" || cfg.Endpoint.TokenURL == "" {
		return errors.New("refusing to persist incomplete OAuth state")
	}
	state := oauthState{
		Version: oauthStateVersion, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		AuthURL: cfg.Endpoint.AuthURL, TokenURL: cfg.Endpoint.TokenURL,
		RedirectURL: cfg.RedirectURL, Scopes: append([]string(nil), cfg.Scopes...),
		Token: token,
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := atomicWrite0600(s.path, data); err != nil {
		return fmt.Errorf("persist OAuth state file: %w", err)
	}
	if s.writer != nil {
		if err := s.writer.Write(ctx, data); err != nil {
			return fmt.Errorf("persist rotated OAuth state: %w", err)
		}
	}
	return nil
}

func atomicWrite0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".robinhood-oauth-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

type savingTokenSource struct {
	mu      sync.Mutex
	source  oauth2.TokenSource
	cfg     *oauth2.Config
	current *oauth2.Token
	save    func(context.Context, *oauth2.Config, *oauth2.Token) error
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, err := s.source.Token()
	if err != nil {
		return nil, err
	}
	if !sameToken(s.current, token) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.save(ctx, s.cfg, token); err != nil {
			return nil, err
		}
		copyToken := *token
		s.current = &copyToken
	}
	return token, nil
}

func sameToken(a, b *oauth2.Token) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken &&
		a.TokenType == b.TokenType && a.Expiry.Equal(b.Expiry)
}

type oauthOptions struct {
	StateFile                string
	RedirectURL              string
	AuthorizationCodeFetcher auth.AuthorizationCodeFetcher
	Writer                   SecretWriter
	HTTPClient               *http.Client
}

func newOAuthHandler(opts oauthOptions) (*auth.AuthorizationCodeHandler, error) {
	if opts.StateFile == "" {
		return nil, errors.New("OAuth state file is required")
	}
	store := &oauthFileStore{path: opts.StateFile, writer: opts.Writer}
	var initial oauth2.TokenSource
	var preregistered *oauthex.ClientCredentials
	var dynamic *auth.DynamicClientRegistrationConfig
	redirectURL := opts.RedirectURL

	state, err := store.load()
	if err == nil {
		cfg := &oauth2.Config{
			ClientID: state.ClientID, ClientSecret: state.ClientSecret,
			Endpoint:    oauth2.Endpoint{AuthURL: state.AuthURL, TokenURL: state.TokenURL},
			RedirectURL: state.RedirectURL, Scopes: state.Scopes,
		}
		redirectURL = state.RedirectURL
		tokenContext := context.WithValue(context.Background(), oauth2.HTTPClient, opts.HTTPClient)
		source := cfg.TokenSource(tokenContext, state.Token)
		initial = &savingTokenSource{source: source, cfg: cfg, current: state.Token, save: store.save}
		preregistered = &oauthex.ClientCredentials{ClientID: state.ClientID}
		if state.ClientSecret != "" {
			preregistered.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: state.ClientSecret}
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if redirectURL == "" {
			return nil, errors.New("redirect URL is required for initial Robinhood authorization")
		}
		dynamic = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{redirectURL}, ClientName: "quant-stocks rebalancer",
			TokenEndpointAuthMethod: "none",
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
		}}
	} else {
		return nil, err
	}

	fetcher := opts.AuthorizationCodeFetcher
	if fetcher == nil {
		fetcher = func(context.Context, *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			return nil, errors.New("interactive authorization required; run the robinhood-auth command")
		}
	}
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: preregistered, DynamicClientRegistrationConfig: dynamic,
		RedirectURL: redirectURL, AuthorizationCodeFetcher: fetcher,
		RequestRefreshToken: true, Client: opts.HTTPClient,
		InitialTokenSource: initial,
		NewTokenSource: func(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			if err := store.save(ctx, cfg, token); err != nil {
				return nil, err
			}
			copyToken := *token
			tokenContext := context.WithValue(ctx, oauth2.HTTPClient, opts.HTTPClient)
			return &savingTokenSource{
				source: cfg.TokenSource(tokenContext, token), cfg: cfg, current: &copyToken, save: store.save,
			}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return handler, nil
}

type codeReceiver struct {
	listener net.Listener
	server   *http.Server
	result   chan *auth.AuthorizationResult
	err      chan error
}

func newCodeReceiver(port int) (*codeReceiver, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	r := &codeReceiver{listener: listener, result: make(chan *auth.AuthorizationResult, 1), err: make(chan error, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, req *http.Request) {
		result := &auth.AuthorizationResult{
			Code: req.URL.Query().Get("code"), State: req.URL.Query().Get("state"), Iss: req.URL.Query().Get("iss"),
		}
		if result.Code == "" {
			r.err <- errors.New("OAuth callback did not contain a code")
			http.Error(w, "Authentication failed", http.StatusBadRequest)
			return
		}
		r.result <- result
		fmt.Fprint(w, "Robinhood authorization succeeded. You may close this window.")
	})
	r.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := r.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.err <- err
		}
	}()
	return r, nil
}

func (r *codeReceiver) redirectURL() string {
	return "http://" + r.listener.Addr().String() + "/callback"
}

func (r *codeReceiver) fetcher(output io.Writer) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		fmt.Fprintf(output, "Open this URL in a desktop browser:\n%s\n", args.URL)
		select {
		case result := <-r.result:
			return result, nil
		case err := <-r.err:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (r *codeReceiver) close(ctx context.Context) {
	_ = r.server.Shutdown(ctx)
}
