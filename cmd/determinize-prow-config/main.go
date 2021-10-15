// determinize-prow-config reads and writes Prow configuration
// to enforce formatting on the files
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"k8s.io/apimachinery/pkg/util/sets"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	prowconfig "k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/plugins"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/config"
	"github.com/openshift/ci-tools/pkg/prowconfigsharding"
)

type options struct {
	prowConfigDir              string
	shardedProwConfigBaseDir   string
	shardedPluginConfigBaseDir string
}

func (o *options) Validate() error {
	if o.prowConfigDir == "" {
		return errors.New("--prow-config-dir is required")
	}
	return nil
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&o.prowConfigDir, "prow-config-dir", "", "Path to the Prow configuration directory.")
	fs.StringVar(&o.shardedProwConfigBaseDir, "sharded-prow-config-base-dir", "", "Basedir for the sharded prow config. If set, org and repo-specific config will get removed from the main prow config and written out in an org/repo tree below the base dir.")
	fs.StringVar(&o.shardedPluginConfigBaseDir, "sharded-plugin-config-base-dir", "", "Basedir for the sharded plugin config. If set, the plugin config will get sharded")
	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("could not parse input")
	}
	return o
}

func main() {
	o := gatherOptions()
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("invalid options")
	}

	if err := updateProwConfig(o.prowConfigDir, o.shardedProwConfigBaseDir); err != nil {
		logrus.WithError(err).Fatal("could not update Prow configuration")
	}

	if err := updatePluginConfig(o.prowConfigDir, o.shardedPluginConfigBaseDir); err != nil {
		logrus.WithError(err).Fatal("could not update Prow plugin configuration")
	}
}

func updateProwConfig(configDir, shardingBaseDir string) error {
	configPath := path.Join(configDir, config.ProwConfigFile)
	agent := prowconfig.Agent{}
	var additionalConfigs []string
	if shardingBaseDir != "" {
		additionalConfigs = append(additionalConfigs, shardingBaseDir)
	}
	if err := agent.Start(configPath, "", additionalConfigs, "_prowconfig.yaml"); err != nil {
		return fmt.Errorf("could not load Prow configuration: %w", err)
	}

	config := agent.Config()

	if shardingBaseDir != "" {
		pc, err := shardProwConfig(&config.ProwConfig, afero.NewBasePathFs(afero.NewOsFs(), shardingBaseDir))
		if err != nil {
			return fmt.Errorf("failed to shard the prow config: %w", err)
		}
		config.ProwConfig = *pc
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("could not marshal Prow configuration: %w", err)
	}

	return ioutil.WriteFile(configPath, data, 0644)
}

func updatePluginConfig(configDir, shardingBaseDir string) error {
	configPath := path.Join(configDir, config.PluginConfigFile)
	agent := plugins.ConfigAgent{}
	if err := agent.Load(configPath, []string{filepath.Dir(configPath)}, "_pluginconfig.yaml", false); err != nil {
		return fmt.Errorf("could not load Prow plugin configuration: %w", err)
	}
	cfg := agent.Config()
	if shardingBaseDir != "" {
		pc, err := prowconfigsharding.WriteShardedPluginConfig(cfg, afero.NewBasePathFs(afero.NewOsFs(), shardingBaseDir))
		if err != nil {
			return fmt.Errorf("failed to shard plugin config: %w", err)
		}
		cfg = pc
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("could not marshal Prow plugin configuration: %w", err)
	}

	return ioutil.WriteFile(configPath, data, 0644)
}

// prowConfigWithPointers mimics the upstream prowConfig but has pointer fields only
// in order to avoid serializing empty structs.
type prowConfigWithPointers struct {
	BranchProtection *prowconfig.BranchProtection `json:"branch-protection,omitempty"`
	Tide             *tideConfig                  `json:"tide,omitempty"`
}

type tideConfig struct {
	MergeType map[string]github.PullRequestMergeType `json:"merge_method,omitempty"`
	Queries   prowconfig.TideQueries                 `json:"queries,omitempty"`
}

func shardProwConfig(pc *prowconfig.ProwConfig, target afero.Fs) (*prowconfig.ProwConfig, error) {
	configsByOrgRepo := map[prowconfig.OrgRepo]*prowConfigWithPointers{}
	for org, orgConfig := range pc.BranchProtection.Orgs {
		for repo, repoConfig := range orgConfig.Repos {
			if configsByOrgRepo[prowconfig.OrgRepo{Org: org, Repo: repo}] == nil {
				configsByOrgRepo[prowconfig.OrgRepo{Org: org, Repo: repo}] = &prowConfigWithPointers{}
			}
			configsByOrgRepo[prowconfig.OrgRepo{Org: org, Repo: repo}].BranchProtection = &prowconfig.BranchProtection{
				Orgs: map[string]prowconfig.Org{org: {Repos: map[string]prowconfig.Repo{repo: repoConfig}}},
			}
			delete(pc.BranchProtection.Orgs[org].Repos, repo)
		}

		if isPolicySet(orgConfig.Policy) {
			if configsByOrgRepo[prowconfig.OrgRepo{Org: org}] == nil {
				configsByOrgRepo[prowconfig.OrgRepo{Org: org}] = &prowConfigWithPointers{}
			}
			configsByOrgRepo[prowconfig.OrgRepo{Org: org}].BranchProtection = &prowconfig.BranchProtection{
				Orgs: map[string]prowconfig.Org{org: orgConfig},
			}
		}
		delete(pc.BranchProtection.Orgs, org)
	}

	for orgOrgRepoString, mergeMethod := range pc.Tide.MergeType {
		var orgRepo prowconfig.OrgRepo
		if idx := strings.Index(orgOrgRepoString, "/"); idx != -1 {
			orgRepo.Org = orgOrgRepoString[:idx]
			orgRepo.Repo = orgOrgRepoString[idx+1:]
		} else {
			orgRepo.Org = orgOrgRepoString
		}

		if configsByOrgRepo[orgRepo] == nil {
			configsByOrgRepo[orgRepo] = &prowConfigWithPointers{}
		}
		configsByOrgRepo[orgRepo].Tide = &tideConfig{MergeType: map[string]github.PullRequestMergeType{orgOrgRepoString: mergeMethod}}
		delete(pc.Tide.MergeType, orgOrgRepoString)
	}

	for _, query := range pc.Tide.Queries {
		for _, org := range query.Orgs {
			if configsByOrgRepo[prowconfig.OrgRepo{Org: org}] == nil {
				configsByOrgRepo[prowconfig.OrgRepo{Org: org}] = &prowConfigWithPointers{}
			}
			if configsByOrgRepo[prowconfig.OrgRepo{Org: org}].Tide == nil {
				configsByOrgRepo[prowconfig.OrgRepo{Org: org}].Tide = &tideConfig{}
			}
			queryCopy, err := deepCopyTideQuery(&query)
			if err != nil {
				return nil, fmt.Errorf("failed to deepcopy tide query %+v: %w", query, err)
			}
			queryCopy.Orgs = []string{org}
			queryCopy.Repos = nil
			configsByOrgRepo[prowconfig.OrgRepo{Org: org}].Tide.Queries = append(configsByOrgRepo[prowconfig.OrgRepo{Org: org}].Tide.Queries, *queryCopy)
		}
		for _, repo := range query.Repos {
			slashSplit := strings.Split(repo, "/")
			if len(slashSplit) != 2 {
				return nil, fmt.Errorf("repo '%s' in query %+v is not a valid repo specification", repo, query)
			}
			orgRepo := prowconfig.OrgRepo{Org: slashSplit[0], Repo: slashSplit[1]}
			if configsByOrgRepo[orgRepo] == nil {
				configsByOrgRepo[orgRepo] = &prowConfigWithPointers{}
			}
			if configsByOrgRepo[orgRepo].Tide == nil {
				configsByOrgRepo[orgRepo].Tide = &tideConfig{}
			}
			queryCopy, err := deepCopyTideQuery(&query)
			if err != nil {
				return nil, fmt.Errorf("failed to deepcopy tide query %+v: %w", query, err)
			}
			queryCopy.Orgs = nil
			queryCopy.Repos = []string{repo}
			ensureStaffEngFor410(queryCopy)
			ensureCherryPickFor49(queryCopy)
			ensureExcluded49And410(queryCopy)
			configsByOrgRepo[orgRepo].Tide.Queries = append(configsByOrgRepo[orgRepo].Tide.Queries, *queryCopy)
		}
	}
	pc.Tide.Queries = nil

	for orgOrRepo, cfg := range configsByOrgRepo {
		if err := prowconfigsharding.MkdirAndWrite(target, filepath.Join(orgOrRepo.Org, orgOrRepo.Repo, config.SupplementalProwConfigFileName), cfg); err != nil {
			return nil, err
		}
	}

	return pc, nil
}

var r49 = sets.NewString("release-4.9")
var o49 = sets.NewString("openshift-4.9")
var or49 = r49.Union(o49)
var r410 = sets.NewString("release-4.10")
var o410 = sets.NewString("openshift-4.10")
var or410 = r410.Union(o410)

var weirdExcludedAllowlist = sets.NewString(
	// Assisted
	"openshift/assisted-installer",
	"openshift/assisted-installer-agent",
	"openshift/assisted-test-infra",
	"openshift/assisted-image-service",
	"openshift/assisted-service",
	// Weird but consistent, no OCP criteria
	"openshift/windows-machine-config-bootstrapper",
	"openshift/windows-machine-config-operator",
	"openshift-priv/windows-machine-config-bootstrapper",
	"openshift-priv/windows-machine-config-operator",
	"red-hat-storage/ceph-csi",
).Union(weirdCPNo49Allowlist)

func ensureExcluded49And410(q *prowconfig.TideQuery) {
	branches := sets.NewString(q.ExcludedBranches...)
	if branches.Has("release-4.8") {
		branches.Insert("release-4.9")
		branches.Insert("release-4.10")
	}
	if branches.Has("openshift-4.8") {
		branches.Insert("openshift-4.9")
		branches.Insert("openshift-4.10")
	}
	if branches.Len() > 0 {
		if branches.Intersection(or410).Len() == 0 && !weirdExcludedAllowlist.Has(q.Repos[0]) {
			fmt.Printf("Weird complement query (without 4.10): %s\n", q.Repos)
		}
	}
	q.ExcludedBranches = branches.List()
}

var weirdCPNo49Allowlist = sets.NewString(
	// Logging team
	"openshift/cluster-logging-operator",
	"openshift/elasticsearch-operator",
	"openshift/elasticsearch-proxy",
	"openshift/origin-aggregated-logging",
	// Does not seem to be even branched, likely does not need this config
	"openshift/app-netutil",
	"openshift/network-tools",
	"openshift-priv/app-netutil",
	"openshift-priv/network-tools",
)

func ensureCherryPickFor49(q *prowconfig.TideQuery) {
	reqLabels := sets.NewString(q.Labels...)
	branches := sets.NewString(q.IncludedBranches...)
	if reqLabels.Has("cherry-pick-approved") {
		if branches.Has("release-4.8") {
			branches.Insert("release-4.9")
		}
		if branches.Has("openshift-4.8") {
			branches.Insert("openshift-4.9")
		}
		if branches.Intersection(or49).Len() == 0 && !weirdCPNo49Allowlist.Has(q.Repos[0]) {
			fmt.Printf("Weird cherry-pick-approved query (without 4.9): %s\n", q.Repos)
		}
		if branches.Intersection(or410).Len() != 0 {
			fmt.Printf("Weird cherry-pick-approved query (with 4.10): %s\n", q.Repos)
		}
	}
	q.IncludedBranches = branches.List()
}

func ensureStaffEngFor410(q *prowconfig.TideQuery) {
	reqLabels := sets.NewString(q.Labels...)
	branches := sets.NewString(q.IncludedBranches...)

	if reqLabels.Has("staff-eng-approved") {
		if branches.Has("release-4.9") {
			branches.Delete("release-4.9")
			branches.Insert("release-4.10")
		}
		if branches.Has("openshift-4.9") {
			branches.Delete("openshift-4.9")
			branches.Insert("openshift-4.10")
		}

		if !(branches.Equal(r410) || branches.Equal(o410) || branches.Equal(or410)) {
			fmt.Printf("Weird staff-eng-approved query: %s\n", q.Repos)
		}
	}
	q.IncludedBranches = branches.List()

}

func deepCopyTideQuery(q *prowconfig.TideQuery) (*prowconfig.TideQuery, error) {
	serialized, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("failed to marhsal: %w", err)
	}
	var result prowconfig.TideQuery
	if err := json.Unmarshal(serialized, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal: %w", err)
	}

	return &result, nil
}

func isPolicySet(p prowconfig.Policy) bool {
	return !apiequality.Semantic.DeepEqual(p, prowconfig.Policy{})
}
