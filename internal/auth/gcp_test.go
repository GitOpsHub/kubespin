package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingOutput scripts responses to shelled-out commands keyed by their
// joined form, so a test can drive multiple distinct gcloud invocations.
type recordingOutput struct {
	calls     [][]string
	responses map[string]string
	errs      map[string]error
}

func newRecordingOutput() *recordingOutput {
	return &recordingOutput{responses: map[string]string{}, errs: map[string]error{}}
}

func (r *recordingOutput) run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	key := strings.Join(call, " ")
	if err, ok := r.errs[key]; ok {
		return "", err
	}
	return r.responses[key], nil
}

func TestGCPProvider_Name(t *testing.T) {
	if (&GCPProvider{}).Name() != "gcp" {
		t.Errorf("Name() = %q, want gcp", (&GCPProvider{}).Name())
	}
}

func TestGCPProvider_IsAuthenticated_Valid(t *testing.T) {
	out := newRecordingOutput()
	out.responses["gcloud auth application-default print-access-token"] = "ya29.faketoken"
	out.responses["gcloud config get-value account"] = "you@org.com"
	p := &GCPProvider{out: out.run}

	ok, detail, err := p.IsAuthenticated(t.Context())
	if err != nil {
		t.Fatalf("IsAuthenticated: %v", err)
	}
	if !ok {
		t.Error("Authenticated = false, want true")
	}
	if !strings.Contains(detail.Message, "you@org.com") {
		t.Errorf("detail = %+v, want the account mentioned", detail)
	}
}

func TestGCPProvider_IsAuthenticated_Invalid(t *testing.T) {
	out := newRecordingOutput()
	out.errs["gcloud auth application-default print-access-token"] = errors.New("no credentials")
	p := &GCPProvider{out: out.run}

	ok, _, err := p.IsAuthenticated(t.Context())
	if err != nil {
		t.Fatalf("IsAuthenticated returned an error: %v, want nil with ok=false", err)
	}
	if ok {
		t.Error("Authenticated = true, want false with no ADC token available")
	}
}

func TestGCPProvider_Login_RunsBothLogins(t *testing.T) {
	runner := &recordingRunner{}
	p := &GCPProvider{run: runner.run}

	if err := p.Login(t.Context()); err != nil {
		if !strings.Contains(err.Error(), "gcloud CLI not found") {
			t.Fatalf("Login: %v", err)
		}
		return
	}

	if len(runner.calls) != 2 {
		t.Fatalf("commands run = %v, want exactly two", runner.calls)
	}
	if strings.Join(runner.calls[0], " ") != "gcloud auth login" {
		t.Errorf("first command = %v, want gcloud auth login", runner.calls[0])
	}
	if strings.Join(runner.calls[1], " ") != "gcloud auth application-default login" {
		t.Errorf("second command = %v, want gcloud auth application-default login", runner.calls[1])
	}
}

func TestGCPProvider_Logout_RevokesBothSessions(t *testing.T) {
	runner := &recordingRunner{}
	p := &GCPProvider{run: runner.run}

	if err := p.Logout(t.Context()); err != nil {
		if !strings.Contains(err.Error(), "gcloud CLI not found") {
			t.Fatalf("Logout: %v", err)
		}
		return
	}

	if len(runner.calls) != 2 {
		t.Fatalf("commands run = %v, want exactly two", runner.calls)
	}
	if strings.Join(runner.calls[0], " ") != "gcloud auth application-default revoke --quiet" {
		t.Errorf("first command = %v", runner.calls[0])
	}
	if strings.Join(runner.calls[1], " ") != "gcloud auth revoke --all --quiet" {
		t.Errorf("second command = %v", runner.calls[1])
	}
}
