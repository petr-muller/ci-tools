package prowexec

type Prow interface {
	Do() (bool, error)
}

