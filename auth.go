package main

import (
	"context"
	"fmt"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/go-github/github"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	"io/ioutil"
	"os"
	"strconv"
	"time"
)

type AppTokenSource struct {
}

func (t AppTokenSource) Token() (*oauth2.Token, error) {
	claim := jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(APP_ID, 10),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claim)
	signedJwt, err := token.SignedString(pem)

	return &oauth2.Token{
		AccessToken: signedJwt,
		TokenType: "Bearer",
	}, err
}

type InstallationTokenSource struct {
	ctx context.Context
	installId *int64
	client *github.Client
	accessTokenUrl string
}

func newInstallationTokenSource(ctx context.Context, installId *int64) (*InstallationTokenSource, error) {
	httpClient := oauth2.NewClient(ctx, AppTokenSource{})
	ghClient := github.NewClient(httpClient)
	install, _, err := ghClient.Apps.GetInstallation(ctx, *installId)
	if err != nil {
		return nil, err
	}

	return &InstallationTokenSource{
		ctx: ctx,
		installId: installId,
		client: github.NewClient(httpClient),
		accessTokenUrl : install.GetAccessTokensURL(),
	}, nil
}

func (t InstallationTokenSource) Token() (*oauth2.Token, error) {
	req, err := t.client.NewRequest("POST", t.accessTokenUrl, nil)
	if err != nil {
		return nil, err
	}

	token := &github.InstallationToken{}
	resp, err := t.client.Do(t.ctx, req, token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		respBody, _ := ioutil.ReadAll(resp.Body)
		return nil, errors.Errorf("Bad status code returned for access token url- %d %s", resp.StatusCode, respBody)
	}

	return &oauth2.Token{
		AccessToken: *token.Token,
		Expiry: *token.ExpiresAt,
		TokenType: "token",
	}, nil
}

func loadPemBytes() {
	var pemBytes []byte
	if pemstring := os.Getenv("PEM"); pemstring != "" {
		pemBytes = []byte(pemstring)
	} else if pemfile := os.Getenv("PEMFILE"); pemfile != "" {
		file, err := os.Open(pemfile)
		if err != nil {
			log.Fatalf("Unable to find pemfile at %s: %w", pemfile, err)
		}
		pemBytes, err = ioutil.ReadAll(file)
		if err != nil {
			log.Fatalf("Unable to read pem bytes from file %s- %w", pemfile, err)
		}
	}
	var err error
	pem, err = jwt.ParseRSAPrivateKeyFromPEM(pemBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid PEM content- unable to parse %v", err)
		os.Exit(1)
	}
	log.Info("Initialized private key")
}

func initClientForInstallation(ctx context.Context, installId *int64) (*github.Client, error) {
	source, err := newInstallationTokenSource(ctx, installId)
	if err != nil {
		return nil, err
	}
	client := oauth2.NewClient(ctx, source)
	return github.NewClient(client), nil
}

