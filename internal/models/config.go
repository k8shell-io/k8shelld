package models

// PodmanConfig represents the configuration for the Podman feature
type PodmanConfig struct {
	Enabled                 bool      `yaml:"enabled"`
	CreateDockerSockSymlink bool      `yaml:"createDockerSockSymlink"`
	Storages                []Storage `yaml:"storages"`
}

type Storage struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path"`
	Size     string `yaml:"size"`
	Readonly bool   `yaml:"readonly"`
}
