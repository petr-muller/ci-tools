package main

import (
	"errors"
	"flag"
	"io/ioutil"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/logrusutil"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
	"github.com/openshift/ci-tools/pkg/gitsync"
	"github.com/openshift/ci-tools/pkg/promotion"
)

type options struct {
	promotion promotion.Options
	git       gitsync.Options
	targetOrg string
}

func (o *options) Validate() error {
	if err := o.promotion.Validate(); err != nil {
		return err
	}
	if o.targetOrg == "" {
		return errors.New("--target-org is required")
	}
	if o.promotion.Org == "" {
		return errors.New("--org is required")
	}
	if o.promotion.Confirm {
		if o.git.Username == "" {
			return errors.New("--username is required with --confirm")
		}
		if o.git.TokenPath == "" {
			return errors.New("--token-path is required with --confirm")
		}
	}

	return nil
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	o.promotion.Bind(fs)
	o.git.Bind(fs)

	fs.StringVar(&o.targetOrg, "target-org", "", "Name of the GH org holding the mirrored repos where the current branches would be synced.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("could not parse input")
	}
	return o
}

var secrets sets.String

func init() {
	secrets = sets.NewString()
	logrus.SetFormatter(logrusutil.NewCensoringFormatter(logrus.StandardLogger().Formatter, func() sets.String { return secrets }))
}

func makeFatalCheck(checked func() error, failMsg string) func() {
	return func() {
		if err := checked(); err != nil {
			logrus.WithError(err).Fatal(failMsg)
		}
	}
}

func main() {
	o := gatherOptions()
	if err := o.Validate(); err != nil {
		logrus.Fatalf("Invalid options: %v", err)
	}

	var token string
	if rawToken, err := ioutil.ReadFile(o.git.TokenPath); err != nil {
		logrus.WithError(err).Fatal("Could not read token.")
	} else {
		token = strings.TrimSpace(string(rawToken))
		secrets.Insert(token)
	}

	gitSync, err := o.git.NewSync(o.promotion.Confirm, o.git.Username, token)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to set up git synchronizer")
	}
	defer makeFatalCheck(gitSync.Clean, "Failed to clean up git synchronizer")()

	failed := false
	if err := config.OperateOnCIOperatorConfigDir(o.promotion.ConfigDir, func(configuration *api.ReleaseBuildConfiguration, repoInfo *config.Info) error {
		if o.promotion.Org != repoInfo.Org || (o.promotion.Repo != "" && o.promotion.Repo != repoInfo.Repo) {
			return nil
		}
		if !promotion.PromotesOfficialImages(configuration) {
			return nil
		}

		if err := gitSync.From(repoInfo.Org, repoInfo.Repo, repoInfo.Branch).To(o.targetOrg, repoInfo.Repo, repoInfo.Branch); err != nil {
			failed = true
		}

		return nil
	}); err != nil || failed {
		logrus.WithError(err).Fatal("Could not branch configurations.")
	}
}
