package robinhood

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type fakeSecretWriter struct {
	value []byte
	err   error
}

func (w *fakeSecretWriter) Write(_ context.Context, value []byte) error {
	w.value = append([]byte(nil), value...)
	return w.err
}

func TestOAuthStoreWrites0600AndDurableCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.json")
	writer := &fakeSecretWriter{}
	store := &oauthFileStore{path: path, writer: writer}
	cfg := &oauth2.Config{
		ClientID: "client", Endpoint: oauth2.Endpoint{AuthURL: "https://auth.example/authorize", TokenURL: "https://auth.example/token"},
		RedirectURL: "http://127.0.0.1/callback", Scopes: []string{"trade"},
	}
	token := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}
	if err := store.save(context.Background(), cfg, token); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions=%o", info.Mode().Perm())
	}
	if len(writer.value) == 0 {
		t.Fatal("durable writer was not called")
	}
	loaded, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ClientID != "client" || loaded.Token.RefreshToken != "refresh" {
		t.Fatalf("bad state: %+v", loaded)
	}
}

func TestOAuthWriteBackFailureFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.json")
	writer := &fakeSecretWriter{err: errors.New("denied")}
	store := &oauthFileStore{path: path, writer: writer}
	cfg := &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{TokenURL: "https://auth.example/token"}}
	err := store.save(context.Background(), cfg, &oauth2.Token{AccessToken: "access"})
	if err == nil {
		t.Fatal("expected synchronous write-back failure")
	}
}

func TestOAuthStoreRejectsLooseFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&oauthFileStore{path: path}).load(); err == nil {
		t.Fatal("OAuth store accepted a file with group/world permissions")
	}
}

func TestSavingTokenSourceDoesNotReturnRotatedTokenWhenPersistenceFails(t *testing.T) {
	cfg := &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{TokenURL: "https://auth.example/token"}}
	source := &savingTokenSource{
		source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "new", RefreshToken: "new-refresh"}),
		cfg:    cfg, current: &oauth2.Token{AccessToken: "old", RefreshToken: "old-refresh"},
		save: func(context.Context, *oauth2.Config, *oauth2.Token) error { return errors.New("write-back failed") },
	}
	if _, err := source.Token(); err == nil {
		t.Fatal("rotated token escaped despite persistence failure")
	}
}
