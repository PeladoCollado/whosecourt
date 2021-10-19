package main

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"github.com/google/go-github/github"
	"github.com/dgrijalva/jwt-go"
	"go.uber.org/zap"
	"net/http"
	"os"
	"time"
	"golang.org/x/oauth2"
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

func init() {
	pemBytes := []byte(os.Getenv("PEM"))
	var err error
	pem, err = jwt.ParseRSAPrivateKeyFromPEM(pemBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr,"Invalid PEM content- unable to parse %w", err)
		os.Exit(1)
	}
}

type TokenSource struct {

}

func (t TokenSource) Token() (*oauth2.Token, error) {
	claim := jwt.StandardClaims{
		Issuer: APP_ID,
		IssuedAt: time.Now().Unix(),
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

	// set up the github client with auth
	httpClient := oauth2.NewClient(context.Background(), TokenSource{})
	client = github.NewClient(httpClient)

	// handle incoming events
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		ctx := context.Background()
		log.Debugf("Received new %s event- %v", r.Method, r.Header)
		if r.Method == "POST" {
			switch githubEvent := r.Header.Get("X-GitHub-Event"); githubEvent {
			case "pull_request":
				event := &github.PullRequestEvent{}

				loadLabels(context.Background(), *event.Repo.Owner.Name, *event.Repo.Owner.Name)
				decoder.Decode(event)
				action := *event.Action
				if action == "review_requested" || action == "opened" {
					if event.RequestedReviewer != nil || len(event.PullRequest.RequestedReviewers) > 0 {
						changeCourt(ctx, REVIEWER_COURT, event.PullRequest)
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
					changeCourt(ctx, court, event.PullRequest)
				}
			case "pull_request_review":
				event := &github.PullRequestReviewEvent{}
				err := decoder.Decode(event)
				if err != nil {
					w.WriteHeader(400)
					w.Write([]byte(fmt.Sprintf("Unable to decode JSON - %w", err)))
				}
				changeCourt(ctx, REVIEWER_COURT, event.PullRequest)
			case "pull_request_review_comment":


			}
		}
	})
	http.ListenAndServe("localhost:8080", http.DefaultServeMux)
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