package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/prow/pkg/flagutil"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
	"github.com/openshift/ci-tools/pkg/promotion"
)

type options struct {
	promotion.FutureOptions
	gitDir    string
	username  string
	tokenPath string
	ignore    flagutil.Strings
}

func (o *options) Validate() error {
	if err := o.FutureOptions.Validate(); err != nil {
		return err
	}
	if o.Confirm {
		if o.username == "" {
			return errors.New("--username is required with --confirm")
		}
		if o.tokenPath == "" {
			return errors.New("--token-path is required with --confirm")
		}
	}
	return nil
}

func (o *options) bind(fs *flag.FlagSet) {
	fs.StringVar(&o.gitDir, "git-dir", "", "Optional dir to do git operations in. If unset, temp dir will be used.")
	fs.StringVar(&o.username, "username", "", "Username to use when pushing to GitHub.")
	fs.StringVar(&o.tokenPath, "token-path", "", "Path to token to use when pushing to GitHub.")
	fs.Var(&o.ignore, "ignore", "Ignore a repo or entire org. Format: org or org/repo. Can be passed multiple times.")
	o.FutureOptions.Bind(fs)
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	o.bind(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("could not parse input")
	}
	return o
}

type censoringFormatter struct {
	secret   string
	delegate logrus.Formatter
}

func (f *censoringFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	for key, value := range entry.Data {
		if valueString, ok := value.(string); ok {
			if strings.Contains(valueString, f.secret) {
				entry.Data[key] = strings.Replace(valueString, f.secret, "xxx", -1)
			}
		}
	}
	return f.delegate.Format(entry)
}

type gitCmd func(l *logrus.Entry, args ...string) error

// branchWork groups all configs that share the same org/repo/branch.
type branchWork struct {
	info    config.Info
	configs []*api.ReleaseBuildConfiguration
}

// repoWork groups all branches for a single org/repo.
type repoWork struct {
	org, repo string
	branches  []*branchWork
}

func main() {
	o := gatherOptions()
	if err := o.Validate(); err != nil {
		logrus.Fatalf("Invalid options: %v", err)
	}

	ignoreSet := o.ignore.StringSet()

	gitDir := o.gitDir
	if gitDir == "" {
		tempDir, err := os.MkdirTemp("", "")
		if err != nil {
			logrus.WithError(err).Fatal("Could not create temp dir for git operations")
		}
		defer func() {
			if err := os.RemoveAll(tempDir); err != nil {
				logrus.WithError(err).Fatal("Could not clean up temp dir for git operations")
			}
		}()
		gitDir = tempDir
	}

	var token string
	if o.Confirm {
		if rawToken, err := os.ReadFile(o.tokenPath); err != nil {
			logrus.WithError(err).Fatal("Could not read token.")
		} else {
			token = strings.TrimSpace(string(rawToken))
			logrus.SetFormatter(&censoringFormatter{delegate: new(logrus.TextFormatter), secret: token})
		}
	}

	// Phase 1: Collect and group work items by org/repo and branch.
	repoMap := map[string]*repoWork{}
	branchMap := map[string]map[string]*branchWork{}
	brachingFailure := false

	if err := o.OperateOnCIOperatorConfigDir(o.ConfigDir, api.WithoutOKD, func(configuration *api.ReleaseBuildConfiguration, repoInfo *config.Info) error {
		if ignoreSet.Has(repoInfo.Org) || ignoreSet.Has(fmt.Sprintf("%s/%s", repoInfo.Org, repoInfo.Repo)) {
			logrus.WithField("repo", fmt.Sprintf("%s/%s", repoInfo.Org, repoInfo.Repo)).Info("Skipping due to --ignore")
			return nil
		}

		repoKey := fmt.Sprintf("%s/%s", repoInfo.Org, repoInfo.Repo)
		if _, ok := repoMap[repoKey]; !ok {
			repoMap[repoKey] = &repoWork{org: repoInfo.Org, repo: repoInfo.Repo}
			branchMap[repoKey] = map[string]*branchWork{}
		}

		bm := branchMap[repoKey]
		if _, ok := bm[repoInfo.Branch]; !ok {
			bm[repoInfo.Branch] = &branchWork{info: *repoInfo}
		}
		bm[repoInfo.Branch].configs = append(bm[repoInfo.Branch].configs, configuration)

		return nil
	}); err != nil {
		logrus.WithError(err).Error("Could not branch configurations.")
		brachingFailure = true
	}

	// Assemble final work list.
	var work []*repoWork
	for key, rw := range repoMap {
		for _, bw := range branchMap[key] {
			rw.branches = append(rw.branches, bw)
		}
		work = append(work, rw)
	}

	// Phase 2: Process repos in parallel.
	var (
		mu            sync.Mutex
		failedConfigs = sets.New[string]()
	)
	appendFailedConfigs := func(configs []*api.ReleaseBuildConfiguration) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range configs {
			configInfo := fmt.Sprintf("%s/%s@%s", c.Metadata.Org, c.Metadata.Repo, c.Metadata.Branch)
			if c.Metadata.Variant != "" {
				configInfo += "__" + c.Metadata.Variant
			}
			failedConfigs.Insert(configInfo)
		}
	}

	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup

	for _, rw := range work {
		wg.Add(1)
		go func(rw *repoWork) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			processRepo(rw, gitDir, &o, token, appendFailedConfigs)
		}(rw)
	}

	wg.Wait()

	if len(failedConfigs) > 0 {
		logrus.WithField("configs", failedConfigs.UnsortedList()).Error("Failed configurations.")
		brachingFailure = true
	}

	if brachingFailure {
		os.Exit(1)
	}
}

func processRepo(rw *repoWork, gitDir string, o *options, token string, appendFailedConfigs func([]*api.ReleaseBuildConfiguration)) {
	repoDir := path.Join(gitDir, rw.org, rw.repo)
	repoLogger := logrus.WithFields(logrus.Fields{"org": rw.org, "repo": rw.repo})

	if err := os.MkdirAll(repoDir, 0775); err != nil {
		repoLogger.WithError(err).Error("could not ensure git dir existed")
		for _, bw := range rw.branches {
			appendFailedConfigs(bw.configs)
		}
		return
	}

	git := gitCmdFunc(repoDir)

	remote, err := url.Parse(fmt.Sprintf("https://github.com/%s/%s", rw.org, rw.repo))
	if err != nil {
		repoLogger.WithError(err).Error("Could not construct remote URL.")
		for _, bw := range rw.branches {
			appendFailedConfigs(bw.configs)
		}
		return
	}
	if o.Confirm {
		remote.User = url.UserPassword(o.username, token)
	}

	if err := git(repoLogger, "init"); err != nil {
		for _, bw := range rw.branches {
			appendFailedConfigs(bw.configs)
		}
		return
	}

	for _, bw := range rw.branches {
		logger := config.LoggerForInfo(bw.info)

		if err := git(logger, "fetch", "--depth", "1", remote.String(), bw.info.Branch); err != nil {
			appendFailedConfigs(bw.configs)
			continue
		}

		for _, futureRelease := range o.FutureReleases.Strings() {
			futureBranch, err := promotion.DetermineReleaseBranch(o.CurrentRelease, futureRelease, bw.info.Branch)
			if err != nil {
				logger.WithError(err).Error("could not determine release branch")
				appendFailedConfigs(bw.configs)
				continue
			}
			if futureBranch == bw.info.Branch {
				continue
			}

			futureLogger := logger.WithField("future-branch", futureBranch)

			if !o.Confirm {
				futureLogger.Info("Would create new branch.")
				continue
			}

			for depth := 1; depth < 9; depth += 1 {
				retry, err := pushBranch(futureLogger, remote, futureBranch, git)
				if err != nil {
					futureLogger.WithError(err).Error("Failed to push branch")
					appendFailedConfigs(bw.configs)
					break
				}

				if !retry {
					break
				}

				if depth == 8 && retry {
					futureLogger.Error("Could not push branch even with retries.")
					appendFailedConfigs(bw.configs)
					break
				}

				if err := fetchDeeper(futureLogger, remote, git, &bw.info, int(math.Exp2(float64(depth)))); err != nil {
					appendFailedConfigs(bw.configs)
					break
				}
			}
		}
	}
}

func pushBranch(logger *logrus.Entry, remote *url.URL, futureBranch string, gitCmd gitCmd) (bool, error) {
	command := []string{"push", remote.String(), fmt.Sprintf("FETCH_HEAD:refs/heads/%s", futureBranch)}
	logger = logger.WithFields(logrus.Fields{"commands": fmt.Sprintf("git %s", strings.Join(command, " "))})
	if err := gitCmd(logger, command...); err != nil {
		tooShallowErr := strings.Contains(err.Error(), "Updates were rejected because the remote contains work that you do")
		if tooShallowErr {
			logger.Warn("Failed to push, trying a deeper clone...")
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func fetchDeeper(logger *logrus.Entry, remote *url.URL, gitCmd gitCmd, repoInfo *config.Info, depth int) error {
	command := []string{"fetch", "--deepen", strconv.Itoa(depth), remote.String(), repoInfo.Branch}
	if err := gitCmd(logger, command...); err != nil {
		return err
	}
	return nil
}

// isTransientError checks if a git command error is transient and worth retrying.
func isTransientError(output string) bool {
	transientIndicators := []string{
		"Connection timed out",
		"Connection reset",
		"Connection refused",
		"Could not resolve host",
		"The requested URL returned error: 5",
		"error: RPC failed",
		"SSL_ERROR",
		"unexpected disconnect",
		"early EOF",
	}
	for _, indicator := range transientIndicators {
		if strings.Contains(output, indicator) {
			return true
		}
	}
	return false
}

func gitCmdFunc(dir string) gitCmd {
	return func(l *logrus.Entry, args ...string) error {
		l = l.WithField("commands", fmt.Sprintf("git %s", strings.Join(args, " ")))
		var b []byte
		var err error
		l.Debug("Running command.")
		sleepyTime := time.Second
		for i := 0; i < 3; i++ {
			c := exec.Command("git", args...)
			c.Dir = dir
			b, err = c.CombinedOutput()
			if err != nil {
				output := string(b)
				err = fmt.Errorf("running git %v returned error %w with output %q", args, err, output)
				if !isTransientError(output) {
					break
				}
				l.WithError(err).Debugf("Retrying #%d, if this is not the 3rd try then this will be retried", i+1)
				time.Sleep(sleepyTime)
				sleepyTime *= 2
				continue
			}
			break
		}
		l = l.WithField("output", string(b))
		if err != nil {
			l.Error("Failed to execute command.")
			return err
		}

		l.Debug("Executed command.")
		return nil
	}
}
