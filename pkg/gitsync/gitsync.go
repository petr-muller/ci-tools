package gitsync

import (
	"flag"
	"fmt"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

type Options struct {
	Username  string
	TokenPath string

	gitDir string
}

func (o *Options) Bind(fs *flag.FlagSet) {
	fs.StringVar(&o.gitDir, "git-dir", "", "Optional dir to do git operations in. If unset, temp dir will be used.")
	fs.StringVar(&o.Username, "username", "", "Username to use when pushing to GitHub.")
	fs.StringVar(&o.TokenPath, "token-path", "", "Path to token to use when pushing to GitHub.")
}

type Source interface {
	To(org, repo, branch string) error
}

type source struct {
	org    string
	repo   string
	branch string

	gitDir  string
	user    *url.Userinfo
	confirm bool

	logger *logrus.Entry
}

func getRemote(org, repo string, user *url.Userinfo) (*url.URL, error) {
	remote, err := url.Parse(fmt.Sprintf("https://github.com/%s/%s", org, repo))
	if err != nil {
		return nil, fmt.Errorf("could not construct remote URL: %v", err)
	}
	remote.User = user

	return remote, nil
}

func (s source) push(remote *url.URL, branch string, logger *logrus.Entry) (bool, error) {
	command := []string{"push", remote.String(), fmt.Sprintf("FETCH_HEAD:refs/heads/%s", branch)}
	cmdLogger := logger.WithFields(logrus.Fields{"commands": fmt.Sprintf("git %s", strings.Join(command, " "))})
	cmd := exec.Command("git", command...)
	cmd.Dir = s.gitDir
	cmdLogger.Debug("Running command.")
	if out, err := cmd.CombinedOutput(); err != nil {
		errLogger := cmdLogger.WithError(err).WithFields(logrus.Fields{"output": string(out)})
		tooShallowErr := strings.Contains(string(out), "Updates were rejected because the remote contains work that you do")
		if tooShallowErr {
			errLogger.Warn("Failed to push, trying a deeper clone...")
			return true, nil
		}
		errLogger.Error("Failed to execute command.")
		return false, fmt.Errorf("failed to execute command")
	} else {
		cmdLogger.WithFields(logrus.Fields{"output": string(out)}).Debug("Executed command.")
		logger.Info("Pushed new branch.")
		return false, nil
	}
}

func (s source) fetch(depth int, logger *logrus.Entry) error {
	if err := os.MkdirAll(s.gitDir, 0775); err != nil {
		return fmt.Errorf("could not ensure git dir existed: %v", err)
	}

	remote, err := getRemote(s.org, s.repo, s.user)
	if err != nil {
		return err
	}

	for _, command := range [][]string{{"init"}, {"fetch", "--depth", strconv.Itoa(depth), remote.String(), s.branch}} {
		cmdLogger := logger.WithFields(logrus.Fields{"commands": fmt.Sprintf("git %s", strings.Join(command, " "))})
		cmd := exec.Command("git", command...)
		cmd.Dir = s.gitDir
		cmdLogger.Debug("Running command.")
		if out, err := cmd.CombinedOutput(); err != nil {
			cmdLogger.WithError(err).WithFields(logrus.Fields{"output": string(out)}).Error("Failed to execute command.")
			return fmt.Errorf("failed to fetch revision to local clone")
		} else {
			cmdLogger.WithFields(logrus.Fields{"output": string(out)}).Debug("Executed command.")
		}
	}

	return nil
}

func (s source) To(org, repo, branch string) error {
	logger := s.logger.WithFields(logrus.Fields{
		"to-org":    org,
		"to-repo":   repo,
		"to-branch": branch,
	})
	if err := s.fetch(1, logger); err != nil {
		return err
	}

	if !s.confirm {
		logger.Info("Would push to remote branch.")
		return nil
	}

	targetRemote, err := getRemote(org, repo, s.user)
	if err != nil {
		return err
	}

	for depth := 1; depth < 9; depth += 1 {
		retry, err := s.push(targetRemote, branch, logger)
		if err != nil {
			return nil
		}
		if !retry {
			break
		}

		if depth == 8 && retry {
			logger.Error("Could not push branch even with retries")
			return fmt.Errorf("could not push branch even with retries")
		}

		if err := s.fetch(int(math.Exp2(float64(depth))), logger); err != nil {
			break
		}
	}
	return nil
}

type Sync interface {
	Clean() error
	From(org, repo, branch string) Source
}

type sync struct {
	gitDir   string
	username string
	password string
	clean    bool
	confirm  bool
}

func (s sync) Clean() error {
	if s.clean {
		return os.RemoveAll(s.gitDir)
	}
	return nil
}

func (s sync) From(org, repo, branch string) Source {
	src := source{
		org:     org,
		repo:    repo,
		branch:  branch,
		gitDir:  path.Join(s.gitDir, org, repo),
		confirm: s.confirm,

		logger: logrus.WithFields(logrus.Fields{
			"from-org":    org,
			"from-repo":   repo,
			"from-branch": branch,
		}),
	}

	if s.username != "" {
		src.user = url.UserPassword(s.username, s.password)
	}

	return src
}

func (o Options) NewSync(confirm bool, username, password string) (Sync, error) {
	var err error
	s := sync{
		gitDir:   o.gitDir,
		username: username,
		password: password,
		confirm:  confirm,
	}
	if s.gitDir == "" {
		s.gitDir, err = ioutil.TempDir("", "")
		s.clean = true
	}

	return s, err
}
