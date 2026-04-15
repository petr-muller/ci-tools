package main

import (
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"

	"sigs.k8s.io/prow/pkg/github"
)

type fakeClient struct {
	labelsAdded   []string
	labelsRemoved []string
}

func (c *fakeClient) AddLabel(org, repo string, number int, label string) error {
	c.labelsAdded = append(c.labelsAdded, fmt.Sprintf("%s/%s#%d:%s", org, repo, number, label))
	return nil
}

func (c *fakeClient) RemoveLabel(org, repo string, number int, label string) error {
	c.labelsRemoved = append(c.labelsRemoved, fmt.Sprintf("%s/%s#%d:%s", org, repo, number, label))
	return nil
}

func TestHandleReviewEvent(t *testing.T) {
	testCases := []struct {
		name          string
		event         github.ReviewEvent
		expectAdded   []string
		expectRemoved []string
	}{
		{
			name: "ignore review from different user",
			event: github.ReviewEvent{
				Action: github.ReviewActionSubmitted,
				Review: github.Review{
					User:  github.User{Login: "someone-else"},
					State: github.ReviewStateApproved,
				},
				PullRequest: github.PullRequest{Number: 1},
				Repo:        github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
		},
		{
			name: "ignore edited action from coderabbit",
			event: github.ReviewEvent{
				Action: github.ReviewActionEdited,
				Review: github.Review{
					User:  github.User{Login: coderabbitBotLogin},
					State: github.ReviewStateApproved,
				},
				PullRequest: github.PullRequest{Number: 1},
				Repo:        github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
		},
		{
			name: "add label on approval when absent",
			event: github.ReviewEvent{
				Action: github.ReviewActionSubmitted,
				Review: github.Review{
					User:  github.User{Login: coderabbitBotLogin},
					State: github.ReviewStateApproved,
				},
				PullRequest: github.PullRequest{Number: 42},
				Repo:        github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
			expectAdded: []string{"org/repo#42:coderabbit-nod"},
		},
		{
			name: "no-op on approval when label already present",
			event: github.ReviewEvent{
				Action: github.ReviewActionSubmitted,
				Review: github.Review{
					User:  github.User{Login: coderabbitBotLogin},
					State: github.ReviewStateApproved,
				},
				PullRequest: github.PullRequest{
					Number: 42,
					Labels: []github.Label{{Name: coderabbitLabel}},
				},
				Repo: github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
		},
		{
			name: "remove label on changes_requested when present",
			event: github.ReviewEvent{
				Action: github.ReviewActionSubmitted,
				Review: github.Review{
					User:  github.User{Login: coderabbitBotLogin},
					State: github.ReviewStateChangesRequested,
				},
				PullRequest: github.PullRequest{
					Number: 42,
					Labels: []github.Label{{Name: coderabbitLabel}},
				},
				Repo: github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
			expectRemoved: []string{"org/repo#42:coderabbit-nod"},
		},
		{
			name: "no-op on changes_requested when label absent",
			event: github.ReviewEvent{
				Action: github.ReviewActionSubmitted,
				Review: github.Review{
					User:  github.User{Login: coderabbitBotLogin},
					State: github.ReviewStateChangesRequested,
				},
				PullRequest: github.PullRequest{Number: 42},
				Repo:        github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
		},
		{
			name: "remove label on comment review when present",
			event: github.ReviewEvent{
				Action: github.ReviewActionSubmitted,
				Review: github.Review{
					User:  github.User{Login: coderabbitBotLogin},
					State: github.ReviewStateCommented,
				},
				PullRequest: github.PullRequest{
					Number: 42,
					Labels: []github.Label{{Name: coderabbitLabel}},
				},
				Repo: github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
			expectRemoved: []string{"org/repo#42:coderabbit-nod"},
		},
		{
			name: "add label on dismissed review when absent",
			event: github.ReviewEvent{
				Action: github.ReviewActionDismissed,
				Review: github.Review{
					User: github.User{Login: coderabbitBotLogin},
				},
				PullRequest: github.PullRequest{Number: 42},
				Repo:        github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
			expectAdded: []string{"org/repo#42:coderabbit-nod"},
		},
		{
			name: "no-op on dismissed review when label already present",
			event: github.ReviewEvent{
				Action: github.ReviewActionDismissed,
				Review: github.Review{
					User: github.User{Login: coderabbitBotLogin},
				},
				PullRequest: github.PullRequest{
					Number: 42,
					Labels: []github.Label{{Name: coderabbitLabel}},
				},
				Repo: github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{}
			s := &server{ghc: fc}
			s.handleReviewEvent(logrus.NewEntry(logrus.StandardLogger()), tc.event)

			if diff := diffStringSlices(tc.expectAdded, fc.labelsAdded); diff != "" {
				t.Errorf("labels added mismatch: %s", diff)
			}
			if diff := diffStringSlices(tc.expectRemoved, fc.labelsRemoved); diff != "" {
				t.Errorf("labels removed mismatch: %s", diff)
			}
		})
	}
}

func TestHandlePullRequestEvent(t *testing.T) {
	testCases := []struct {
		name          string
		event         github.PullRequestEvent
		expectRemoved []string
	}{
		{
			name: "ignore non-synchronize action",
			event: github.PullRequestEvent{
				Action:      github.PullRequestActionOpened,
				PullRequest: github.PullRequest{Number: 1},
				Repo:        github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
		},
		{
			name: "no-op on synchronize when label absent",
			event: github.PullRequestEvent{
				Action:      github.PullRequestActionSynchronize,
				PullRequest: github.PullRequest{Number: 42},
				Repo:        github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
		},
		{
			name: "remove label on synchronize when present",
			event: github.PullRequestEvent{
				Action: github.PullRequestActionSynchronize,
				PullRequest: github.PullRequest{
					Number: 42,
					Labels: []github.Label{{Name: coderabbitLabel}},
				},
				Repo: github.Repo{Owner: github.User{Login: "org"}, Name: "repo"},
			},
			expectRemoved: []string{"org/repo#42:coderabbit-nod"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{}
			s := &server{ghc: fc}
			s.handlePullRequestEvent(logrus.NewEntry(logrus.StandardLogger()), tc.event)

			if diff := diffStringSlices(nil, fc.labelsAdded); diff != "" {
				t.Errorf("unexpected labels added: %s", diff)
			}
			if diff := diffStringSlices(tc.expectRemoved, fc.labelsRemoved); diff != "" {
				t.Errorf("labels removed mismatch: %s", diff)
			}
		})
	}
}

func diffStringSlices(want, got []string) string {
	if len(want) == 0 && len(got) == 0 {
		return ""
	}
	if fmt.Sprintf("%v", want) == fmt.Sprintf("%v", got) {
		return ""
	}
	return fmt.Sprintf("want %v, got %v", want, got)
}
