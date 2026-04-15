package main

import (
	"strings"

	"github.com/sirupsen/logrus"

	"sigs.k8s.io/prow/pkg/github"
)

const (
	coderabbitBotLogin = "coderabbitai[bot]"
	coderabbitLabel    = "coderabbit-nod"
)

type githubClient interface {
	AddLabel(org, repo string, number int, label string) error
	RemoveLabel(org, repo string, number int, label string) error
}

type server struct {
	ghc githubClient
}

func (s *server) handleReviewEvent(l *logrus.Entry, re github.ReviewEvent) {
	if re.Review.User.Login != coderabbitBotLogin {
		return
	}

	org := re.Repo.Owner.Login
	repo := re.Repo.Name
	number := re.PullRequest.Number
	logger := l.WithFields(logrus.Fields{
		github.OrgLogField:  org,
		github.RepoLogField: repo,
		github.PrLogField:   number,
	})

	shouldHaveLabel := (re.Action == github.ReviewActionSubmitted && re.Review.State == github.ReviewStateApproved) ||
		re.Action == github.ReviewActionDismissed
	hasLabel := prHasLabel(re.PullRequest, coderabbitLabel)

	if shouldHaveLabel == hasLabel {
		return
	}

	if shouldHaveLabel {
		if err := s.ghc.AddLabel(org, repo, number, coderabbitLabel); err != nil {
			logger.WithError(err).Error("failed to add coderabbit-nod label")
		}
	} else {
		if err := s.ghc.RemoveLabel(org, repo, number, coderabbitLabel); err != nil {
			logger.WithError(err).Error("failed to remove coderabbit-nod label")
		}
	}
}

func (s *server) handlePullRequestEvent(l *logrus.Entry, pre github.PullRequestEvent) {
	if pre.Action != github.PullRequestActionSynchronize {
		return
	}

	if !prHasLabel(pre.PullRequest, coderabbitLabel) {
		return
	}

	org := pre.Repo.Owner.Login
	repo := pre.Repo.Name
	number := pre.PullRequest.Number
	logger := l.WithFields(logrus.Fields{
		github.OrgLogField:  org,
		github.RepoLogField: repo,
		github.PrLogField:   number,
	})

	if err := s.ghc.RemoveLabel(org, repo, number, coderabbitLabel); err != nil {
		logger.WithError(err).Error("failed to remove coderabbit-nod label on synchronize")
	}
}

func prHasLabel(pr github.PullRequest, label string) bool {
	for _, l := range pr.Labels {
		if strings.EqualFold(l.Name, label) {
			return true
		}
	}
	return false
}
