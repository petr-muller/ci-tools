package main

import (
	"flag"
	"fmt"
	"github.com/openshift/ci-tools/pkg/periodics"
	"os"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	prowconfig "k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/flagutil"

	"github.com/openshift/ci-tools/pkg/rehearse"
	"github.com/openshift/ci-tools/pkg/util"
)

type options struct {
	dryRun bool

	prowConfigPath string
	jobConfigPath  string

	periodics flagutil.Strings
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	fs.BoolVar(&o.dryRun, "dry-run", true, "Whether to actually submit jobs to Prow")

	fs.StringVar(&o.prowConfigPath, "prow-config-path", "", "Path to Prow config file")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to Prow job config file or directory")

	fs.Var(&o.periodics, "periodic", "Periodic jobs that will be triggered, provide one or more times.")

	fs.Parse(os.Args[1:])
	return o
}

func validateOptions(o options) error {
	if len(o.prowConfigPath) == 0 {
		return fmt.Errorf("required flag --prow-config-path was unset")
	}
	if len(o.jobConfigPath) == 0 {
		return fmt.Errorf("required flag --job-config-path was unset")
	}
	if len(o.periodics.Strings()) == 0 {
		return fmt.Errorf("at least one job needs to be specified with --periodic flag")
	}

	return nil
}

func main() {
	o := gatherOptions()

	err := validateOptions(o)
	if err != nil {
		logrus.WithError(err).Fatal("invalid options")
	}

	logger := logrus.New()

	var clusterConfig *rest.Config
	if !o.dryRun {
		clusterConfig, err = util.LoadClusterConfig()
		if err != nil {
			logger.WithError(err).Fatal("could not load cluster clusterConfig")
		}
	}

	prowConfig, err := prowconfig.Load(o.prowConfigPath, o.jobConfigPath)
	if err != nil {
		logger.WithError(err).Fatal("failed to load Prow configuration")
	}

	if err := prowConfig.ValidateJobConfig(); err != nil {
		logger.WithError(err).Fatal("jobconfig validation failed")
	}

	pjclient, err := rehearse.NewProwJobClient(clusterConfig, prowConfig.ProwJobNamespace, o.dryRun)
	if err != nil {
		logger.WithError(err).Fatal("could not create a ProwJob client")
	}

	jobs, err := getJobs(sets.NewString(o.periodics.Strings()...), prowConfig.Periodics)

	executor := periodics.NewExecutor(jobs, o.dryRun, pjclient)
	success, err := executor.Do()
	if err != nil {
		logger.WithError(err).Fatal("Failed to execute some jobs")
	}
	if !success {
		logger.Fatal("All jobs ran, but some failed")
	}
	logger.Info("All jobs ran successfully")
}

func getJobs(names sets.String, allJobs []prowconfig.Periodic) (jobs []prowconfig.Periodic, err error) {
	byName := map[string]prowconfig.Periodic{}
	for _, job := range allJobs {
		byName[job.Name] = job
	}

	for name := range names {
		job, exists := byName[name]
		if !exists {
			return nil, fmt.Errorf("no such job: %s", name)
		}
		jobs = append(jobs, job)
	}

	return
}
