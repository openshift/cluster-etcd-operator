package etcdenvvar

type FakeEnvVar struct {
	EnvVars   map[string]string
	Listeners []Enqueueable
}

func (f FakeEnvVar) AddListener(listener Enqueueable) {
	f.Listeners = append(f.Listeners, listener)
}

func (f FakeEnvVar) GetEnvVars() map[string]string {
	return f.EnvVars
}
