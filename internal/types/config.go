package types

// DockerConfig represents the configuration for the Docker feature
type DockerConfig struct {
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
