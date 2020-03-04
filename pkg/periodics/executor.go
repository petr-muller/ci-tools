package periodics

import (
	pj "k8s.io/test-infra/prow/client/clientset/versioned/typed/prowjobs/v1"
	prowconfig "k8s.io/test-infra/prow/config"

	"github.com/openshift/ci-tools/pkg/prowexec"
)

type executor struct {
	jobs   []prowconfig.Periodic
	dry    bool
	client pj.ProwJobInterface
}

func (e *executor) Do() (bool, error) {
	return false, nil
}

func NewExecutor(jobs []prowconfig.Periodic, dry bool, pjclient pj.ProwJobInterface) prowexec.Prow {
	return &executor{
		jobs:   jobs,
		dry:    dry,
		client: pjclient,
	}
}
