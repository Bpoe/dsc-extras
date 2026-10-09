package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

type fakeSecretGetter struct {
	response azsecrets.GetSecretResponse
	err      error
	name     string
	version  string
}

func (f *fakeSecretGetter) GetSecret(_ context.Context, name, version string, _ *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error) {
	f.name = name
	f.version = version
	return f.response, f.err
}

func stringPointer(value string) *string {
	return &value
}

func responseWithValue(value string) azsecrets.GetSecretResponse {
	return azsecrets.GetSecretResponse{
		Secret: azsecrets.Secret{Value: stringPointer(value)},
	}
}

func runWithClient(args []string, getter *fakeSecretGetter) (int, string, string) {
	var stdout, stderr strings.Builder
	factory := func(vault string) (secretGetter, error) {
		if vault != "https://custom.vault.example/" {
			return nil, fmt.Errorf("unexpected vault URI")
		}
		return getter, nil
	}
	code := run(args, &stdout, &stderr, factory, time.Second)
	return code, stdout.String(), stderr.String()
}

func TestRunWritesExactSecretAndRequestsLatestVersion(t *testing.T) {
	getter := &fakeSecretGetter{response: responseWithValue("exact secret")}
	code, stdout, stderr := runWithClient([]string{
		"--name", "DatabasePassword", "--vault", "https://custom.vault.example/",
	}, getter)

	if code != 0 || stdout != "exact secret" || stderr != "" {
		t.Fatalf("run() = (%d, %q, %q), want (0, %q, %q)", code, stdout, stderr, "exact secret", "")
	}
	if getter.name != "DatabasePassword" || getter.version != "" {
		t.Fatalf("GetSecret called with name=%q version=%q, want name and latest version", getter.name, getter.version)
	}
}

func TestRunRejectsMissingAndEmptyArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing name", args: []string{"--vault", "https://custom.vault.example/"}},
		{name: "missing vault", args: []string{"--name", "secret"}},
		{name: "empty name", args: []string{"--name=", "--vault", "https://custom.vault.example/"}},
		{name: "empty vault", args: []string{"--name", "secret", "--vault="}},
		{name: "missing both"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := runWithClient(test.args, &fakeSecretGetter{})
			if code == 0 || stdout != "" || stderr == "" {
				t.Fatalf("run() = (%d, %q, %q), want nonzero, empty stdout, diagnostic", code, stdout, stderr)
			}
		})
	}
}

func TestValidateVaultURI(t *testing.T) {
	tests := []struct {
		value string
		valid bool
	}{
		{value: "https://production.vault.azure.net/", valid: true},
		{value: "https://vault.example.test:8443/", valid: true},
		{value: "http://vault.example.test/", valid: false},
		{value: "https://", valid: false},
		{value: "not a uri", valid: false},
		{value: "https://user:password@vault.example.test/", valid: false},
		{value: "https://vault.example.test/?api-version=1", valid: false},
		{value: "https://vault.example.test/#secret", valid: false},
		{value: "https://vault.example.test/#", valid: false},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			err := validateVaultURI(test.value)
			if (err == nil) != test.valid {
				t.Fatalf("validateVaultURI(%q) error = %v, want valid=%t", test.value, err, test.valid)
			}
		})
	}
}

func TestRunHandlesSecretNotFoundAsEmptySuccess(t *testing.T) {
	getter := &fakeSecretGetter{err: &azcore.ResponseError{
		StatusCode: 404,
		ErrorCode:  "SecretNotFound",
	}}
	code, stdout, stderr := runWithClient([]string{
		"--name", "missing", "--vault", "https://custom.vault.example/",
	}, getter)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("run() = (%d, %q, %q), want (0, empty, empty)", code, stdout, stderr)
	}
}

func TestRunReportsOperationalResponseErrorsWithoutLeakingDetails(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		errorCode  string
		want       string
	}{
		{name: "authentication", statusCode: 401, want: "authentication failed"},
		{name: "authorization", statusCode: 403, want: "authorization failed"},
		{name: "rate limit", statusCode: 429, want: "rate limit"},
		{name: "service", statusCode: 503, want: "service failure"},
		{name: "unexpected not found", statusCode: 404, errorCode: "VaultNotFound", want: "unexpected HTTP response"},
		{name: "not found error code with wrong status", statusCode: 403, errorCode: "SecretNotFound", want: "authorization failed"},
		{name: "malformed HTTP 200 response", statusCode: 200, want: "malformed response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			getter := &fakeSecretGetter{err: &azcore.ResponseError{
				StatusCode:  test.statusCode,
				ErrorCode:   test.errorCode,
				RawResponse: nil,
			}}
			code, stdout, stderr := runWithClient([]string{
				"--name", "secret", "--vault", "https://custom.vault.example/",
			}, getter)
			if code == 0 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Fatalf("run() = (%d, %q, %q), want nonzero, empty stdout, diagnostic containing %q", code, stdout, stderr, test.want)
			}
		})
	}
}

func TestRunReportsNetworkTimeoutAndMalformedValues(t *testing.T) {
	tests := []struct {
		name   string
		getter *fakeSecretGetter
		want   string
	}{
		{
			name:   "network error",
			getter: &fakeSecretGetter{err: &net.DNSError{Err: "lookup failed"}},
			want:   "network failure",
		},
		{
			name:   "timeout error",
			getter: &fakeSecretGetter{err: context.DeadlineExceeded},
			want:   "timeout failure",
		},
		{
			name:   "malformed response",
			getter: &fakeSecretGetter{},
			want:   "malformed Key Vault response",
		},
		{
			name:   "empty value",
			getter: &fakeSecretGetter{response: responseWithValue("")},
			want:   "malformed Key Vault response",
		},
		{
			name:   "multiline value",
			getter: &fakeSecretGetter{response: responseWithValue("secret\nsecond line")},
			want:   "single line",
		},
		{
			name:   "carriage return value",
			getter: &fakeSecretGetter{response: responseWithValue("secret\rvalue")},
			want:   "single line",
		},
		{
			name: "error containing token and secret",
			getter: &fakeSecretGetter{err: errors.New(
				"access_token=token-never-print secret-value-never-print",
			)},
			want: "secret retrieval failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := runWithClient([]string{
				"--name", "secret", "--vault", "https://custom.vault.example/",
			}, test.getter)
			if code == 0 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Fatalf("run() = (%d, %q, %q), want nonzero, empty stdout, diagnostic containing %q", code, stdout, stderr, test.want)
			}
			if strings.Contains(stderr, "token-never-print") || strings.Contains(stderr, "secret-value-never-print") ||
				strings.Contains(stderr, "secret\nsecond line") || strings.Contains(stderr, "secret\rvalue") {
				t.Fatalf("diagnostic disclosed sensitive value: %q", stderr)
			}
		})
	}
}

func TestRunDoesNotExposeClientFactoryError(t *testing.T) {
	var stdout, stderr strings.Builder
	factory := func(string) (secretGetter, error) {
		return nil, errors.New("credential token-must-not-appear")
	}
	code := run([]string{
		"--name", "secret", "--vault", "https://custom.vault.example/",
	}, &stdout, &stderr, factory, time.Second)
	if code == 0 || stdout.String() != "" || !strings.Contains(stderr.String(), "authentication failed") ||
		strings.Contains(stderr.String(), "token-must-not-appear") {
		t.Fatalf("run() = (%d, %q, %q), want sanitized authentication error", code, stdout.String(), stderr.String())
	}
}

func TestRunForwardsNameAndSuppliedVault(t *testing.T) {
	getter := &fakeSecretGetter{response: responseWithValue("ok")}
	var receivedVault string
	factory := func(vault string) (secretGetter, error) {
		receivedVault = vault
		return getter, nil
	}
	var stdout, stderr strings.Builder
	code := run([]string{
		"--name", "forwarded-name", "--vault", "https://unmodified.example/path",
	}, &stdout, &stderr, factory, time.Second)
	if code != 0 || receivedVault != "https://unmodified.example/path" || getter.name != "forwarded-name" {
		t.Fatalf("run() = (%d, vault %q, name %q), want supplied values forwarded", code, receivedVault, getter.name)
	}
}

func TestRunSupportsHelpAndVersion(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
		out  bool
	}{
		{name: "help", args: []string{"--help"}, want: "Usage: dsckvsec", out: false},
		{name: "version", args: []string{"--version"}, want: version, out: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			code := run(test.args, &stdout, &stderr, func(string) (secretGetter, error) {
				t.Fatal("client must not be created for help or version")
				return nil, nil
			}, time.Second)
			actual := stderr.String()
			if test.out {
				actual = stdout.String()
			}
			if code != 0 || !strings.Contains(actual, test.want) {
				t.Fatalf("run() = (%d, %q, %q), want success containing %q", code, stdout.String(), stderr.String(), test.want)
			}
		})
	}
}
