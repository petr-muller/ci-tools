package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/getlantern/deepcopy"
	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	utilpointer "k8s.io/utils/pointer"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
)

type options struct {
	org, repo, branch, variant     string
	name, workflow, clusterProfile string
	openshiftReleasePath           string

	help bool
}

func parseOptions() *options {
	fs := flag.NewFlagSet("", flag.ExitOnError)
	opt := &options{}
	fs.StringVar(&opt.org, "org", "openshift", "Organization (default: openshift)")
	fs.StringVar(&opt.repo, "repo", "", "Repository")
	fs.StringVar(&opt.branch, "branch", "", "Branch")
	fs.StringVar(&opt.variant, "variant", "", "Variant")

	fs.StringVar(&opt.openshiftReleasePath, "openshift-release-path", "", "Path to openshift/release working copy")

	fs.StringVar(&opt.name, "name", "", "Name of the test to be added")
	fs.StringVar(&opt.workflow, "workflow", "", "Name of the workflow the test should be using")
	fs.StringVar(&opt.clusterProfile, "cluster-profile", "", "Name of the cluster profile the test should be using")

	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("Failed to parse flags")
	}
	if opt.help {
		fs.Usage()
		os.Exit(0)
	}

	return opt
}

func main() {
	opt := parseOptions()

	var variant string
	if opt.variant != "" {
		variant = fmt.Sprintf("__%s", variant)
	}
	ciopConfigDir := filepath.Join(opt.openshiftReleasePath, "ci-operator", "config")
	ciopConfigFilename := fmt.Sprintf("%s-%s-%s%s.yaml", opt.org, opt.repo, opt.branch, variant)
	ciopConfigPath := filepath.Join(ciopConfigDir, opt.org, opt.repo, ciopConfigFilename)
	if err := config.OperateOnCIOperatorConfig(ciopConfigPath, func(cfg *api.ReleaseBuildConfiguration, info *config.Info) error {
		var original api.ReleaseBuildConfiguration
		if err := deepcopy.Copy(&original, cfg); err != nil {
			return err
		}
		for _, test := range cfg.Tests {
			if test.As == opt.name {
				return fmt.Errorf("config already has test '%s'", opt.name)
			}
		}
		newTest := api.TestStepConfiguration{
			As: opt.name,
			MultiStageTestConfiguration: &api.MultiStageTestConfiguration{
				ClusterProfile: api.ClusterProfile(opt.clusterProfile),
				Workflow:       utilpointer.StringPtr(opt.workflow),
			},
		}
		cfg.Tests = append(cfg.Tests, newTest)
		output := config.DataWithInfo{Configuration: *cfg, Info: *info}
		if err := output.CommitTo(ciopConfigDir); err != nil {
			return err
		}
		fmt.Printf("CI configuration file %s was changed:%s\n", ciopConfigPath, cmp.Diff(original, *cfg))

		return nil
	}); err != nil {
		logrus.WithError(err).Fatal("Failed to add new test to ci-operator config")
	}
}
