// Package plan defines the resolved description of one sandboxed command. It
// carries references to secrets, never their values.
package plan

import "encoding/json"

// Plan is the output of resolve and the input of engines.
type Plan struct {
	Profile   string     `json:"profile"`
	Spec      string     `json:"spec"`
	SpecChain []string   `json:"spec_chain"`
	Engine    string     `json:"engine"`
	Phase     string     `json:"phase"`
	Workspace string     `json:"workspace"`
	Workdir   string     `json:"workdir"`
	Image     ImagePlan  `json:"image"`
	Mounts    []Mount    `json:"mounts"`
	Env       []EnvVar   `json:"env"`
	Network   Network    `json:"network"`
	Limits    Limits     `json:"limits"`
	Claude    ClaudePlan `json:"claude"`
	Setup     *SetupPlan `json:"setup,omitempty"`
	Argv      []string   `json:"argv"`
}

// ImagePlan lists spec image layers, root first. The base image is implicit
// layer 0, built from the embedded Dockerfile.
type ImagePlan struct {
	Layers []ImageLayer `json:"layers"`
}

// ImageLayer holds absolute paths.
type ImageLayer struct {
	Spec       string `json:"spec"`
	Dockerfile string `json:"dockerfile"`
	Context    string `json:"context"`
}

// Mount is a bind mount. Guest equals Host for same-path mounts.
type Mount struct {
	Host     string `json:"host"`
	Guest    string `json:"guest"`
	ReadOnly bool   `json:"read_only"`
}

// Environment value sources.
const (
	SourceLiteral = "literal"
	SourceFile    = "file"
	SourceEnv     = "env"
)

// EnvVar names one environment variable. Value is set only for literals; Ref
// is the file path or variable name for file and env sources.
type EnvVar struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Ref    string `json:"ref,omitempty"`
	Value  string `json:"value,omitempty"`
}

// Network is the resolved egress policy. Hosts is deduplicated and sorted.
type Network struct {
	Policy string   `json:"policy"`
	Sets   []string `json:"sets"`
	Hosts  []string `json:"hosts"`
}

// Limits are VM resource limits; zero values mean the engine default.
type Limits struct {
	CPUs   int    `json:"cpus"`
	Memory string `json:"memory"`
}

// ClaudePlan configures the paddock-local ~/.claude in the guest.
type ClaudePlan struct {
	ManagedSettings json.RawMessage `json:"managed_settings,omitempty"`
	SeedFiles       []string        `json:"seed_files"`
	Skills          string          `json:"skills,omitempty"`
	Agents          string          `json:"agents,omitempty"`
}

// SetupPlan describes the setup command and its dependency cache.
type SetupPlan struct {
	Command      []string `json:"command"`
	CacheKeyFile string   `json:"cache_key_file,omitempty"`
	CacheDir     string   `json:"cache_dir,omitempty"`
	// CacheHost is the host directory mounted read-write at CacheDir; set
	// only in the setup phase.
	CacheHost string `json:"cache_host,omitempty"`
}
