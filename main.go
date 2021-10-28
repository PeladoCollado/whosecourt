package main

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/google/go-github/github"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"os"
	"regexp"
	"strings"
)

const APP_ID = 147975

const REVIEWER_COURT = "reviewers_court"
const AUTHOR_COURT = "authors_court"

var ReviewerLabels = map[string]bool{REVIEWER_COURT: true, AUTHOR_COURT: true}

var courtLabels = make(map[string]*github.Label)
var labelIds = make(map[int64]bool)

var log *zap.SugaredLogger
var pem *rsa.PrivateKey
var courtCommentRegex *regexp.Regexp

func init() {
	z, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to initialize logger- %v", err)
		os.Exit(1)
	}
	log = z.Sugar()

	courtCommentRegex, err = regexp.Compile("<!-- ([\\w_]+) -->")
	if err != nil {
		log.Fatalf("Can't compile the regex! 🤷🏽‍♂️%v", err)
	}

	loadPemBytes()

}

func main() {
	lambda.Start(handleEvent)
}

func handleEvent(ctx context.Context, r events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	log.Debugf("Received new %s event- %v", r.HTTPMethod, r.Headers)
	var err error
	var client *github.Client
	if r.HTTPMethod == "POST" {
		switch githubEvent := r.Headers["x-github-event"]; githubEvent {
		case "pull_request":
			event := &github.PullRequestEvent{}

			err = json.Unmarshal([]byte(r.Body), event)
			if err != nil {
				log.Error("Unable to parse json - %v", err)
				return events.APIGatewayProxyResponse{}, errors.Wrap(err, "Invalid JSON received")
			}

			client, err = initClientForInstallation(ctx, event.Installation.ID)
			if err != nil {
				return events.APIGatewayProxyResponse{}, errors.Wrap(err, "Unable to connect to github")
			}
			err = loadLabels(context.Background(), event.Repo, client)
			if err != nil {
				log.Error("Unable to load labels for repository- %v", err)
				return events.APIGatewayProxyResponse{}, errors.Wrap(err, "Unable to load labels for repository")
			}
			action := *event.Action
			if action == "review_requested" || action == "opened" || action == "reopened" {
				if event.RequestedReviewer != nil || len(event.PullRequest.RequestedReviewers) > 0 {
					err = changeCourt(ctx, REVIEWER_COURT, event.PullRequest, client)
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
				err = changeCourt(ctx, court, event.PullRequest, client)
			}
		case "pull_request_review":
			event := &github.PullRequestReviewEvent{}
			err = json.Unmarshal([]byte(r.Body), event)
			if err != nil {
				return events.APIGatewayProxyResponse{StatusCode: 400, Body: fmt.Sprintf("Unable to decode JSON - %v", err)}, nil
			}
			client, err := initClientForInstallation(ctx, event.Installation.ID)
			if err != nil {
				return events.APIGatewayProxyResponse{}, errors.Wrap(err, "Unable to connect to github")
			}
			err = changeCourt(ctx, REVIEWER_COURT, event.PullRequest, client)
		case "pull_request_review_comment":
			event := &github.PullRequestReviewCommentEvent{}
			err = json.Unmarshal([]byte(r.Body), event)
			if err != nil {
				log.Error("Unable to parse json - %v", err)
				return events.APIGatewayProxyResponse{}, errors.Wrap(err, "Invalid JSON received")
			}

			client, err := initClientForInstallation(ctx, event.Installation.ID)
			if err != nil {
				return events.APIGatewayProxyResponse{}, errors.Wrap(err, "Unable to connect to github")
			}
			err = loadLabels(context.Background(), event.Repo, client)
			if err != nil {
				log.Error("Unable to load labels for repository- %v", err)
				return events.APIGatewayProxyResponse{}, errors.Wrap(err, "Unable to load labels for repository")
			}

			// support label updates in comments, such as <!-- reviewers_court --> or <!-- authors_court -->
			if courtCommentRegex.Match([]byte(*event.Comment.Body)) {
				labels := courtCommentRegex.FindStringSubmatch(*event.Comment.Body)
				if len(labels) > 1 {
					err = changeCourt(ctx, labels[1], event.PullRequest, client)
				}
			}
		}
	}
	if err != nil {
		log.Errorf("Error handling request %v", err)
		return events.APIGatewayProxyResponse{StatusCode: 500, Body: err.Error(), IsBase64Encoded: false}, err
	} else {
		return events.APIGatewayProxyResponse{StatusCode: 200}, nil
	}
}

func loadLabels(ctx context.Context, repo *github.Repository, client *github.Client) error {
	if len(labelIds) == len(ReviewerLabels) {
		return nil
	}
	owner, repoName, err2 := getRepoOwner(repo)
	if err2 != nil {
		return err2
	}
	for l, _ := range ReviewerLabels {
		label, resp, err := client.Issues.GetLabel(ctx, owner, repoName, l)

		// if label doesn't exist, create it
		if resp != nil && resp.StatusCode == 404 {
			log.Infof("Unable to find label %s. Attempting to create it", l)
			label, resp, err = client.Issues.CreateLabel(ctx, owner, repoName, &github.Label{Name: &l})
			if err != nil {
				return errors.Wrap(err, fmt.Sprintf("Unable to create label %s", l))
			}
		} else if err != nil {
			return err
		}
		labelIds[*label.ID] = true
		courtLabels[*label.Name] = label
	}
	log.Info("Found labels %v", courtLabels)
	return nil
}

func getRepoOwner(repo *github.Repository) (string, string, error) {
	var owner string
	var repoName string
	if repo.Owner.Name != nil {
		owner = *repo.Owner.Name
		repoName = *repo.Name
	} else if strings.Count(*repo.FullName, "/") == 1 {
		parts := strings.Split(*repo.FullName, "/")
		owner = parts[0]
		repoName = parts[1]
	} else {
		return "", "", fmt.Errorf("can't determine repository information from event %v", *repo)
	}
	return owner, repoName, nil
}

func changeCourt(ctx context.Context, court string, pr *github.PullRequest, client *github.Client) error {
	log.Infof("Changing %d's court to %s", pr.ID, court)
	var oldCourt string
	if court == REVIEWER_COURT {
		oldCourt = AUTHOR_COURT
	} else {
		oldCourt = REVIEWER_COURT
	}

	owner, repoName, err := getRepoOwner(pr.GetBase().Repo)
	if err != nil {
		return err
	}
	resp, err := client.Issues.RemoveLabelForIssue(ctx, owner, repoName, *pr.Number, oldCourt)
	if err != nil && (resp == nil || resp.StatusCode != 404) {
		return err
	}
	_, _, err = client.Issues.AddLabelsToIssue(ctx, owner, repoName, *pr.Number, []string{court})
	return err
}
