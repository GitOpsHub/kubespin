package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeProvider is a scriptable Provider for exercising the orchestrator
// without any real cloud CLI or credentials.
type fakeProvider struct {
	name string

	authenticated bool
	statusErr     error
	detail        StatusDetail

	loginErr    error
	loginCalls  int
	logoutErr   error
	logoutCalls int

	// afterLogin flips authenticated to true once Login has been called,
	// simulating a real provider that is unauthenticated until logged in.
	afterLogin bool
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) IsAuthenticated(context.Context) (bool, StatusDetail, error) {
	return f.authenticated, f.detail, f.statusErr
}

func (f *fakeProvider) Login(context.Context) error {
	f.loginCalls++
	if f.loginErr == nil && f.afterLogin {
		f.authenticated = true
	}
	return f.loginErr
}

func (f *fakeProvider) Logout(context.Context) error {
	f.logoutCalls++
	f.authenticated = false
	return f.logoutErr
}

func TestRegistry_Select(t *testing.T) {
	aws := &fakeProvider{name: "aws"}
	gcp := &fakeProvider{name: "gcp"}
	azure := &fakeProvider{name: "azure"}
	reg := NewRegistry(aws, gcp, azure)

	t.Run("empty selects all in registry order", func(t *testing.T) {
		got, err := reg.Select(nil)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if len(got) != 3 || got[0] != aws || got[1] != gcp || got[2] != azure {
			t.Errorf("got %v, want all three in order", got)
		}
	})

	t.Run("subset, case-insensitive", func(t *testing.T) {
		got, err := reg.Select([]string{"AWS", "gcp"})
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if len(got) != 2 || got[0] != aws || got[1] != gcp {
			t.Errorf("got %v, want aws and gcp", got)
		}
	})

	t.Run("unknown name errors", func(t *testing.T) {
		_, err := reg.Select([]string{"oracle"})
		if err == nil || !strings.Contains(err.Error(), "oracle") {
			t.Fatalf("error = %v, want one naming the unknown provider", err)
		}
	})
}

func TestStatus_RunsEveryProviderConcurrentlyAndReportsAll(t *testing.T) {
	aws := &fakeProvider{name: "aws", authenticated: true, detail: StatusDetail{Message: "ok"}}
	gcp := &fakeProvider{name: "gcp", authenticated: false}
	azure := &fakeProvider{name: "azure", statusErr: errors.New("az CLI not found")}

	results := Status(t.Context(), []Provider{aws, gcp, azure})
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}

	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Provider] = r
	}

	if !byName["aws"].Authenticated || byName["aws"].Status.Message != "ok" {
		t.Errorf("aws result = %+v", byName["aws"])
	}
	if byName["gcp"].Authenticated {
		t.Errorf("gcp result = %+v, want unauthenticated", byName["gcp"])
	}
	if byName["azure"].Err == nil {
		t.Errorf("azure result = %+v, want the CLI-missing error surfaced", byName["azure"])
	}
}

func TestLogin_SkipsAlreadyAuthenticatedProviders(t *testing.T) {
	p := &fakeProvider{name: "aws", authenticated: true}

	Login(t.Context(), []Provider{p}, false)

	if p.loginCalls != 0 {
		t.Errorf("Login called %d times, want 0 for an already-authenticated provider", p.loginCalls)
	}
}

func TestLogin_ForceReAuthenticatesRegardless(t *testing.T) {
	p := &fakeProvider{name: "aws", authenticated: true}

	Login(t.Context(), []Provider{p}, true)

	if p.loginCalls != 1 {
		t.Errorf("Login called %d times, want 1 with force set", p.loginCalls)
	}
}

func TestLogin_AuthenticatesAnUnauthenticatedProvider(t *testing.T) {
	p := &fakeProvider{name: "aws", authenticated: false, afterLogin: true}

	results := Login(t.Context(), []Provider{p}, false)

	if p.loginCalls != 1 {
		t.Errorf("Login called %d times, want 1", p.loginCalls)
	}
	if !results[0].Authenticated {
		t.Errorf("result = %+v, want authenticated after login", results[0])
	}
}

func TestLogin_SurfacesLoginFailure(t *testing.T) {
	p := &fakeProvider{name: "aws", authenticated: false, loginErr: errors.New("browser flow cancelled")}

	results := Login(t.Context(), []Provider{p}, false)

	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "cancelled") {
		t.Errorf("result = %+v, want the login error surfaced", results[0])
	}
}

func TestLogout_ClearsEveryProvider(t *testing.T) {
	aws := &fakeProvider{name: "aws", authenticated: true}
	gcp := &fakeProvider{name: "gcp", authenticated: true}

	results := Logout(t.Context(), []Provider{aws, gcp})

	if aws.logoutCalls != 1 || gcp.logoutCalls != 1 {
		t.Errorf("logout calls = aws:%d gcp:%d, want 1 each", aws.logoutCalls, gcp.logoutCalls)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("result = %+v, want no error", r)
		}
	}
}

func TestEnsureAll_ErrorsNamingEveryUnauthenticatedProvider(t *testing.T) {
	aws := &fakeProvider{name: "aws", authenticated: true}
	gcp := &fakeProvider{name: "gcp", authenticated: false}
	azure := &fakeProvider{name: "azure", authenticated: false}

	err := EnsureAll(t.Context(), []Provider{aws, gcp, azure})
	if err == nil {
		t.Fatal("expected an error listing the unauthenticated providers")
	}
	for _, want := range []string{"gcp", "azure", "kubespin login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "aws") {
		t.Errorf("error %q mentions the already-authenticated provider", err)
	}
}

func TestEnsureAll_PassesWhenEveryProviderIsAuthenticated(t *testing.T) {
	aws := &fakeProvider{name: "aws", authenticated: true}
	if err := EnsureAll(t.Context(), []Provider{aws}); err != nil {
		t.Errorf("EnsureAll = %v, want nil", err)
	}
}

func TestWriteTable_DoesNotPanicOnEveryResultShape(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	results := []Result{
		{Provider: "aws", Authenticated: true, Status: StatusDetail{Message: "ok", ExpiresAt: &expiry}},
		{Provider: "gcp", Authenticated: false},
		{Provider: "azure", Err: errors.New("boom")},
	}

	var buf strings.Builder
	WriteTable(&buf, results)

	out := buf.String()
	for _, want := range []string{"AWS", "GCP", "AZURE", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("table %q does not contain %q", out, want)
		}
	}
}
