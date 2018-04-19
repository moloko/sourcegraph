package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"sourcegraph.com/sourcegraph/sourcegraph/cmd/frontend/internal/db"
	"sourcegraph.com/sourcegraph/sourcegraph/cmd/frontend/internal/pkg/types"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/actor"
	"sourcegraph.com/sourcegraph/sourcegraph/schema"

	oidc "github.com/coreos/go-oidc"
	"sourcegraph.com/sourcegraph/sourcegraph/cmd/frontend/internal/session"
)

// providerJSON is the JSON structure the OIDC provider returns at its discovery endpoing
type providerJSON struct {
	Issuer      string `json:"issuer"`
	AuthURL     string `json:"authorization_endpoint"`
	TokenURL    string `json:"token_endpoint"`
	JWKSURL     string `json:"jwks_uri"`
	UserInfoURL string `json:"userinfo_endpoint"`
}

var testOIDCUser = "bob-test-user"

// new OIDCIDServer returns a new running mock OIDC ID Provider service. It is the caller's
// responsibility to call Close().
func newOIDCIDServer(t *testing.T, code string) *httptest.Server {
	idBearerToken := "test_id_token_f4bdefbd77f"
	s := http.NewServeMux()

	s.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(providerJSON{
			Issuer:      oidcProvider.Issuer,
			AuthURL:     oidcProvider.Issuer + "/oauth2/v1/authorize",
			TokenURL:    oidcProvider.Issuer + "/oauth2/v1/token",
			UserInfoURL: oidcProvider.Issuer + "/oauth2/v1/userinfo",
		})
	})
	s.HandleFunc("/oauth2/v1/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		b, _ := ioutil.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(b))

		check(t, code == values.Get("code"), "code did not match expected")
		if got, want := values.Get("grant_type"), "authorization_code"; got != want {
			t.Errorf("got grant_type %v, want %v", got, want)
		}
		redirectURI, _ := url.QueryUnescape(values.Get("redirect_uri"))
		if want := "http://example.com/.auth/callback"; redirectURI != want {
			t.Errorf("got redirect_uri %v, want %v", redirectURI, want)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fmt.Sprintf(`{
			"access_token": "aaaaa",
			"token_type": "Bearer",
			"expires_in": 3600,
			"scope": "openid",
			"id_token": %q
		}`, idBearerToken)))
	})
	s.HandleFunc("/oauth2/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		authzHeader := r.Header.Get("Authorization")
		authzParts := strings.Split(authzHeader, " ")
		if len(authzParts) != 2 {
			t.Fatalf("Expected 2 parts to authz header, instead got %d: %q", len(authzParts), authzHeader)
		}
		if authzParts[0] != "Bearer" {
			t.Fatalf("No bearer token found in authz header %q", authzHeader)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fmt.Sprintf(`{
			"sub": %q,
			"profile": "This is a profile",
			"email": "bob@foo.com",
			"email_verified": true,
			"picture": "https://example.com/picture.png"
		}`, testOIDCUser)))
	})

	srv := httptest.NewServer(s)

	// Mock user
	db.Mocks.Users.GetByExternalID = func(ctx context.Context, provider, id string) (*types.User, error) {
		if provider == oidcProvider.Issuer && id == srv.URL+":"+testOIDCUser {
			return &types.User{
				ID:         123,
				ExternalID: &id,
				Username:   id,
				AvatarURL:  "https://example.com/picture.png",
			}, nil
		}
		return nil, fmt.Errorf("provider %q user %q not found in mock", provider, id)
	}

	return srv
}

func Test_newOIDCAuthHandler(t *testing.T) {
	cleanup := session.ResetMockSessionStore(t)
	defer cleanup()

	tempdir, err := ioutil.TempDir("", "sourcegraph-oidc-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempdir)

	oidcIDServer := newOIDCIDServer(t, "THECODE")
	defer oidcIDServer.Close()

	oidcProvider = &schema.OpenIDConnectAuthProvider{
		Issuer:       oidcIDServer.URL,
		ClientID:     "aaaaaaaaaaaaaa",
		ClientSecret: "aaaaaaaaaaaaaaaaaaaaaaaaa",
	}

	validState := (&authnState{CSRFToken: "THE_CSRF_TOKEN", Redirect: "/redirect"}).Encode()
	mockVerifyIDToken = func(rawIDToken string) *oidc.IDToken {
		if rawIDToken != "test_id_token_f4bdefbd77f" {
			t.Fatalf("unexpected raw ID token: %s", rawIDToken)
		}
		return &oidc.IDToken{
			Issuer:  oidcIDServer.URL,
			Subject: testOIDCUser,
			Expiry:  time.Now().Add(time.Hour),
			Nonce:   validState, // we re-use the state param as the nonce
		}
	}

	testOIDCExternalID := oidcToExternalID(oidcProvider.Issuer, testOIDCUser)
	const mockUserID = 123
	db.Mocks.Users.GetByExternalID = func(ctx context.Context, provider, id string) (*types.User, error) {
		if provider == oidcProvider.Issuer && id == testOIDCExternalID {
			return &types.User{ID: mockUserID, ExternalID: &id, Username: "testuser"}, nil
		}
		return nil, fmt.Errorf("provider %q user %q not found in mock", provider, id)
	}
	db.Mocks.Users.Update = func(userID int32, update db.UserUpdate) error {
		if userID != mockUserID {
			t.Errorf("got userID %d, want %d", userID, mockUserID)
		}
		return nil
	}
	defer func() { db.Mocks = db.MockStores{} }()

	authedHandler, err := newOIDCAuthHandler(context.Background(), newAppHandler(t, mockUserID), "http://example.com")
	if err != nil {
		t.Fatal(err)
	}

	doRequest := func(method, urlStr, body string, cookies []*http.Cookie) *http.Response {
		req := httptest.NewRequest(method, urlStr, bytes.NewBufferString(body))
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		respRecorder := httptest.NewRecorder()
		authedHandler.ServeHTTP(respRecorder, req)
		return respRecorder.Result()
	}

	t.Run("unauthenticated homepage visit -> login redirect", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com", "", nil)
		if want := http.StatusFound; resp.StatusCode != want {
			t.Errorf("got response code %v, want %v", resp.StatusCode, want)
		}
		if got, want := resp.Header.Get("Location"), "/.auth/login?redirect="; got != want {
			t.Errorf("got redirect URL %v, want %v", got, want)
		}
	})
	t.Run("unauthenticated subpage visit -> login redirect", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com/page", "", nil)
		if want := http.StatusFound; resp.StatusCode != want {
			t.Errorf("got response code %v, want %v", resp.StatusCode, want)
		}
		if got, want := resp.Header.Get("Location"), "/.auth/login?redirect=%2Fpage"; got != want {
			t.Errorf("got redirect URL %v, want %v", got, want)
		}
	})
	t.Run("unauthenticated non-existent page visit -> login redirect", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com/nonexistent", "", nil)
		if want := http.StatusFound; resp.StatusCode != want {
			t.Errorf("got response code %v, want %v", resp.StatusCode, want)
		}
		if got, want := resp.Header.Get("Location"), "/.auth/login?redirect=%2Fnonexistent"; got != want {
			t.Errorf("got redirect URL %v, want %v", got, want)
		}
	})
	t.Run("login redirect -> sso login", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com/.auth/login", "", nil)
		if want := http.StatusFound; resp.StatusCode != want {
			t.Errorf("got response code %v, want %v", resp.StatusCode, want)
		}
		locHeader := resp.Header.Get("Location")
		check(t, strings.HasPrefix(locHeader, oidcProvider.Issuer+"/"), "did not redirect to OIDC Provider")
		idpLoginURL, err := url.Parse(locHeader)
		if err != nil {
			t.Fatal(err)
		}
		check(t, oidcProvider.ClientID == idpLoginURL.Query().Get("client_id"), "client id didn't match")
		if got, want := idpLoginURL.Query().Get("redirect_uri"), "http://example.com/.auth/callback"; got != want {
			t.Errorf("got redirect_uri %v, want %v", got, want)
		}
		if got, want := idpLoginURL.Query().Get("response_type"), "code"; got != want {
			t.Errorf("got response_type %v, want %v", got, want)
		}
		if got, want := idpLoginURL.Query().Get("scope"), "openid profile email"; got != want {
			t.Errorf("got scope %v, want %v", got, want)
		}
	})
	t.Run("OIDC callback without CSRF token -> error", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com/.auth/callback?code=THECODE&state=ASDF", "", nil)
		if want := http.StatusBadRequest; resp.StatusCode != want {
			t.Errorf("got status code %v, want %v", resp.StatusCode, want)
		}
	})
	var authCookies []*http.Cookie
	t.Run("OIDC callback with CSRF token -> set auth cookies", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com/.auth/callback?code=THECODE&state="+url.PathEscape(validState), "", []*http.Cookie{{Name: oidcStateCookieName, Value: validState}})
		if want := http.StatusFound; resp.StatusCode != want {
			t.Errorf("got status code %v, want %v", resp.StatusCode, want)
		}
		if got, want := resp.Header.Get("Location"), "/redirect"; got != want {
			t.Errorf("got redirect URL %v, want %v", got, want)
		}
		authCookies = unexpiredCookies(resp)
	})
	t.Run("authenticated homepage visit", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com", "", authCookies)
		if want := http.StatusOK; resp.StatusCode != want {
			t.Errorf("got response code %v, want %v", resp.StatusCode, want)
		}
		respBody, _ := ioutil.ReadAll(resp.Body)
		if got, want := string(respBody), "This is the home"; got != want {
			t.Errorf("got response body %v, want %v", got, want)
		}
	})
	t.Run("authenticated subpage visit", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com/page", "", authCookies)
		if want := http.StatusOK; resp.StatusCode != want {
			t.Errorf("got response code %v, want %v", resp.StatusCode, want)
		}
		respBody, _ := ioutil.ReadAll(resp.Body)
		if got, want := string(respBody), "This is a page"; got != want {
			t.Errorf("got response body %v, want %v", got, want)
		}
	})
	t.Run("authenticated non-existent page visit -> 404", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com/nonexistent", "", authCookies)
		if want := http.StatusNotFound; resp.StatusCode != want {
			t.Errorf("got response code %v, want %v", resp.StatusCode, want)
		}
	})
	t.Run("verify actor gets set in request context", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com/require-authn", "", authCookies)
		if want := http.StatusOK; resp.StatusCode != want {
			t.Errorf("got status code %v, want %v", resp.StatusCode, want)
		}
	})
}

func Test_newOIDCAuthHandler_NoOpenRedirect(t *testing.T) {
	cleanup := session.ResetMockSessionStore(t)
	defer cleanup()

	tempdir, err := ioutil.TempDir("", "sourcegraph-oidc-test-no-open-redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempdir)

	oidcIDServer := newOIDCIDServer(t, "THECODE")
	defer oidcIDServer.Close()

	oidcProvider = &schema.OpenIDConnectAuthProvider{
		Issuer:       oidcIDServer.URL,
		ClientID:     "aaaaaaaaaaaaaa",
		ClientSecret: "aaaaaaaaaaaaaaaaaaaaaaaaa",
	}

	state := (&authnState{CSRFToken: "THE_CSRF_TOKEN", Redirect: "http://evil.com"}).Encode()
	mockVerifyIDToken = func(rawIDToken string) *oidc.IDToken {
		if rawIDToken != "test_id_token_f4bdefbd77f" {
			t.Fatalf("unexpected raw ID token: %s", rawIDToken)
		}
		return &oidc.IDToken{
			Issuer:  oidcIDServer.URL,
			Subject: testOIDCUser,
			Expiry:  time.Now().Add(time.Hour),
			Nonce:   state, // we re-use the state param as the nonce
		}
	}

	authedHandler, err := newOIDCAuthHandler(context.Background(), newAppHandler(t, 123), "http://example.com")
	if err != nil {
		t.Fatal(err)
	}

	doRequest := func(method, urlStr, body string, cookies []*http.Cookie) *http.Response {
		req := httptest.NewRequest(method, urlStr, bytes.NewBufferString(body))
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		respRecorder := httptest.NewRecorder()
		authedHandler.ServeHTTP(respRecorder, req)
		return respRecorder.Result()
	}

	t.Run("OIDC callback with CSRF token -> set auth cookies", func(t *testing.T) {
		resp := doRequest("GET", "http://example.com/.auth/callback?code=THECODE&state="+url.PathEscape(state), "", []*http.Cookie{{Name: oidcStateCookieName, Value: state}})
		if want := http.StatusFound; resp.StatusCode != want {
			t.Errorf("got status code %v, want %v", resp.StatusCode, want)
		}
		if got, want := resp.Header.Get("Location"), "/"; got != want {
			t.Errorf("got redirect URL %v, want %v", got, want)
		} // Redirect to "/", NOT "http://evil.com"
	})
}

// newAppHandler returns a new mock app handler meant to be wrapped by the OIDC handler in tests.
func newAppHandler(t *testing.T, mockedUserID int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "":
			w.Write([]byte("This is the home"))
		case "/page":
			w.Write([]byte("This is a page"))
		case "/require-authn":
			actr := actor.FromContext(r.Context())
			if actr.UID == 0 {
				t.Errorf("in authn expected-endpoint, no actor was set; expected actor with UID %d", mockedUserID)
			} else if actr.UID != mockedUserID {
				t.Errorf("in authn expected-endpoint, actor with incorrect UID was set; %d != %d", actr.UID, mockedUserID)
			}
			w.Write([]byte("Authenticated"))
		default:
			http.Error(w, "", http.StatusNotFound)
		}
	})
}
