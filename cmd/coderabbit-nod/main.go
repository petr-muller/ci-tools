package main

import (
	"flag"
	"os"
	"time"

	"github.com/sirupsen/logrus"

	prowconfig "sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/config/secret"
	prowflagutil "sigs.k8s.io/prow/pkg/flagutil"
	"sigs.k8s.io/prow/pkg/githubeventserver"
	"sigs.k8s.io/prow/pkg/interrupts"
	"sigs.k8s.io/prow/pkg/logrusutil"
	"sigs.k8s.io/prow/pkg/pjutil"
	"sigs.k8s.io/prow/pkg/pluginhelp"
)

type options struct {
	webhookSecretFile string

	githubEventServerOptions githubeventserver.Options
	github                   prowflagutil.GitHubOptions

	dryRun bool
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	fs.StringVar(&o.webhookSecretFile, "hmac-secret-file", "", "Path to the file containing the GitHub HMAC secret.")
	fs.BoolVar(&o.dryRun, "dry-run", true, "Run in dry-run mode (do not mutate GitHub state).")

	o.github.AddFlags(fs)
	o.githubEventServerOptions.Bind(fs)

	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatalf("cannot parse args: '%s'", os.Args[1:])
	}
	return o
}

func (o *options) Validate() error {
	if err := o.github.Validate(o.dryRun); err != nil {
		return err
	}
	if err := o.githubEventServerOptions.DefaultAndValidate(); err != nil {
		return err
	}
	return nil
}

func helpProvider(_ []prowconfig.OrgRepo) (*pluginhelp.PluginHelp, error) {
	return &pluginhelp.PluginHelp{
		Description: "The coderabbit-nod plugin manages the coderabbit-nod label on PRs based on CodeRabbit review status. The label is present when CodeRabbit has approved the PR or its review has been dismissed, and absent when CodeRabbit has actionable feedback or new commits have been pushed.",
	}, nil
}

func main() {
	logrusutil.ComponentInit()
	logger := logrus.WithField("plugin", "coderabbit-nod")

	o := gatherOptions()
	if err := o.Validate(); err != nil {
		logger.Fatalf("Invalid options: %v", err)
	}

	if err := secret.Add(o.github.TokenPath, o.webhookSecretFile); err != nil {
		logger.WithError(err).Fatal("Error starting secrets agent.")
	}

	githubClient, err := o.github.GitHubClient(o.dryRun)
	if err != nil {
		logger.WithError(err).Fatal("Error getting GitHub client.")
	}

	serv := &server{ghc: githubClient}

	eventServer := githubeventserver.New(o.githubEventServerOptions, secret.GetTokenGenerator(o.webhookSecretFile), logger)
	eventServer.RegisterReviewEventHandler(serv.handleReviewEvent)
	eventServer.RegisterHandlePullRequestEvent(serv.handlePullRequestEvent)
	eventServer.RegisterHelpProvider(helpProvider, logger)

	health := pjutil.NewHealth()
	health.ServeReady()

	interrupts.ListenAndServe(eventServer, time.Second*30)
	interrupts.WaitForGracefulShutdown()
}
