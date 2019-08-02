/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/sirupsen/logrus"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/config/secret"
	prowflagutil "k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/github"
	_ "k8s.io/test-infra/prow/hook"
	"k8s.io/test-infra/prow/plugins"
	"k8s.io/test-infra/prow/repoowners"
	"os"
)

type options struct {
	configPath       string
	jobConfigPath    string
	pluginConfigPath string

	pullNumber int
	org        string
	repo       string

	github       prowflagutil.GitHubOptions
	githubClient githubClient
}

type githubClient interface {
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	GetRef(org, repo, ref string) (string, error)
}

func (o *options) Validate() error {
	if o.configPath == "" {
		return errors.New("required flag --config-path was unset")
	}

	if err := o.github.Validate(false); err != nil {
		return err
	}

	return nil
}

func gatherOptions() options {
	var o options
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&o.configPath, "config-path", "", "Path to config.yaml.")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs.")
	fs.StringVar(&o.pluginConfigPath, "plugin-config-path", "", "Path to prow job plugin configs.")

	fs.IntVar(&o.pullNumber, "pull-number", 0, "Git pull number under test")
	fs.StringVar(&o.org, "org", "openshift", "GH Org")
	fs.StringVar(&o.repo, "repo", "release", "GH Repo")
	o.github.AddFlagsWithoutDefaultGitHubTokenPath(fs)
	fs.Parse(os.Args[1:])
	return o
}

func main() {
	o := gatherOptions()
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatalf("Bad flags")
	}

	confAgent := &config.Agent{}
	if err := confAgent.Start(o.configPath, o.jobConfigPath); err != nil {
		logrus.WithError(err).Fatal("Error loading config")
	}

	pluginConfAgent := &plugins.ConfigAgent{}
	if err := pluginConfAgent.Start(o.pluginConfigPath); err != nil {
		logrus.WithError(err).Fatal("Error starting plugin config agent")
	}

	var secretAgent *secret.Agent
	if o.github.TokenPath != "" {
		secretAgent = &secret.Agent{}
		if err := secretAgent.Start([]string{o.github.TokenPath}); err != nil {
			logrus.WithError(err).Fatal("Failed to start secret agent")
		}
	}

	gitClient, err := o.github.GitClient(secretAgent, false)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting Git client.")
	}
	defer gitClient.Clean()

	githubClient, err := o.github.GitHubClient(secretAgent, false)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to get GitHub client")
	}
	mdYAMLEnabled := func(org, repo string) bool {
		return pluginConfAgent.Config().MDYAMLEnabled(org, repo)
	}
	skipCollaborators := func(org, repo string) bool {
		return pluginConfAgent.Config().SkipCollaborators(org, repo)
	}
	ownersDirBlacklist := func() config.OwnersDirBlacklist {
		return confAgent.Config().OwnersDirBlacklist
	}
	ownersClient := repoowners.NewClient(gitClient, githubClient, mdYAMLEnabled, skipCollaborators, ownersDirBlacklist)

	clientAgent := &plugins.ClientAgent{
		GitHubClient: githubClient,
		OwnersClient: ownersClient,
	}

	pluginAgent := plugins.NewAgent(confAgent, pluginConfAgent, clientAgent, nil, logrus.NewEntry(logrus.StandardLogger()))

	changes, err := pluginAgent.GitHubClient.GetPullRequestChanges(o.org, o.repo, o.pullNumber)
	if err != nil {
		logrus.WithError(err).Fatalf("error getting PR changes: %v")
	}
	fmt.Printf("CHANGES:\n%v", changes[0].Filename)

	oc, err := pluginAgent.OwnersClient.LoadRepoOwners("petr-muller", "ci-tools", "owners-labels-experiments")
	if err != nil {
		logrus.WithError(err).Fatal("error loading RepoOwners")
	}
	fmt.Printf("LABELS:\n%v", oc.FindLabelsForFile(changes[0].Filename).List())
}
