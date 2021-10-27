package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/dgrijalva/jwt-go"
	"github.com/google/go-github/github"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"io/ioutil"
	"os"
	"regexp"
	"time"
)

const APP_ID = "myAppId"

const REVIEWER_COURT = "reviewers_court"
const AUTHOR_COURT = "authors_court"

var ReviewerLabels = []string{REVIEWER_COURT, AUTHOR_COURT}

var courtLabels = make(map[string]*github.Label)
var labelIds = make(map[int64]bool)

var client *github.Client
var log *zap.SugaredLogger
var pem *rsa.PrivateKey
var courtCommentRegex *regexp.Regexp

type TokenSource struct {
}

func (t TokenSource) Token() (*oauth2.Token, error) {
	claim := jwt.StandardClaims{
		Issuer:    APP_ID,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}
	token := jwt.NewWithClaims(jwt.GetSigningMethod("RS256"), claim)
	signedJwt, err := token.SignedString(pem)

	return &oauth2.Token{
		AccessToken: signedJwt,
	}, err
}

func main() {
	z, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to initialize logger- %w", err)
		os.Exit(1)
	}
	log = z.Sugar()

	courtCommentRegex, err = regexp.Compile("<!-- ([\\w_]+) -->")
	if err != nil {
		log.Fatalf("Can't compile the regex! 🤷🏽‍♂️%w", err)
	}

	loadPemBytes()

	// set up the github client with auth
	httpClient := oauth2.NewClient(context.Background(), TokenSource{})
	client = github.NewClient(httpClient)

	lambda.Start(handleEvent)
}

func loadPemBytes() {
	var pemBytes []byte
	if pem := os.Getenv("PEM"); pem != "" {
		pemBytes = []byte(pem)
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
		fmt.Fprintf(os.Stderr, "Invalid PEM content- unable to parse %w", err)
		os.Exit(1)
	}
	log.Info("Initialized private key")
}

func handleEvent(ctx context.Context, r events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(r.Body))
	log.Debugf("Received new %s event- %v", r.HTTPMethod, r.Headers)
	var err error
	if r.HTTPMethod == "POST" {
		switch githubEvent := r.Headers["X-GitHub-Event"]; githubEvent {
		case "pull_request":
			event := &github.PullRequestEvent{}

			loadLabels(context.Background(), *event.Repo.Owner.Name, *event.Repo.Owner.Name)
			decoder.Decode(event)
			action := *event.Action
			if action == "review_requested" || action == "opened" || action == "reopened" {
				if event.RequestedReviewer != nil || len(event.PullRequest.RequestedReviewers) > 0 {
					err = changeCourt(ctx, REVIEWER_COURT, event.PullRequest)
				}
			} else if action == "unlabeled" {
				courtLabelPresent := false
				for i := range event.PullRequest.Labels {
					if labelIds[*event.PullRequest.Labels[i].ID] {
						courtLabelPresent = true
						break
					}
				}
				// the PR was manually labeled
				if courtLabelPresent {
					log.Info("PR was manually labeled- no action taken")
					break
				}

				// author has unlabeled- assume reviewer's court
				var court string
				if event.Sender.ID == event.PullRequest.User.ID {
					court = REVIEWER_COURT
				} else {
					court = AUTHOR_COURT
				}
				err = changeCourt(ctx, court, event.PullRequest)
			}
		case "pull_request_review":
			event := &github.PullRequestReviewEvent{}
			err = decoder.Decode(event)
			if err != nil {
				return events.APIGatewayProxyResponse{StatusCode: 400, Body: fmt.Sprintf("Unable to decode JSON - %w", err)}, nil
			}
			err = changeCourt(ctx, REVIEWER_COURT, event.PullRequest)
		case "pull_request_review_comment":
			event := &github.PullRequestReviewCommentEvent{}
			loadLabels(context.Background(), *event.Repo.Owner.Name, *event.Repo.Owner.Name)
			decoder.Decode(event)

			// support label updates in comments, such as <!-- reviewers_court --> or <!-- authors_court -->
			if courtCommentRegex.Match([]byte(*event.Comment.Body)) {
				labels := courtCommentRegex.FindStringSubmatch(*event.Comment.Body)
				if len(labels) > 1 {
					err = changeCourt(ctx, labels[1], event.PullRequest)
				}
			}
		}
	}
	if err != nil {
		log.Errorf("Error handling request %w", err)
		return events.APIGatewayProxyResponse{StatusCode: 500, Body: err.Error(), IsBase64Encoded: false}, err
	} else {
		return events.APIGatewayProxyResponse{StatusCode: 200}, nil
	}
}

func loadLabels(ctx context.Context, owner string, repo string) error {
	if len(labelIds) == len(ReviewerLabels) {
		return nil
	}
	for _, l := range ReviewerLabels {
		label, _, err := client.Issues.GetLabel(ctx, owner, repo, l)
		if err != nil {
			return err
		}
		labelIds[*label.ID] = true
		courtLabels[*label.Name] = label
	}
	return nil
}

func changeCourt(ctx context.Context, court string, pr *github.PullRequest) error {
	log.Infof("Changing %d's court to %s", pr.ID, court)
	newLabels := make([]*github.Label, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		if _, ok := labelIds[*l.ID]; ok {
			if *l.Name == court {
				log.Infof("Review %d already has target label %s", pr.ID, court)
				return nil
			} else {
				newLabels = append(newLabels, courtLabels[court])
			}
		} else {
			newLabels = append(newLabels, l)
		}
	}
	pr.Labels = newLabels
	_, _, err := client.PullRequests.Edit(ctx, *pr.GetBase().Repo.Owner.Name, *pr.GetBase().Repo.Name, *pr.Number, pr)
	return err
}
