package collections

import "fmt"

// collection represents a resolved collection with metadata.
type collection struct {
	Namespace  string   `yaml:"namespace"`
	Name       string   `yaml:"name"`
	Version    string   `yaml:"version"`
	Source     string   `yaml:"source"`
	Signatures []string `yaml:"signatures"`
	Constraint string   `yaml:"-"`
	Type       string   `yaml:"-"`
}

// key returns the unique key for the collection.
func (c collection) key() string {
	return fmt.Sprintf("%s.%s@%s", c.Namespace, c.Name, c.Version)
}
