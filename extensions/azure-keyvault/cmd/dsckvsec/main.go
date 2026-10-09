package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

const (
	version        = "0.1.0"
	requestTimeout = 30 * time.Second
)

type secretGetter interface {
	GetSecret(context.Context, string, string, *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error)
}

type newSecretGetter func(string) (secretGetter, error)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, newAzureSecretGetter, requestTimeout))
}

func run(args []string, stdout, stderr io.Writer, newGetter newSecretGetter, timeout time.Duration) int {
	flags := flag.NewFlagSet("dsckvsec", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "name of the secret to retrieve")
	vault := flags.String("vault", "", "absolute HTTPS URI of the Azure Key Vault")
	showVersion := flags.Bool("version", false, "print the version")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: dsckvsec --name <secret-name> --vault <vault-uri>")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.Usage()
			return 0
		}
		return reportError(stderr, "invalid arguments")
	}
	if flags.NArg() != 0 {
		return reportError(stderr, "invalid arguments")
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	if *name == "" {
		return reportError(stderr, "--name is required")
	}
	if *vault == "" {
		return reportError(stderr, "--vault is required")
	}
	if err := validateVaultURI(*vault); err != nil {
		return reportError(stderr, "invalid --vault URI: expected an absolute HTTPS Key Vault URI without userinfo, query, or fragment")
	}

	client, err := newGetter(*vault)
	if err != nil {
		return reportError(stderr, "authentication failed while creating the Key Vault client")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	response, err := client.GetSecret(ctx, *name, "", nil)
	if err != nil {
		if isSecretNotFound(err) {
			return 0
		}
		return reportError(stderr, describeError(ctx, err))
	}
	if response.Value == nil || *response.Value == "" {
		return reportError(stderr, "malformed Key Vault response: secret value is empty or missing")
	}
	if strings.ContainsAny(*response.Value, "\r\n") {
		return reportError(stderr, "unsupported secret value: DSC secret values must be a single line")
	}
	if _, err := io.WriteString(stdout, *response.Value); err != nil {
		return reportError(stderr, "failed to write secret value to stdout")
	}
	return 0
}

func newAzureSecretGetter(vaultURI string) (secretGetter, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	return azsecrets.NewClient(vaultURI, credential, nil)
}

func validateVaultURI(value string) error {
	if strings.ContainsAny(value, "?#") {
		return errors.New("query strings and fragments are not allowed")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if !parsed.IsAbs() || parsed.Opaque != "" || parsed.Scheme != "https" ||
		parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("vault URI must be an absolute HTTPS URI")
	}
	return nil
}

func isSecretNotFound(err error) bool {
	var responseError *azcore.ResponseError
	return errors.As(err, &responseError) &&
		responseError.StatusCode == 404 &&
		responseError.ErrorCode == "SecretNotFound"
}

func describeError(ctx context.Context, err error) string {
	var authenticationError *azidentity.AuthenticationFailedError
	if errors.As(err, &authenticationError) {
		return "authentication failed"
	}
	var responseError *azcore.ResponseError
	if errors.As(err, &responseError) {
		switch {
		case responseError.StatusCode == 401:
			return "authentication failed"
		case responseError.StatusCode == 403:
			return "authorization failed"
		case responseError.StatusCode == 429:
			return "Key Vault rate limit exceeded"
		case responseError.StatusCode >= 500:
			return "Key Vault service failure"
		case responseError.StatusCode == 200:
			return "malformed response from Key Vault"
		default:
			return "Key Vault returned an unexpected HTTP response"
		}
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return "network or timeout failure while contacting Key Vault"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return "network failure while contacting Key Vault"
	}
	return "secret retrieval failed"
}

func reportError(stderr io.Writer, message string) int {
	diagnostic, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: message})
	_, _ = fmt.Fprintln(stderr, string(diagnostic))
	return 1
}
