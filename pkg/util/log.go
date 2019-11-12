package util

import (
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
)

type SecretGetter struct {
	sync.RWMutex
	secrets sets.String
}

func NewSecretGetter() *SecretGetter {
	return &SecretGetter{
		secrets: sets.NewString(),
	}
}

func (g *SecretGetter) AddSecrets(newSecrets ...string) {
	g.Lock()
	defer g.Unlock()
	g.secrets.Insert(newSecrets...)
}

func (g *SecretGetter) GetSecrets() sets.String {
	g.RLock()
	defer g.RUnlock()
	return g.secrets
}
